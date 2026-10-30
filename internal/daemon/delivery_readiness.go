package daemon

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/operations"
)

func (s *Server) waitForDeliveryReady(project app.Project, running *projectProcess) {
	integration, ok := s.compose.(interface {
		WaitHealthy(context.Context, app.Project) error
	})
	if !ok {
		return
	}
	base := s.runtimeContext
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 2*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-running.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	err := integration.WaitHealthy(ctx, project)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running[project.ID] != running || errors.Is(err, context.Canceled) {
		return
	}
	status := "running"
	if err != nil {
		status = "crashed"
	}
	running.project.Status = status
	persist, cancelPersist := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPersist()
	if storeErr := s.store.SetRuntime(persist, project.ID, status, running.project.PID, running.project.Port); storeErr != nil {
		slog.Error("Unable to record Compose readiness", "project", project.ID, "error", storeErr)
	}
	if err != nil {
		if logErr := s.store.AddLog(persist, project.ID, "system", operations.PublicMessage(err)); logErr != nil {
			slog.Error("Unable to record Compose readiness failure", "project", project.ID, "error", logErr)
		}
	}
}
