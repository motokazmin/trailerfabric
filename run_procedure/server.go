package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"
	"bufio"
	"strconv"
	"fmt"

	"github.com/machinebox/sdk-go/facebox"
	"github.com/machinebox/sdk-go/tagbox"
	"github.com/matryer/way"
)

// Server is the app server.
type Server struct {
	assets  string
	videos  string
	items   *Items // here the video items on the filesystem
	facebox *facebox.Client
	tagbox *tagbox.Client
	router  *way.Router
}

// NewServer makes a new Server.
func NewServer(assets string, videos string, facebox *facebox.Client, tagbox *tagbox.Client) *Server {
	srv := &Server{
		assets:  assets,
		videos:  videos,
		items:   LoadItemsFromPath(videos),
		facebox: facebox,
		tagbox: tagbox,
		router:  way.NewRouter(),
	}

	srv.router.Handle(http.MethodGet, "/assets/", Static("/assets/", assets))
	srv.router.Handle(http.MethodGet, "/videos/", Static("/videos/", videos))

//	srv.router.HandleFunc(http.MethodGet, "/stream", srv.stream)
	srv.router.HandleFunc(http.MethodGet, "/check", srv.check)
	srv.router.HandleFunc(http.MethodGet, "/all-videos/", srv.handleListVideos)
	srv.router.HandleFunc(http.MethodGet, "/", srv.handleIndex)
	return srv
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(s.assets, "index.html"))
}

func (s *Server) handleListVideos(w http.ResponseWriter, r *http.Request) {
	var res struct {
		Items []Item `json:"items"`
	}
	res.Items = s.items.List()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		log.Printf("[ERROR] encondig response %v\n", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

type Frame struct {
	Frame  int    `json:"frame"`
	Total  int    `json:"total"`
	Millis int    `json:"millis"`
	Image  string `json:"image"`
}

type VideoData struct {
	Frame       int            `json:"frame,omitempty"`
	TotalFrames int            `json:"total_frames,omitempty"`
	Seconds     string         `json:"seconds,omitempty"`
	Complete    bool           `json:"complete,omitempty"`
	Faces       []facebox.Face `json:"faces,omitempty"`
	Thumbnail   *string        `json:"thumbnail,omitempty"`
}

type TagData struct {
    Name  string
    Thumbnail   *string        `json:"thumbnail,omitempty"`
}

func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	var thumbnail *string
	// sent the headers for Server Side Events
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	enc := json.NewEncoder(w)

	f, _ := os.Create("film.csv")
	wr := bufio.NewWriter(f)

	log.Printf("check...\n")
	// starts the video processing script
	filename := r.URL.Query().Get("name")
	flags := []string{"./video.py", "--path", path.Join(s.videos, filename), "--json", "True"}
	cmd := exec.CommandContext(r.Context(), "python", flags...)
	stdout, _ := cmd.StdoutPipe()

	cmd.Start()

	total := 0
	dec := json.NewDecoder(stdout)

	for {
		var f Frame
		err := dec.Decode(&f)
		if err == io.EOF {
			log.Println("krv [DEBUG] EOF", err)
			break
		}
		if err != nil {
			log.Println("krv1 [ERROR]", err)
			break
		}

		imgDec, err := base64.StdEncoding.DecodeString(f.Image)
		if err != nil {
			log.Printf("[ERROR] Error decoding the image %v\n", err)
			http.Error(w, "can not decode the image", http.StatusInternalServerError)
			return
		}
		faces, err := s.facebox.Check(bytes.NewReader(imgDec))
		total = f.Total

		thumbnail = nil
		for _, face := range faces {
			thumbnail = &f.Image
			wr.WriteString("face ")
			wr.WriteString(face.ID)
			wr.WriteString(",")
			wr.WriteString((time.Duration(f.Millis/1000) * time.Second).String())
			wr.WriteString(",")
			wr.WriteString(face.Name)
			wr.WriteString(",")
			wr.WriteString(strconv.FormatBool(face.Matched))
			wr.WriteString(",")
			wr.WriteString(fmt.Sprintf("%f", face.Confidence))
			wr.WriteString(",")
			wr.WriteString(face.Faceprint)
			wr.WriteString("\n")
		}
		SendEvent(w, enc, VideoData{
			Frame:       f.Frame,
			TotalFrames: f.Total,
			Seconds:     (time.Duration(f.Millis/1000) * time.Second).String(),
			Complete:    false,
			Faces:       faces,
			Thumbnail:   thumbnail,
		})

		tags, _ := s.tagbox.Check(bytes.NewReader(imgDec))
		for _, tag := range tags.Tags {
			if tag.Tag != "Presentation" && tag.Tag !=  "Line" && tag.Tag !=  "Logo" && tag.Tag != "Screenshot" &&  tag.Tag != "Darkness" && tag.Tag != "Brand" && tag.Tag != "Light" && tag.Tag != "Night" && tag.Tag != "Font" && tag.Tag != "Poster" && tag.Tag != "Text" && tag.Tag != "Lighting" {
			  wr.WriteString("tag ")
			  wr.WriteString(tag.ID)
			  wr.WriteString(",")
			  wr.WriteString((time.Duration(f.Millis/1000) * time.Second).String())
			  wr.WriteString(",")
			  wr.WriteString(tag.Tag)
			  wr.WriteString(",")
			  wr.WriteString(fmt.Sprintf("%f", tag.Confidence))
			  wr.WriteString("\n")
			}
		}

	}

	cmd.Wait()
	SendEvent(w, enc, VideoData{
		Frame:       total,
		TotalFrames: total,
		Complete:    true,
	})
	wr.Flush()
}

func SendEvent(w http.ResponseWriter, enc *json.Encoder, v interface{}) {
	w.Write([]byte("data: "))
	if err := enc.Encode(v); err != nil {
		log.Printf("[ERROR] Error encoding json %v\n", err)
		http.Error(w, "can not encode the json stream", http.StatusInternalServerError)
		return
	}
	w.Write([]byte("\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// Static gets a static file server for the specified path.
func Static(stripPrefix, dir string) http.Handler {
	h := http.StripPrefix(stripPrefix, http.FileServer(http.Dir(dir)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
	})
}
