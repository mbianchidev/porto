package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/containerlogs"
	"github.com/mbianchidev/porto/internal/datafiles"
	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func (s *Server) dataRoutes(mux *http.ServeMux) {
	s.storageRoutes(mux)
	s.nativeFileRoutes(mux)
	for _, resource := range []struct{ plural, kind string }{
		{"containers", "container"}, {"images", "image"}, {"volumes", "volume"},
	} {
		prefix, kind := "/api/docker/"+resource.plural+"/{id}", resource.kind
		mux.HandleFunc("GET "+prefix+"/files", s.requireRuntime("docker", s.dockerFiles(kind, "list")))
		mux.HandleFunc("GET "+prefix+"/file", s.requireRuntime("docker", s.dockerFiles(kind, "read")))
		mux.HandleFunc("GET "+prefix+"/file/download", s.requireRuntime("docker", s.dockerFiles(kind, "download")))
		mux.HandleFunc("PUT "+prefix+"/file", s.requireRuntime("docker", s.dockerFiles(kind, "write")))
		mux.HandleFunc("DELETE "+prefix+"/file", s.requireRuntime("docker", s.dockerFiles(kind, "delete")))
	}
	mux.HandleFunc("GET /api/docker/containers/{id}/logs/stream", s.requireRuntime("docker", s.dockerLogStream))
	mux.HandleFunc("GET /api/docker/containers/{id}/stats", s.requireRuntime("docker", s.dockerInspectorStats))
}

func (s *Server) dockerFiles(kind, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		descriptor, err := s.docker.FileDescriptor(r.Context(), kind, r.PathValue("id"))
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		request := datafiles.Request{
			Action: action, Path: r.URL.Query().Get("path"), Identity: r.URL.Query().Get("identity"),
			SHA256: r.URL.Query().Get("sha256"), Confirm: queryBool(r, "confirm"),
		}
		if request.Identity == "" && action == "list" {
			request.Identity = descriptor.Resource.Fingerprint()
		}
		if request.Identity != descriptor.Resource.Fingerprint() {
			writeRuntimeError(w, datafiles.ErrConflict)
			return
		}
		var input io.Reader
		if action == "write" {
			r.Body = http.MaxBytesReader(w, r.Body, datafiles.MaxUploadBytes)
			input = r.Body
		}
		if action == "download" {
			directory, err := config.RuntimeDir()
			if err != nil {
				writeRuntimeError(w, err)
				return
			}
			file, err := os.CreateTemp(directory, "porto-file-download-")
			if err != nil {
				writeRuntimeError(w, err)
				return
			}
			defer func() {
				if err := errors.Join(file.Close(), os.Remove(file.Name())); err != nil {
					log.Printf("cleanup temporary file download: %v", err)
				}
			}()
			if err := s.docker.RunFileRequest(r.Context(), descriptor, request, nil, file); err != nil {
				writeRuntimeError(w, err)
				return
			}
			info, err := file.Stat()
			if err != nil {
				writeRuntimeError(w, err)
				return
			}
			if info.Size() > datafiles.MaxUploadBytes {
				writeRuntimeError(w, datafiles.ErrLimit)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", "attachment")
			http.ServeContent(w, r, path.Base(request.Path), info.ModTime(), file)
			return
		}
		var result bytes.Buffer
		err = s.docker.RunFileRequest(r.Context(), descriptor, request, input, &result)
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		if result.Len() > 2*1024*1024 {
			writeRuntimeError(w, datafiles.ErrLimit)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(result.Bytes())
	}
}

func (s *Server) dockerInspectorStats(w http.ResponseWriter, r *http.Request) {
	result, err := s.docker.InspectorStats(r.Context(), r.PathValue("id"))
	writeRuntimeResult(w, result, err)
}

func (s *Server) dockerLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRuntimeError(w, errors.New("log streaming is unavailable"))
		return
	}
	tail := 500
	if value := r.URL.Query().Get("tail"); value != "" {
		var err error
		tail, err = strconv.Atoi(value)
		if err != nil || tail < 0 || tail > 5000 {
			http.Error(w, "log tail must be between 0 and 5000", http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	started := false
	send := func(event string, value any) error {
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		controller := http.NewResponseController(w)
		if err := controller.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		started = true
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	err := s.docker.StreamInspectorLogs(r.Context(), r.PathValue("id"), tail, true,
		func(capabilities portodocker.LogCapabilities) error { return send("capabilities", capabilities) },
		func(record containerlogs.Record) error { return send("log", record) })
	if err != nil && !errors.Is(err, context.Canceled) {
		if !started {
			writeRuntimeError(w, err)
		} else {
			_ = send("failure", map[string]string{"message": err.Error()})
		}
	}
}
