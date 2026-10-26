package resourcesaver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCallerReadinessDeadlineRetainsLastProbeAndCause(t *testing.T) {
	manager, backend, clock := saverFixture(t)
	sleepFixture(t, manager, clock)
	backend.mu.Lock()
	backend.ready = false
	backend.readyError = ReadinessPendingError{Reason: "synthetic HTTP readiness returned 503 Service Unavailable"}
	backend.mu.Unlock()
	configuration := manager.State().Config
	configuration.WakeSeconds = 30
	if err := manager.Configure(configuration); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	release, err := manager.Acquire(ctx, "synthetic-app")
	if release != nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "503 Service Unavailable") {
		t.Fatalf("caller timeout lost the actual pending readiness failure: release=%t, error=%v", release != nil, err)
	}
	waitSaver(t, func() bool { return manager.State().Entries["synthetic-app"].Phase == "failed" })
	state := manager.State()
	if state.Pending != 0 || !strings.Contains(state.Entries["synthetic-app"].Error, "503 Service Unavailable") ||
		!strings.Contains(state.Entries["synthetic-app"].Error, "deadline exceeded") {
		t.Fatalf("timeout diagnostics or reservations were lost after the response: %+v", state)
	}
}
