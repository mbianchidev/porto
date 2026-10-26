package daemon

import (
	"fmt"
	"net/http"

	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func (s *Server) engineResourceStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.docker.EngineResourceStatus(r.Context())
	writeRuntimeResult(w, status, err)
}

func (s *Server) configureEngineResources(w http.ResponseWriter, r *http.Request) {
	var limits portodocker.EngineLimits
	if !decodeRuntimeJSON(w, r, &limits) {
		return
	}
	if err := s.docker.SetEngineLimits(r.Context(), limits); err != nil {
		writeRuntimeError(w, err)
		return
	}
	s.engineResourceStatus(w, r)
}

func (s *Server) applyEngineResources(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RestartEngine bool `json:"restartEngine"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if s.hasRuntimeOperations() || !s.beginRuntimeOperation() {
		writeRuntimeError(w, fmt.Errorf("%w: active runtime/data work prevents engine configuration", portodocker.ErrConflict))
		return
	}
	defer s.endRuntimeOperation()
	if err := s.docker.ApplyEngineLimits(r.Context(), request.RestartEngine); err != nil {
		writeRuntimeError(w, err)
		return
	}
	s.engineResourceStatus(w, r)
}
