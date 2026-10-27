package daemon

import (
	"net/http"

	"github.com/mbianchidev/porto/internal/acceleration"
)

func (s *Server) accelerationStatus(w http.ResponseWriter, r *http.Request) {
	settings, err := s.architecture.Settings()
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	capabilities, probeErr := s.docker.AccelerationCapabilities(r.Context())
	message := ""
	if probeErr != nil {
		message = probeErr.Error()
	}
	writeJSON(w, map[string]any{"settings": settings, "capabilities": capabilities, "message": message})
}

func (s *Server) configureAcceleration(w http.ResponseWriter, r *http.Request) {
	var settings acceleration.Settings
	if !decodeRuntimeJSON(w, r, &settings) {
		return
	}
	if err := s.docker.ValidateAccelerationConfiguration(r.Context(), settings); err != nil {
		writeRuntimeError(w, err)
		return
	}
	if err := s.architecture.Save(r.Context(), settings); err != nil {
		writeRuntimeError(w, err)
		return
	}
	s.accelerationStatus(w, r)
}

func (s *Server) applyAcceleration(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RestartEngine bool `json:"restartEngine"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if err := s.docker.ApplyAcceleration(r.Context(), request.RestartEngine); err != nil {
		writeRuntimeError(w, err)
		return
	}
	s.accelerationStatus(w, r)
}
