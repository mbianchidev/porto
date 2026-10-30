package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/autoscaling"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/filelock"
)

func (s *Server) lockKubeconfigStorage(ctx context.Context) (func(), error) {
	lockContext, cancel := context.WithTimeout(ctx, httpShutdownTimeout)
	defer cancel()
	lock, err := filelock.Acquire(lockContext, filepath.Join(s.store.Directory(), "kubeconfig-storage.lock"))
	if err != nil {
		return nil, fmt.Errorf("another daemon may own kubeconfig storage: %w", err)
	}
	return func() {
		if err := lock.Close(); err != nil {
			slog.Error("Unable to release kubeconfig storage ownership", "error", err)
		}
	}, nil
}

func (s *Server) applyKubeconfigStorage(ctx context.Context, settings app.Settings) error {
	s.kubeStorageErr = nil
	directory, migrationErr := config.ApplyKubeconfigDirectory(ctx, settings.KubeconfigDirectory)
	if migrationErr != nil {
		s.kubeStorageErr = migrationErr
		slog.Error("Kubeconfig folder change could not complete", "error", migrationErr)
		// The committed locator is authoritative, including after a cleanup error.
		var err error
		directory, err = config.KubernetesConfigDir()
		if err != nil {
			return fmt.Errorf("resolve active kubeconfig folder after migration: %w", err)
		}
	}
	s.kubeconfigDir = directory
	s.kubernetes.SetKubeconfigRoot(directory)
	s.clusters.SetKubeconfigRoot(directory)
	manager, err := autoscaling.New(s.clusters, directory, s.autoscalingOperation)
	if err != nil {
		return fmt.Errorf("initialize relocated Kubernetes autoscaling: %w", err)
	}
	s.autoscaling = manager
	return nil
}

func (s *Server) kubeconfigSettings(settings app.Settings) app.Settings {
	settings.KubeconfigActiveDirectory = s.kubeconfigDir
	settings.KubeconfigStorageError = ""
	if s.kubeStorageErr != nil {
		settings.KubeconfigStorageError = s.kubeStorageErr.Error()
	}
	if settings.KubeconfigDirectory == "" {
		root, err := config.Dir()
		if err != nil {
			settings.KubeconfigStorageError = fmt.Sprintf("resolve default kubeconfig folder: %v", err)
			settings.KubeconfigRestartRequired = true
			return settings
		}
		settings.KubeconfigRestartRequired = s.kubeconfigDir != "" &&
			s.kubeconfigDir != filepath.Join(root, "kubernetes")
	} else {
		settings.KubeconfigRestartRequired = settings.KubeconfigDirectory != s.kubeconfigDir
	}
	return settings
}
