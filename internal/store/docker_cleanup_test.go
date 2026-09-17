package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
)

func TestDockerCleanupScheduleAndResultsPersist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "porto.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	status, err := st.DockerCleanupStatus(ctx)
	if err != nil || status.Enabled || status.NextRunAt != "" || len(status.Runs) != 0 {
		t.Fatalf("initial cleanup status = %+v, %v", status, err)
	}
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	run, err := st.BeginDockerCleanup(ctx, app.CleanupManual, now)
	if err != nil || run == nil {
		t.Fatalf("manual cleanup should not require a schedule: %+v, %v", run, err)
	}
	result := app.NewDockerCleanupResult()
	result.Images = app.DockerCleanupStep{Status: app.CleanupSucceeded, ItemsRemoved: 2, Output: "Removed synthetic image references"}
	reclaimed := int64(4096)
	result.BuildCache = app.DockerCleanupStep{Status: app.CleanupSucceeded, ItemsRemoved: 3, BytesReclaimed: &reclaimed}
	if err := st.FinishDockerCleanup(ctx, run.ID, app.CleanupSucceeded, result, "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	status, err = st.DockerCleanupStatus(ctx)
	if err != nil || len(status.Runs) != 1 || !reflect.DeepEqual(status.Runs[0].Result, result) {
		t.Fatalf("cleanup result did not survive reopening: %+v, %v", status, err)
	}
	if status.Runs[0].Trigger != app.CleanupManual || status.Runs[0].Status != app.CleanupSucceeded || status.NextRunAt != "" {
		t.Fatalf("unexpected manual cleanup metadata: %+v", status)
	}

	settings, err := st.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.DockerAutoPruneEnabled = true
	before := time.Now().UTC()
	if err := st.SetSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	status, err = st.DockerCleanupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	due, err := time.Parse(time.RFC3339Nano, status.NextRunAt)
	if err != nil || due.Before(before.Add(app.DockerCleanupInterval)) || due.After(time.Now().Add(app.DockerCleanupInterval)) {
		t.Fatalf("first cleanup must be one week after opt-in: %q, %v", status.NextRunAt, err)
	}
	if run, err := st.BeginDockerCleanup(ctx, app.CleanupScheduled, due.Add(-time.Nanosecond)); err != nil || run != nil {
		t.Fatalf("cleanup ran before its weekly deadline: %+v, %v", run, err)
	}
	run, err = st.BeginDockerCleanup(ctx, app.CleanupScheduled, due)
	if err != nil || run == nil {
		t.Fatalf("due cleanup did not start: %+v, %v", run, err)
	}
	if _, err := st.BeginDockerCleanup(ctx, app.CleanupManual, due); !errors.Is(err, ErrDockerCleanupRunning) {
		t.Fatalf("overlapping cleanup = %v, want conflict", err)
	}
	result.Images.Status = app.CleanupFailed
	result.Images.Error = "synthetic image deletion failed"
	finished := due.Add(time.Minute)
	if err := st.FinishDockerCleanup(ctx, run.ID, app.CleanupFailed, result, result.Images.Error, finished); err != nil {
		t.Fatal(err)
	}
	status, err = st.DockerCleanupStatus(ctx)
	if err != nil || len(status.Runs) != 2 {
		t.Fatalf("cleanup history = %+v, %v", status, err)
	}
	if status.Runs[0].Trigger != app.CleanupScheduled || status.Runs[0].Error != result.Images.Error ||
		status.Runs[0].Result.BuildCache.ItemsRemoved != 3 ||
		status.NextRunAt != finished.Add(app.DockerCleanupInterval).Format(time.RFC3339Nano) {
		t.Fatalf("scheduled failure lost partial results or changed the weekly cadence: %+v", status)
	}
	settings.DockerAutoPruneEnabled = false
	if err := st.SetSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	status, err = st.DockerCleanupStatus(ctx)
	if err != nil || status.Enabled || status.NextRunAt != "" || len(status.Runs) != 2 {
		t.Fatalf("disabling cleanup must preserve results but clear the schedule: %+v, %v", status, err)
	}
}

func TestDockerCleanupSettingChangesDoNotResetExistingSchedule(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	settings, err := st.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.DockerAutoPruneEnabled = true
	if err := st.SetSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	before, err := st.DockerCleanupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings.ReduceMotion = true
	if err := st.SetSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	after, err := st.DockerCleanupStatus(ctx)
	if err != nil || before.NextRunAt != after.NextRunAt {
		t.Fatalf("unrelated preference save reset cleanup: before=%+v, after=%+v, error=%v", before, after, err)
	}
}

func TestDockerCleanupClaimsSerialize(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := st.BeginDockerCleanup(context.Background(), app.CleanupManual, time.Now())
			results <- err
		})
	}
	wg.Wait()
	first, second := <-results, <-results
	if !((first == nil && errors.Is(second, ErrDockerCleanupRunning)) || (second == nil && errors.Is(first, ErrDockerCleanupRunning))) {
		t.Fatalf("cleanup claims = %v, %v; want one successful claim", first, second)
	}
}

func TestDockerCleanupRecoversInterruptedRunWithoutHidingIt(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	run, err := st.BeginDockerCleanup(ctx, app.CleanupManual, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecoverDockerCleanup(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	status, err := st.DockerCleanupStatus(ctx)
	if err != nil || status.Runs[0].Status != app.CleanupInterrupted || status.Runs[0].Error == "" {
		t.Fatalf("interrupted run was not reported: %+v, %v", status, err)
	}
	if err := st.FinishDockerCleanup(ctx, run.ID, app.CleanupSucceeded, app.NewDockerCleanupResult(), "", now); err == nil {
		t.Fatal("stale worker overwrote the interrupted result")
	}
	if next, err := st.BeginDockerCleanup(ctx, app.CleanupManual, now.Add(2*time.Minute)); err != nil || next == nil || next.ID <= run.ID {
		t.Fatalf("recovered cleanup could not be retried: %+v, %v", next, err)
	}
}
