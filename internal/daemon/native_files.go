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
	mux.HandleFunc("GET /api/files/attachments", func(w http.ResponseWriter, _ *http.Request) {
		if s.nativeFiles == nil {
			writeRuntimeError(w, errors.New("native files manager is unavailable"))
			return
		}
		writeJSON(w, s.nativeFiles.List())
	})
	mux.HandleFunc("GET /api/files/attachments/{id}", s.nativeFileAttachment)
	mux.HandleFunc("POST /api/files/attachments", s.createNativeFileAttachment)
	mux.HandleFunc("DELETE /api/files/attachments/{id}", s.detachNativeFileAttachment)
}

func (s *Server) nativeFileCapabilities(w http.ResponseWriter, r *http.Request) {
	if s.nativeFiles == nil {
		writeRuntimeError(w, errors.New("native files manager is unavailable"))
		return
	}
	kind := r.URL.Query().Get("kind")
	switch kind {
	case "container", "image", "volume", "vm":
	default:
		writeRuntimeError(w, fmt.Errorf("%w: native files require a container, image, volume or managed VM", datafiles.ErrInvalid))
		return
	}
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	if kind == "vm" && !settings.VMsEnabled || kind != "vm" && !settings.DockerEnabled {
		writeJSON(w, nativefiles.Capability{Message: "The selected runtime is disabled.", Fallback: "Enable the runtime explicitly in Settings, or use saved volume archives."})
		return
	}
	writeJSON(w, s.nativeFileCapability(r.Context(), kind, r.URL.Query().Get("name"), queryBool(r, "writable")))
}

func (s *Server) nativeFileCapability(ctx context.Context, kind, name string, writable bool) nativefiles.Capability {
	local := false
	var resource datafiles.Resource
	var err error
	if kind == "vm" {
		if name != "" {
			resource, err = s.vms.NativeFilesResource(ctx, name)
		}
	} else if name != "" {
		resource, local, err = s.docker.NativeFilesResource(ctx, kind, name, writable)
	} else {
		local, err = s.docker.NativeFilesLocal(ctx)
	}
	if err != nil {
		return nativefiles.Capability{Message: err.Error(), Fallback: "Start or reconnect the resource explicitly, or use the in-app Files inspector, verified volume archives, or porto vm copy."}
	}
	capability := s.nativeFiles.Capability(ctx, local)
	if resource.ID != "" {
		capability.Resource, capability.Identity = &resource, resource.Fingerprint()
	}
	return capability
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
	switch request.Kind {
	case "container", "image", "volume", "vm":
	default:
		writeRuntimeError(w, fmt.Errorf("%w: native files require a container, image, volume or managed VM", datafiles.ErrInvalid))
		return
	}
	if request.Name == "" {
		writeRuntimeError(w, fmt.Errorf("%w: select an exact resource name", datafiles.ErrInvalid))
		return
	}
	if s.nativeFiles == nil {
		writeRuntimeError(w, errors.New("native files manager is unavailable"))
		return
	}
	if !s.beginRuntimeOperation() {
		writeRuntimeError(w, fmt.Errorf("%w: Porto is shutting down", datafiles.ErrConflict))
		return
	}
	defer s.endRuntimeOperation()
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	if request.Kind != "vm" {
		if !settings.DockerEnabled {
			writeRuntimeError(w, errors.New("Docker runtime is disabled"))
			return
		}
	} else if !settings.VMsEnabled {
		writeRuntimeError(w, errors.New("VM runtime is disabled"))
		return
	}
	for _, existing := range s.nativeFiles.List() {
		if existing.Resource.Kind == request.Kind && (existing.Resource.Name == request.Name || existing.Resource.ID == request.Name) && existing.ReadOnly == !request.Writable {
			if request.Identity != "" && request.Identity != existing.Identity {
				writeRuntimeError(w, datafiles.ErrConflict)
				return
			}
			record, err := s.nativeFiles.Get(r.Context(), existing.ID)
			writeRuntimeResult(w, record, err)
			return
		}
	}
	capability := s.nativeFileCapability(r.Context(), request.Kind, request.Name, request.Writable)
	if !capability.Supported {
		writeRuntimeError(w, fmt.Errorf("%w: %s %s", datafiles.ErrUnsupported, capability.Message, capability.Fallback))
		return
	}
	if request.Identity != "" && request.Identity != capability.Identity {
		writeRuntimeError(w, datafiles.ErrConflict)
		return
	}
	var target nativefiles.Target
	if request.Kind == "vm" {
		target, err = s.vms.NativeFilesTarget(r.Context(), request.Name, request.Writable)
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
