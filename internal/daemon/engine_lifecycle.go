package daemon

import (
	"context"
	"fmt"
	"net/http"

	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func (s *Server) requireDetachedVMFiles(w http.ResponseWriter, name string) bool {
	if s.nativeFiles != nil {
		if err := s.nativeFiles.Guard("vm", name); err != nil {
			writeRuntimeError(w, err)
			return false
		}
	}
	return true
}

// startDockerEngine boots the existing Porto-owned engine. It never creates or
// repairs one; use engine/install for that. The boot is bound to the daemon's
// lifetime rather than the request, so a reloaded dashboard or an impatient
// client cannot interrupt limactl halfway through starting the guest.
func (s *Server) startDockerEngine(w http.ResponseWriter, r *http.Request) {
	if !s.beginRuntimeOperation() {
		http.Error(w, "Porto is shutting down; the engine was not started", http.StatusServiceUnavailable)
		return
	}
	defer s.endRuntimeOperation()
	ctx := s.runtimeContext
	if ctx == nil {
		ctx = context.WithoutCancel(r.Context())
	}
	if err := s.docker.StartEngine(ctx); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "started"})
}

func (s *Server) stopDockerEngine(w http.ResponseWriter, r *http.Request) {
	if s.hasRuntimeOperations() || !s.beginRuntimeOperation() {
		writeRuntimeError(w, fmt.Errorf("%w: finish or cancel active runtime/data work before stopping the engine", portodocker.ErrConflict))
		return
	}
	defer s.endRuntimeOperation()
	if err := s.docker.StopEngine(r.Context()); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "stopped"})
}

func (s *Server) removeDockerEngine(w http.ResponseWriter, r *http.Request) {
	if !queryBool(r, "confirm") {
		http.Error(w, "confirm=true is required to remove the engine and its data", http.StatusBadRequest)
		return
	}
	if s.hasRuntimeOperations() || !s.beginRuntimeOperation() {
		writeRuntimeError(w, fmt.Errorf("%w: finish or cancel active runtime/data work before removing the engine", portodocker.ErrConflict))
		return
	}
	defer s.endRuntimeOperation()
	if s.nativeFiles != nil {
		if err := s.nativeFiles.Close(r.Context()); err != nil {
			writeRuntimeError(w, err)
			return
		}
	}
	if err := s.docker.RemoveEngine(r.Context()); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "removed"})
}
