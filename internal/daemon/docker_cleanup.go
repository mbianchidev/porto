package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/store"
)

func (s *Server) dockerCleanupStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.store.DockerCleanupStatus(r.Context())
	if err == nil {
		s.cleanupMu.Lock()
		unstored := s.cleanupUnstored
		s.cleanupMu.Unlock()
		if unstored != nil {
			for index, run := range status.Runs {
				if run.ID == unstored.ID && run.Status == app.CleanupRunning {
					status.Runs[index] = *unstored
				}
			}
		}
	}
	writeRuntimeResult(w, status, err)
}

func (s *Server) runDockerCleanupNow(w http.ResponseWriter, r *http.Request) {
	run, err := s.startDockerCleanup(r.Context(), app.CleanupManual)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	if run == nil {
		writeRuntimeError(w, errors.New("Docker cleanup was not started"))
		return
	}
	writeJSONStatus(w, http.StatusAccepted, run)
}

func (s *Server) startDockerCleanup(ctx context.Context, trigger app.DockerCleanupTrigger) (*app.DockerCleanupRun, error) {
	s.cleanupMu.Lock()
	defer s.cleanupMu.Unlock()
	if s.cleanupDone != nil {
		return nil, fmt.Errorf("%w: Docker cleanup is already running", portodocker.ErrConflict)
	}
	if s.cleanupUnstored != nil {
		return nil, fmt.Errorf("%w: cleanup results could not be saved; resolve the storage error and restart Porto before retrying", portodocker.ErrUnavailable)
	}
	if s.runtimeContext == nil || s.cleanupRunner == nil {
		return nil, fmt.Errorf("%w: the Docker cleanup worker is not initialized", portodocker.ErrUnavailable)
	}
	if s.runtimeContext.Err() != nil || !s.beginRuntimeOperation() {
		return nil, fmt.Errorf("%w: Porto is shutting down", portodocker.ErrConflict)
	}
	run, err := s.store.BeginDockerCleanup(ctx, trigger, s.dockerCleanupTime())
	if err != nil || run == nil {
		s.endRuntimeOperation()
		if errors.Is(err, store.ErrDockerCleanupRunning) || errors.Is(err, store.ErrDockerCleanupDisabled) {
			err = fmt.Errorf("%w: %w", portodocker.ErrConflict, err)
		}
		return nil, err
	}
	runContext, cancel := context.WithTimeout(s.runtimeContext, app.DockerCleanupTimeout)
	done := make(chan struct{})
	s.cleanupCancel = cancel
	s.cleanupDone = done
	go s.executeDockerCleanup(runContext, cancel, done, *run)
	return run, nil
}

func (s *Server) executeDockerCleanup(ctx context.Context, cancel context.CancelFunc, done chan struct{}, run app.DockerCleanupRun) {
	defer func() {
		cancel()
		s.cleanupMu.Lock()
		s.endRuntimeOperation()
		s.cleanupCancel = nil
		s.cleanupDone = nil
		close(done)
		s.cleanupMu.Unlock()
	}()
	log.Printf("Docker cleanup %d (%s) started", run.ID, run.Trigger)
	result, cleanupErr := s.cleanupRunner(ctx)
	status := app.CleanupSucceeded
	message := ""
	if cleanupErr != nil {
		status = app.CleanupFailed
		message = cleanupErr.Error()
		if errors.Is(ctx.Err(), context.Canceled) {
			status = app.CleanupInterrupted
		} else if errors.Is(cleanupErr, portodocker.ErrConflict) {
			status = app.CleanupSkipped
		}
	}
	finished := s.dockerCleanupTime()
	persistContext, cancelPersist := context.WithTimeout(context.Background(), 5*time.Second)
	err := s.store.FinishDockerCleanup(persistContext, run.ID, status, result, message, finished)
	cancelPersist()
	if err != nil {
		log.Printf("persist Docker cleanup %d result: %v; cleanup status=%s error=%s", run.ID, err, status, message)
		status = app.CleanupFailed
		message = errors.Join(cleanupErr, fmt.Errorf("cleanup results could not be saved: %w", err)).Error()
		run.Status, run.Result, run.Error = status, result, message
		run.CompletedAt = finished.Format(time.RFC3339Nano)
		s.cleanupMu.Lock()
		s.cleanupUnstored = &run
		s.cleanupMu.Unlock()
	}
	cacheBytes := "not reported"
	if result.BuildCache.BytesReclaimed != nil {
		cacheBytes = fmt.Sprintf("%d", *result.BuildCache.BytesReclaimed)
	}
	log.Printf("Docker cleanup %d (%s) %s: image references removed=%d, build cache records removed=%d, build cache bytes reclaimed=%s, error=%q",
		run.ID, run.Trigger, status, result.Images.ItemsRemoved, result.BuildCache.ItemsRemoved, cacheBytes, message)
}

func (s *Server) dockerCleanupLoop(ctx context.Context) {
	s.checkScheduledDockerCleanup(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkScheduledDockerCleanup(ctx)
		}
	}
}

func (s *Server) checkScheduledDockerCleanup(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if _, err := s.startDockerCleanup(ctx, app.CleanupScheduled); err != nil && !errors.Is(err, portodocker.ErrConflict) {
		log.Printf("schedule Docker cleanup: %v", err)
	}
}

func (s *Server) stopDockerCleanup(ctx context.Context) error {
	s.cleanupMu.Lock()
	cancel, done := s.cleanupCancel, s.cleanupDone
	if cancel != nil {
		cancel()
	}
	s.cleanupMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for Docker cleanup to stop: %w", context.Cause(ctx))
	}
}

func (s *Server) dockerCleanupTime() time.Time {
	if s.cleanupNow != nil {
		return s.cleanupNow().UTC()
	}
	return time.Now().UTC()
}
