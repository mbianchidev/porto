package operations

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProjectActionsAndQueuedJobsShareAnOwnerAndCancelCleanly(t *testing.T) {
	manager := New(t.TempDir())
	owner, release, err := manager.Reserve(context.Background(), "project:1")
	if err != nil {
		t.Fatal(err)
	}
	if manager.BusyFor(owner, "project:1") || !manager.BusyFor(context.Background(), "project:1") {
		t.Fatal("reservation did not distinguish its exact owner")
	}
	if _, err := manager.Start(context.Background(), "project:1", "compose.deploy", func(context.Context, func(string) error) error { return nil }); !errors.Is(err, ErrBusy) {
		t.Fatal("delivery bypassed a direct project action")
	}
	done := make(chan struct{})
	go func() { defer close(done); <-owner.Done(); release() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestOperationHistoryNeverStoresUnclassifiedToolOutput(t *testing.T) {
	manager := New(t.TempDir())
	value, err := manager.Start(context.Background(), "synthetic:1", "synthetic.action", func(ctx context.Context, progress func(string) error) error {
		if !manager.InJob(ctx, "synthetic:1") || manager.BusyFor(ctx, "synthetic:1") {
			return Failure("Owner context was not propagated.")
		}
		if err := progress("checking synthetic fixture"); err != nil {
			return err
		}
		return errors.New("unclassified synthetic-secret-value")
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := manager.Wait(ctx, value.ID)
	if err != nil || result.Status != "failed" || strings.Contains(result.Error, "synthetic-secret-value") {
		t.Fatalf("unsafe operation failure: %+v %v", result, err)
	}
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
