package daemon

import (
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) synchronizedShareStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.syncShares.Status(r.Context())
	writeRuntimeResult(w, status, err)
}

func (s *Server) createSynchronizedShare(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ProjectID int64    `json:"projectId"`
		Project   string   `json:"project"`
		Ignore    []string `json:"ignore"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	name := strings.TrimSpace(request.Project)
	if request.ProjectID > 0 {
		name = strconv.FormatInt(request.ProjectID, 10)
	}
	if name == "" {
		http.Error(w, "select a registered project before enabling synchronized sharing", http.StatusBadRequest)
		return
	}
	project, err := s.store.GetProject(r.Context(), name)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	share, err := s.syncShares.Create(r.Context(), project.ID, project.Path, request.Ignore)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, share)
}

func (s *Server) configureSynchronizedShare(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Mode string `json:"mode"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if err := s.syncShares.SetMode(r.Context(), r.PathValue("id"), request.Mode); err != nil {
		writeRuntimeError(w, err)
		return
	}
	s.synchronizedShareStatus(w, r)
}

func (s *Server) deleteSynchronizedShare(w http.ResponseWriter, r *http.Request) {
	if !queryBool(r, "confirm") {
		http.Error(w, "confirm=true is required; both filesystem copies will be retained", http.StatusBadRequest)
		return
	}
	if err := s.syncShares.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "detached", "message": "Only the owned synchronization session was removed. Host and guest data were retained."})
}
