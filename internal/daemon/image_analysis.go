package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/mbianchidev/porto/internal/imageanalysis"
)

func (s *Server) imageAnalysisRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/docker/analysis", s.listImageAnalysis)
	mux.HandleFunc("DELETE /api/docker/analysis", func(w http.ResponseWriter, _ *http.Request) {
		writeAnalysisResult(w, map[string]string{"status": "cleared"}, s.imageAnalysis.ClearCache())
	})
	mux.HandleFunc("POST /api/docker/images/{id}/analysis", s.requireRuntime("docker", s.startImageAnalysis))
	mux.HandleFunc("GET /api/docker/analysis/{id}", s.getImageAnalysis)
	mux.HandleFunc("POST /api/docker/analysis/{id}/cancel", s.cancelImageAnalysis)
	mux.HandleFunc("GET /api/docker/analysis/{id}/export", s.exportImageAnalysis)
	mux.HandleFunc("GET /api/docker/analysis/database", s.imageDatabaseStatus)
	mux.HandleFunc("POST /api/docker/analysis/database/update", s.updateImageDatabase)
	mux.HandleFunc("PUT /api/docker/analysis/database/import", s.importImageDatabase)
}

func (s *Server) listImageAnalysis(w http.ResponseWriter, _ *http.Request) {
	value, err := s.imageAnalysis.List()
	writeAnalysisResult(w, value, err)
}

func (s *Server) startImageAnalysis(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Platform string `json:"platform"`
		imageanalysis.Options
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	base := s.runtimeContext
	if base == nil {
		http.Error(w, "image analysis requires a running daemon", http.StatusServiceUnavailable)
		return
	}
	value, err := s.imageAnalysis.Start(base, r.PathValue("id"), request.Platform, request.Options)
	if err != nil {
		writeAnalysisResult(w, nil, err)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, value)
}

func (s *Server) getImageAnalysis(w http.ResponseWriter, r *http.Request) {
	value, err := s.imageAnalysis.Get(r.PathValue("id"))
	writeAnalysisResult(w, value, err)
}

func (s *Server) cancelImageAnalysis(w http.ResponseWriter, r *http.Request) {
	if err := s.imageAnalysis.Cancel(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]string{"status": "canceling"})
}

func (s *Server) exportImageAnalysis(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "report"
	}
	data, err := s.imageAnalysis.Export(r.Context(), r.PathValue("id"), format)
	if err != nil {
		writeAnalysisResult(w, nil, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="porto-image-analysis-`+format+`.json"`)
	_, _ = w.Write(data)
}

func (s *Server) imageDatabaseStatus(w http.ResponseWriter, r *http.Request) {
	value, err := s.imageAnalysis.DatabaseStatus(r.Context())
	writeAnalysisResult(w, value, err)
}

func (s *Server) updateImageDatabase(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ConfirmNetwork bool  `json:"confirmNetwork"`
		MaxBytes       int64 `json:"maxBytes"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if !request.ConfirmNetwork {
		http.Error(w, "explicit network-download consent is required; scans never update the database automatically", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	value, err := s.imageAnalysis.UpdateDatabase(ctx, request.MaxBytes)
	writeAnalysisResult(w, value, err)
}

func (s *Server) importImageDatabase(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, imageanalysis.MaxDatabaseBytes)
	value, err := s.imageAnalysis.ImportDatabase(r.Context(), r.Body)
	writeAnalysisResult(w, value, err)
}

func writeAnalysisResult(w http.ResponseWriter, value any, err error) {
	if err == nil {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, value)
		return
	}
	status := http.StatusInternalServerError
	if errors.Is(err, imageanalysis.ErrInvalid) {
		status = http.StatusBadRequest
	} else if errors.Is(err, imageanalysis.ErrBusy) {
		status = http.StatusConflict
	} else if errors.Is(err, os.ErrNotExist) {
		status = http.StatusNotFound
	}
	http.Error(w, err.Error(), status)
}
