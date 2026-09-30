package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/nativefiles"
)

func (s *Server) nativeFileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/files/capabilities", s.nativeFileCapabilities)
	mux.HandleFunc("GET /api/files/attachments", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, s.nativeFiles.List()) })
	mux.HandleFunc("GET /api/files/attachments/{id}", s.nativeFileAttachment)
	mux.HandleFunc("POST /api/files/attachments", s.createNativeFileAttachment)
	mux.HandleFunc("DELETE /api/files/attachments/{id}", s.detachNativeFileAttachment)
}

func (s *Server) nativeFileCapabilities(w http.ResponseWriter, r *http.Request) {
	if s.nativeFiles == nil {
		writeRuntimeError(w, errors.New("native files manager is unavailable"))
		return
	}
	local := false
	if r.URL.Query().Get("kind") != "vm" {
		var err error
		local, err = s.docker.NativeFilesLocal(r.Context())
		if err != nil {
			writeJSON(w, nativefiles.Capability{Supported: false, Message: err.Error(), Fallback: "Start the Porto engine explicitly or use saved volume archives."})
			return
		}
	}
	writeJSON(w, s.nativeFiles.Capability(r.Context(), local))
}

func (s *Server) createNativeFileAttachment(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Kind, Name, Identity string
		Writable, Confirm    bool
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if !request.Confirm {
		writeRuntimeError(w, fmt.Errorf("%w: explicit native access consent is required", datafiles.ErrInvalid))
		return
	}
	if s.nativeFiles == nil {
		writeRuntimeError(w, errors.New("native files manager is unavailable"))
		return
	}
	for _, existing := range s.nativeFiles.List() {
		if existing.Resource.Kind == request.Kind && (existing.Resource.Name == request.Name || existing.Resource.ID == request.Name) && existing.ReadOnly == !request.Writable {
			record, err := s.nativeFiles.Get(r.Context(), existing.ID)
			writeRuntimeResult(w, record, err)
			return
		}
	}
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	local := false
	if request.Kind != "vm" {
		if !settings.DockerEnabled {
			writeRuntimeError(w, errors.New("Docker runtime is disabled"))
			return
		}
		local, err = s.docker.NativeFilesLocal(r.Context())
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
	} else if !settings.VMsEnabled {
		writeRuntimeError(w, errors.New("VM runtime is disabled"))
		return
	}
	capability := s.nativeFiles.Capability(r.Context(), local)
	if !capability.Supported {
		writeRuntimeError(w, fmt.Errorf("%w: %s %s", datafiles.ErrUnsupported, capability.Message, capability.Fallback))
		return
	}
	var target nativefiles.Target
	if request.Kind == "vm" {
		target, err = s.vms.NativeFilesTarget(r.Context(), request.Name)
	} else {
		target, err = s.docker.NativeFilesTarget(r.Context(), request.Kind, request.Name, request.Writable)
	}
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	if request.Identity != "" && request.Identity != target.Identity {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
		defer cancel()
		writeRuntimeError(w, errors.Join(datafiles.ErrConflict, target.Release(cleanup)))
		return
	}
	attachment, err := s.nativeFiles.Attach(r.Context(), target, request.Writable)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
		defer cancel()
		err = errors.Join(err, target.Release(cleanup))
	}
	writeRuntimeResult(w, attachment, err)
}

func (s *Server) nativeFileAttachment(w http.ResponseWriter, r *http.Request) {
	if s.nativeFiles == nil {
		writeRuntimeError(w, errors.New("native files manager is unavailable"))
		return
	}
	record, err := s.nativeFiles.Get(r.Context(), r.PathValue("id"))
	writeRuntimeResult(w, record, err)
}

func (s *Server) detachNativeFileAttachment(w http.ResponseWriter, r *http.Request) {
	if s.nativeFiles == nil {
		writeRuntimeError(w, errors.New("native files manager is unavailable"))
		return
	}
	if err := s.nativeFiles.Detach(r.Context(), r.PathValue("id")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
