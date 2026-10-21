package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/mbianchidev/porto/internal/autoscaling"
)

type kubernetesAutoscalingService interface {
	Run(context.Context) error
	Configure(context.Context, string, autoscaling.Config) (autoscaling.Status, error)
	Status(context.Context, string) (autoscaling.Status, error)
	Pause(context.Context, string) error
	Resume(context.Context, string) error
	Forget(context.Context, string) error
	Rename(context.Context, string, string) error
	ApplyScaledObject(context.Context, string, autoscaling.ScaledObject) error
	DeleteScaledObject(context.Context, string, string, string) error
}

func (s *Server) autoscalingOperation(ctx context.Context, name, action string) (func(), error) {
	if !s.beginRuntimeOperation() {
		return nil, errors.New("Porto is shutting down; autoscaling mutation was not started")
	}
	settings, err := s.store.Settings(ctx)
	if err != nil || !settings.KubernetesEnabled {
		s.endRuntimeOperation()
		return nil, errors.Join(err, errors.New("Kubernetes runtime is disabled"))
	}
	release, err := s.beginKubernetesClusterOperation(ctx, name, action)
	if err != nil {
		s.endRuntimeOperation()
		return nil, err
	}
	return func() {
		release()
		s.endRuntimeOperation()
	}, nil
}

func (s *Server) setKubernetesAutoscalingPaused(ctx context.Context, paused bool) error {
	if s.autoscaling == nil {
		return nil
	}
	names, err := s.clusters.ManagedNames()
	if err != nil {
		return err
	}
	var failures []error
	for _, name := range names {
		if paused {
			err = s.autoscaling.Pause(ctx, name)
		} else {
			err = s.autoscaling.Resume(ctx, name)
		}
		if err != nil {
			if paused {
				failures = append(failures, fmt.Errorf("autoscaling for %s: %w", name, err))
			} else {
				log.Printf("Kubernetes enabled, but autoscaling for %s remains paused: %v", name, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (s *Server) resumeKubernetesAutoscaling(ctx context.Context, name string) string {
	if s.autoscaling == nil {
		return ""
	}
	if err := s.autoscaling.Resume(ctx, name); err != nil {
		log.Printf("resume Kubernetes autoscaling for %s: %v", name, err)
		return "Cluster operation completed, but autoscaling could not resume: " + err.Error()
	}
	return ""
}

func (s *Server) kubernetesAutoscalingStatus(w http.ResponseWriter, r *http.Request) {
	if s.autoscaling == nil {
		http.Error(w, "Kubernetes autoscaling manager is unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	value, err := s.autoscaling.Status(ctx, r.PathValue("name"))
	writeRuntimeResult(w, value, err)
}

func (s *Server) configureKubernetesAutoscaling(w http.ResponseWriter, r *http.Request) {
	if s.autoscaling == nil {
		http.Error(w, "Kubernetes autoscaling manager is unavailable", http.StatusServiceUnavailable)
		return
	}
	var request autoscaling.Config
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	base := s.runtimeContext
	if base == nil {
		base = r.Context()
	}
	ctx, cancel := context.WithTimeout(base, 20*time.Minute)
	defer cancel()
	// Configure pauses its controller before acquiring the shared operation gate.
	value, err := s.autoscaling.Configure(ctx, r.PathValue("name"), request)
	writeRuntimeResult(w, value, err)
}

func (s *Server) applyKubernetesWorkloadScaling(w http.ResponseWriter, r *http.Request) {
	if s.autoscaling == nil {
		http.Error(w, "Kubernetes autoscaling manager is unavailable", http.StatusServiceUnavailable)
		return
	}
	var request autoscaling.ScaledObject
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if err := s.autoscaling.ApplyScaledObject(r.Context(), r.PathValue("name"), request); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "configured"})
}

func (s *Server) deleteKubernetesWorkloadScaling(w http.ResponseWriter, r *http.Request) {
	if s.autoscaling == nil {
		http.Error(w, "Kubernetes autoscaling manager is unavailable", http.StatusServiceUnavailable)
		return
	}
	if !queryBool(r, "confirm") {
		http.Error(w, "confirm=true is required to remove a workload scaler", http.StatusBadRequest)
		return
	}
	if err := s.autoscaling.DeleteScaledObject(r.Context(), r.PathValue("name"), r.PathValue("namespace"), r.PathValue("scaler")); err != nil {
		writeRuntimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
