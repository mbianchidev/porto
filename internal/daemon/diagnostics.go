package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/certificates"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/diagnostics"
	portodocker "github.com/mbianchidev/porto/internal/docker"
)

type diagnosticsRepairRequest struct {
	Confirm bool   `json:"confirm"`
	Target  string `json:"target"`
}

func (s *Server) diagnosticsCollector() *diagnostics.Collector {
	stateDirectory, _ := config.Dir()
	logPath, _ := config.LogPath()
	var settings diagnostics.SettingsReader
	if s.store != nil {
		settings = s.store
	}
	return &diagnostics.Collector{
		Settings: settings,
		Docker:   s.docker,
		DockerStatus: func(ctx context.Context) portodocker.Status {
			return s.dockerEndpointStatus(s.docker.Status(ctx, s.dockerSocket))
		},
		Kubernetes:        s.kubernetes,
		Clusters:          s.clusters,
		VMs:               s.vms,
		Providers:         s.providers,
		DockerSocket:      s.dockerSocket,
		StateDirectory:    stateDirectory,
		LogPath:           logPath,
		DaemonAvailable:   true,
		DashboardReady:    s.ui != nil,
		DaemonIdentity:    s.daemonIdentity,
		CertificateStatus: s.diagnosticCertificateStatus,
	}
}

func (s *Server) diagnosticCertificateStatus() (certificates.Status, error) {
	if s.tlsCertificates == nil {
		return certificates.Status{}, errors.New("TLS certificate manager is not initialized")
	}
	return s.tlsCertificates.Status()
}

func (s *Server) diagnosticsReport(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.diagnosticsCollector().Collect(r.Context()))
}

func (s *Server) diagnosticsBundlePreview(w http.ResponseWriter, r *http.Request) {
	collector := s.diagnosticsCollector()
	report := collector.Collect(r.Context())
	writeJSON(w, diagnostics.PreviewBundle(
		report,
		collector.BundleSources(r.Context()),
		collector.RedactionContext(),
	))
}

func (s *Server) diagnosticsBundle(w http.ResponseWriter, r *http.Request) {
	collector := s.diagnosticsCollector()
	report := collector.Collect(r.Context())
	var bundle bytes.Buffer
	_, err := diagnostics.BuildBundle(
		&bundle,
		report,
		collector.BundleSources(r.Context()),
		collector.RedactionContext(),
	)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	filename := "porto-diagnostics-" + report.GeneratedAt.Format("20060102T150405Z") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", bundle.Len()))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(bundle.Bytes()); err != nil {
		log.Printf("write diagnostic bundle: %v", err)
		return
	}
	log.Printf("generated local diagnostic bundle with %d bytes", bundle.Len())
}

func (s *Server) diagnosticsRepair(w http.ResponseWriter, r *http.Request) {
	var request diagnosticsRepairRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if !request.Confirm {
		http.Error(w, "diagnostic repair requires explicit confirmation", http.StatusBadRequest)
		return
	}
	action := strings.TrimSpace(r.PathValue("action"))
	if action == "" {
		http.Error(w, "diagnostic repair action is required", http.StatusBadRequest)
		return
	}
	timeout := runtimeOperationTimeout
	if action == "repair-kubernetes-addons" {
		timeout = 15 * time.Minute
	}
	repairContext, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	var err error
	switch action {
	case "restart-docker-runtime":
		err = s.repairDockerRuntime(repairContext)
	case "reinstall-docker-context":
		err = s.docker.InstallContext(repairContext, s.dockerSocket)
	case "repair-kubernetes-addons":
		if strings.TrimSpace(request.Target) == "" {
			err = errors.New("Kubernetes cluster target is required")
		} else {
			err = s.clusters.EnsureAddons(repairContext, request.Target)
		}
	case "renew-certificates":
		_, err = s.renewProjectCertificate(repairContext)
	default:
		http.Error(w, "unknown diagnostic repair action", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("diagnostic repair %s failed: %v", action, err)
		writeRuntimeError(w, err)
		return
	}
	log.Printf("diagnostic repair %s completed", action)
	report := s.diagnosticsCollector().Collect(r.Context())
	writeJSON(w, map[string]any{
		"action":      action,
		"target":      request.Target,
		"status":      "completed",
		"completedAt": time.Now().UTC(),
		"report":      report,
	})
}

func (s *Server) repairDockerRuntime(ctx context.Context) error {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return err
	}
	if !settings.DockerEnabled {
		return errors.New("Docker runtime is disabled")
	}
	ownership := s.docker.EngineOwnershipStatus(ctx)
	if ownership.Conflict {
		return fmt.Errorf("refusing Docker runtime repair because engine ownership is ambiguous: %s", ownership.Message)
	}
	closeContext, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := s.stopDockerAPI(closeContext); err != nil {
		return fmt.Errorf("stop Docker runtime connections: %w", err)
	}
	if err := s.startDockerAPI(s.runtimeContext); err != nil {
		return fmt.Errorf("restart Docker runtime connections: %w", err)
	}
	return nil
}
