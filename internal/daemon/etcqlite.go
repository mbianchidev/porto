package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/mbianchidev/porto/internal/etcqlite"
)

func (s *Server) etcqliteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/integrations/etcqlite", s.etcqliteStatus)
	mux.HandleFunc("POST /api/integrations/etcqlite/enable", s.setEtcqliteEnabled)
	mux.HandleFunc("POST /api/integrations/etcqlite/disable", s.setEtcqliteEnabled)
	mux.HandleFunc("GET /api/integrations/etcqlite/backups", s.requireEtcqlite(s.etcqliteBackups))
	mux.HandleFunc("POST /api/integrations/etcqlite/instances", s.requireEtcqlite(s.createEtcqlite))
	mux.HandleFunc("GET /api/integrations/etcqlite/instances/{name}/logs", s.requireEtcqlite(s.etcqliteLogs))
	for _, action := range []string{"start", "stop", "backup", "restore"} {
		mux.HandleFunc("POST /api/integrations/etcqlite/instances/{name}/"+action, s.requireEtcqlite(func(w http.ResponseWriter, r *http.Request) {
			s.etcqliteAction(w, r, action)
		}))
	}
	mux.HandleFunc("DELETE /api/integrations/etcqlite/instances/{name}", s.requireEtcqlite(func(w http.ResponseWriter, r *http.Request) {
		s.etcqliteAction(w, r, "delete")
	}))
}

func (s *Server) etcqliteStatus(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		writeEtcqliteError(w, err)
		return
	}
	if !settings.EtcqliteEnabled {
		writeJSON(w, etcqlite.Status{Supported: etcqlite.Supported(), State: "disabled", Message: "Disabled. No installation, scanning, startup or cluster retargeting.", Instances: []etcqlite.Instance{}})
		return
	}
	if s.etcqlite == nil || s.etcqliteErr != nil {
		writeEtcqliteError(w, errors.Join(errors.New("Etcqlite integration is unavailable"), s.etcqliteErr))
		return
	}
	status, err := s.etcqlite.Status(r.Context())
	if err != nil {
		writeEtcqliteError(w, err)
		return
	}
	writeJSON(w, status)
}

func (s *Server) requireEtcqlite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		settings, err := s.store.Settings(r.Context())
		if err != nil {
			writeEtcqliteError(w, err)
			return
		}
		if !settings.EtcqliteEnabled {
			writeEtcqliteError(w, etcqlite.ErrDisabled)
			return
		}
		if s.etcqlite == nil || s.etcqliteErr != nil {
			writeEtcqliteError(w, errors.Join(errors.New("Etcqlite integration is unavailable"), s.etcqliteErr))
			return
		}
		next(w, r)
	}
}

func (s *Server) setEtcqliteEnabled(w http.ResponseWriter, r *http.Request) {
	if s.etcqlite == nil || s.etcqliteErr != nil {
		writeEtcqliteError(w, errors.Join(errors.New("Etcqlite integration is unavailable"), s.etcqliteErr))
		return
	}
	settings, err := s.store.Settings(r.Context())
	if err != nil {
		writeEtcqliteError(w, err)
		return
	}
	settings.EtcqliteEnabled = strings.HasSuffix(r.URL.Path, "/enable")
	if err := s.etcqlite.Configure(r.Context(), settings.EtcqliteEnabled, func() error {
		return s.store.SetSettings(r.Context(), settings)
	}); err != nil {
		writeEtcqliteError(w, err)
		return
	}
	writeJSON(w, settings)
}

func (s *Server) createEtcqlite(w http.ResponseWriter, r *http.Request) {
	if !s.beginRuntimeOperation() {
		http.Error(w, "Porto is shutting down; Etcqlite creation was not started", http.StatusServiceUnavailable)
		return
	}
	defer s.endRuntimeOperation()
	var request etcqlite.CreateRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), etcqlite.OperationTimeout)
	defer cancel()
	instance, err := s.etcqlite.Create(ctx, request)
	if err != nil {
		writeEtcqliteError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, instance)
}

func (s *Server) etcqliteAction(w http.ResponseWriter, r *http.Request, action string) {
	if !s.beginRuntimeOperation() {
		http.Error(w, "Porto is shutting down; Etcqlite operation was not started", http.StatusServiceUnavailable)
		return
	}
	defer s.endRuntimeOperation()
	var request etcqlite.IdentityRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if request.ClusterID == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"message": "clusterID from the selected Etcqlite resource is required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), etcqlite.OperationTimeout)
	defer cancel()
	name := r.PathValue("name")
	var result any
	var err error
	switch action {
	case "start":
		result, err = s.etcqlite.Start(ctx, name, request.ClusterID)
	case "stop":
		result, err = s.etcqlite.Stop(ctx, name, request.ClusterID)
	case "backup":
		result, err = s.etcqlite.Backup(ctx, name, request.ClusterID)
	case "delete":
		var path string
		path, err = s.etcqlite.Delete(ctx, name, request.ClusterID, request.Confirm)
		result = map[string]string{"retainedPath": path, "message": "Verified native backup saved; identity and node-local data retained for explicit restoration."}
	case "restore":
		if !request.Confirm {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"message": "confirm=true is required for explicit restoration of retained Etcqlite data"})
			return
		}
		result, err = s.etcqlite.Restore(ctx, name, request.ClusterID)
	}
	if err != nil {
		slog.Error("Etcqlite lifecycle operation failed", "action", action, "instance", name, "error", err)
		writeEtcqliteError(w, err)
		return
	}
	writeJSON(w, result)
}

func (s *Server) etcqliteBackups(w http.ResponseWriter, r *http.Request) {
	backups, err := s.etcqlite.Backups(r.Context())
	if err != nil {
		writeEtcqliteError(w, err)
		return
	}
	writeJSON(w, backups)
}

func (s *Server) etcqliteLogs(w http.ResponseWriter, r *http.Request) {
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "stdout"
	}
	tail := 100
	if value := r.URL.Query().Get("tail"); value != "" {
		var err error
		tail, err = strconv.Atoi(value)
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"message": "Etcqlite log tail must be an integer"})
			return
		}
	}
	lines, err := s.etcqlite.Logs(r.Context(), r.PathValue("name"), stream, tail)
	if err != nil {
		writeEtcqliteError(w, err)
		return
	}
	writeJSON(w, lines)
}

func writeEtcqliteError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, etcqlite.ErrDisabled):
		status = http.StatusServiceUnavailable
	case errors.Is(err, etcqlite.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		status = http.StatusServiceUnavailable
	case errors.Is(err, etcqlite.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, etcqlite.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, etcqlite.ErrUnsupported):
		status = http.StatusNotImplemented
	case errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	case strings.Contains(err.Error(), "unavailable"), strings.Contains(err.Error(), "not installed"):
		status = http.StatusServiceUnavailable
	}
	writeJSONStatus(w, status, map[string]string{"message": err.Error()})
}
