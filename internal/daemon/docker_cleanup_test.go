package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/store"
)

func cleanupServerFixture(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	return cleanupServerAtPath(t, filepath.Join(t.TempDir(), "porto.db"))
}

func cleanupServerAtPath(t *testing.T, path string) (*Server, *http.ServeMux) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{
		store: st, runtimeContext: ctx, cleanupNow: time.Now,
		cleanupRunner: func(context.Context) (app.DockerCleanupResult, error) {
			return successfulCleanupResult(), nil
		},
	}
	mux := http.NewServeMux()
	server.runtimeRoutes(mux)
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := server.stopDockerCleanup(closeContext); err != nil {
			t.Errorf("stop test cleanup: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Errorf("close test store: %v", err)
		}
	})
	return server, mux
}

func successfulCleanupResult() app.DockerCleanupResult {
	reclaimed := int64(4096)
	return app.DockerCleanupResult{
		BuildCache: app.DockerCleanupStep{Status: app.CleanupSucceeded, ItemsRemoved: 3, BytesReclaimed: &reclaimed},
		Images:     app.DockerCleanupStep{Status: app.CleanupSucceeded, ItemsRemoved: 2, Output: "Removed synthetic image references"},
	}
}

func waitForTestCleanup(t *testing.T, server *Server) app.DockerCleanupStatus {
	t.Helper()
	server.cleanupMu.Lock()
	done := server.cleanupDone
	server.cleanupMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup did not complete")
		}
	}
	status, err := server.store.DockerCleanupStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func TestDockerCleanupRunNowIsAsynchronousAndIndependentOfScheduling(t *testing.T) {
	server, mux := cleanupServerFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	canceled := make(chan struct{})
	server.cleanupRunner = func(ctx context.Context) (app.DockerCleanupResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			close(canceled)
			return app.NewDockerCleanupResult(), context.Cause(ctx)
		case <-release:
			return successfulCleanupResult(), nil
		}
	}
	requestContext, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	request := httptest.NewRequest(http.MethodPost, "/api/docker/cleanup", nil).WithContext(requestContext)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("run now = %d: %s", response.Code, response.Body.String())
	}
	var accepted app.DockerCleanupRun
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted.Trigger != app.CleanupManual || accepted.Status != app.CleanupRunning {
		t.Fatalf("accepted response claimed a completed or scheduled run: %+v", accepted)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted cleanup did not start")
	}
	cancelRequest()
	select {
	case <-canceled:
		t.Fatal("closing the page canceled an accepted cleanup")
	case <-time.After(10 * time.Millisecond):
	}
	if !server.hasRuntimeOperations() {
		t.Fatal("cleanup was not registered as an active runtime operation")
	}
	overlap := httptest.NewRecorder()
	mux.ServeHTTP(overlap, httptest.NewRequest(http.MethodPost, "/api/docker/cleanup", nil))
	if overlap.Code != http.StatusConflict {
		t.Fatalf("overlapping cleanup = %d: %s", overlap.Code, overlap.Body.String())
	}
	close(release)
	status := waitForTestCleanup(t, server)
	if status.Enabled || status.NextRunAt != "" || len(status.Runs) != 1 ||
		status.Runs[0].Status != app.CleanupSucceeded || status.Runs[0].Result.Images.ItemsRemoved != 2 {
		t.Fatalf("manual cleanup changed scheduling or lost results: %+v", status)
	}
	if server.hasRuntimeOperations() {
		t.Fatal("finished cleanup leaked its runtime-operation registration")
	}
}

func TestScheduledDockerCleanupReportsFailuresAndRetainsWeeklyCadence(t *testing.T) {
	server, mux := cleanupServerFixture(t)
	settings, err := server.store.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings.DockerAutoPruneEnabled = true
	if err := server.store.SetSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	initial, err := server.store.DockerCleanupStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	due, err := time.Parse(time.RFC3339Nano, initial.NextRunAt)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server.cleanupRunner = func(context.Context) (app.DockerCleanupResult, error) {
		calls.Add(1)
		result := successfulCleanupResult()
		result.Images.Status = app.CleanupFailed
		result.Images.Error = "synthetic image cleanup failed"
		return result, errors.New(result.Images.Error)
	}
	server.cleanupNow = func() time.Time { return due.Add(-time.Nanosecond) }
	server.checkScheduledDockerCleanup(context.Background())
	if calls.Load() != 0 {
		t.Fatal("scheduled cleanup ran early")
	}
	server.cleanupNow = func() time.Time { return due.Add(3 * app.DockerCleanupInterval) }
	server.checkScheduledDockerCleanup(context.Background())
	status := waitForTestCleanup(t, server)
	if calls.Load() != 1 || len(status.Runs) != 1 || status.Runs[0].Trigger != app.CleanupScheduled ||
		status.Runs[0].Status != app.CleanupFailed || status.Runs[0].Result.BuildCache.ItemsRemoved != 3 ||
		!strings.Contains(status.Runs[0].Error, "synthetic image cleanup failed") {
		t.Fatalf("scheduled failure did not retain its partial result: %+v, calls=%d", status, calls.Load())
	}
	server.checkScheduledDockerCleanup(context.Background())
	if calls.Load() != 1 {
		t.Fatal("missed weeks or a failure caused an immediate cleanup retry loop")
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/docker/cleanup", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"trigger":"scheduled"`) ||
		!strings.Contains(response.Body.String(), "synthetic image cleanup failed") {
		t.Fatalf("scheduled results are not available through the API: %d %s", response.Code, response.Body.String())
	}
}

func TestDockerCleanupDisabledRuntimeRemainsReadable(t *testing.T) {
	server, mux := cleanupServerFixture(t)
	settings, err := server.store.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings.DockerEnabled = false
	if err := server.store.SetSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	for method, wantStatus := range map[string]int{http.MethodGet: http.StatusOK, http.MethodPost: http.StatusConflict} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(method, "/api/docker/cleanup", nil))
		if response.Code != wantStatus {
			t.Fatalf("%s cleanup = %d: %s", method, response.Code, response.Body.String())
		}
	}
}

func TestDockerCleanupShutdownReportsInterruption(t *testing.T) {
	server, _ := cleanupServerFixture(t)
	started := make(chan struct{})
	server.cleanupRunner = func(ctx context.Context) (app.DockerCleanupResult, error) {
		close(started)
		<-ctx.Done()
		result := app.NewDockerCleanupResult()
		result.BuildCache = app.DockerCleanupStep{Status: app.CleanupSucceeded, ItemsRemoved: 1}
		return result, context.Cause(ctx)
	}
	if _, err := server.startDockerCleanup(context.Background(), app.CleanupManual); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.stopDockerCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	status := waitForTestCleanup(t, server)
	if status.Runs[0].Status != app.CleanupInterrupted || status.Runs[0].CompletedAt == "" ||
		status.Runs[0].Result.BuildCache.ItemsRemoved != 1 || status.Runs[0].Error == "" {
		t.Fatalf("shutdown hid partial cleanup results: %+v", status)
	}
}

func TestDockerCleanupBusyBuildIsReportedAsSkipped(t *testing.T) {
	server, _ := cleanupServerFixture(t)
	server.cleanupRunner = func(context.Context) (app.DockerCleanupResult, error) {
		return app.NewDockerCleanupResult(), portodocker.ErrConflict
	}
	if _, err := server.startDockerCleanup(context.Background(), app.CleanupManual); err != nil {
		t.Fatal(err)
	}
	status := waitForTestCleanup(t, server)
	if status.Runs[0].Status != app.CleanupSkipped || status.Runs[0].Error == "" {
		t.Fatalf("skipped cleanup was not reported: %+v", status)
	}
}

func TestDockerCleanupReportsResultPersistenceFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "porto.db")
	server, mux := cleanupServerAtPath(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_cleanup_result BEFORE UPDATE ON docker_cleanup_runs
BEGIN SELECT RAISE(FAIL, 'synthetic result storage failure'); END;`); err != nil {
		t.Fatal(err)
	}
	accepted := httptest.NewRecorder()
	mux.ServeHTTP(accepted, httptest.NewRequest(http.MethodPost, "/api/docker/cleanup", nil))
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("run now = %d: %s", accepted.Code, accepted.Body.String())
	}
	waitForTestCleanup(t, server)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/docker/cleanup", nil))
	var status app.DockerCleanupStatus
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(status.Runs) != 1 || status.Runs[0].Status != app.CleanupFailed ||
		status.Runs[0].Result.Images.ItemsRemoved != 2 || !strings.Contains(status.Runs[0].Error, "synthetic result storage failure") {
		t.Fatalf("result persistence failure was hidden as an ongoing run: %d %s", response.Code, response.Body.String())
	}
	retry := httptest.NewRecorder()
	mux.ServeHTTP(retry, httptest.NewRequest(http.MethodPost, "/api/docker/cleanup", nil))
	if retry.Code != http.StatusServiceUnavailable || !strings.Contains(retry.Body.String(), "storage error") {
		t.Fatalf("cleanup retried without recovering its lost result: %d %s", retry.Code, retry.Body.String())
	}
}
