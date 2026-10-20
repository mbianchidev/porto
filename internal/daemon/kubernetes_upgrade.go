package daemon

import (
	"context"
	"net/http"

	"github.com/mbianchidev/porto/internal/kubernetes"
)

func (s *Server) upgradeKubernetesCluster(w http.ResponseWriter, r *http.Request) {
	var request kubernetes.UpgradeRequest
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if !request.Confirm {
		http.Error(w, "confirm=true is required to upgrade a Kubernetes cluster", http.StatusBadRequest)
		return
	}
	if !s.beginRuntimeOperation() {
		http.Error(w, "Porto is shutting down; cluster upgrade was not started", http.StatusServiceUnavailable)
		return
	}
	defer s.endRuntimeOperation()
	base := s.runtimeContext
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, kubernetes.ClusterUpgradeTimeout)
	defer cancel()
	name := r.PathValue("name")
	release, err := s.beginKubernetesClusterOperation(ctx, name, "upgrade")
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	defer release()
	if err := s.stopKubernetesClusterForwards(name); err != nil {
		writeRuntimeError(w, err)
		return
	}
	result, err := s.clusters.UpgradeCluster(ctx, name, request)
	writeRuntimeResult(w, result, err)
}

func (s *Server) recoverKubernetesUpgrade(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeRuntimeJSON(w, r, &request) {
		return
	}
	if !request.Confirm {
		http.Error(w, "confirm=true is required to recover a Kubernetes cluster", http.StatusBadRequest)
		return
	}
	if !s.beginRuntimeOperation() {
		http.Error(w, "Porto is shutting down; recovery was not started", http.StatusServiceUnavailable)
		return
	}
	defer s.endRuntimeOperation()
	base := s.runtimeContext
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, kubernetes.ClusterUpgradeTimeout)
	defer cancel()
	name := r.PathValue("name")
	release, err := s.beginKubernetesClusterOperation(ctx, name, "upgrade-recover")
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	defer release()
	if err := s.clusters.RecoverUpgrade(ctx, name, request.Confirm); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "recovered"})
}
