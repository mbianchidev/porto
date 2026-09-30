package daemon

import (
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
