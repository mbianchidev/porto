package resourcesaver

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type syntheticBackend struct {
	mu          sync.Mutex
	snapshot    Snapshot
	sleeps      int
	resumes     int
	suspends    int
	engineWakes int
	ready       bool
	sleepError  error
	resumeError error
}

func (b *syntheticBackend) Snapshot(context.Context) (Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshot, nil
}
func (b *syntheticBackend) Sleep(context.Context, Entry) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sleeps++
	return b.sleepError
}
func (b *syntheticBackend) Resume(context.Context, Entry) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resumes++
	return b.resumeError
}
func (b *syntheticBackend) Ready(context.Context, Application) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ready, nil
}
func (b *syntheticBackend) SuspendEngine(context.Context, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.suspends++
	b.snapshot.EngineRunning = false
	return nil
}
func (b *syntheticBackend) WakeEngine(context.Context, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.engineWakes++
	b.snapshot.EngineRunning = true
	return nil
}

func saverFixture(t *testing.T) (*Manager, *syntheticBackend, *atomic.Int64) {
	t.Helper()
	backend := &syntheticBackend{ready: true, snapshot: Snapshot{Complete: true, EngineRunning: true,
		Applications: []Application{{ID: "synthetic-app", Name: "synthetic", Eligible: true,
			Members: []Member{{ID: "synthetic-container", Name: "synthetic", Identity: "synthetic-identity"}}}}}}
	manager, err := New(context.Background(), filepath.Join(t.TempDir(), "saver.json"), backend)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	manager.now = func() time.Time { return time.Unix(0, clock.Load()) }
	configuration := Defaults()
	configuration.Mode, configuration.IdleSeconds, configuration.WakeSeconds = ScaleZero, 1, 1
	if err := manager.Configure(configuration); err != nil {
		t.Fatal(err)
	}
	return manager, backend, clock
}

func waitSaver(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("Resource Saver operation did not reach its expected state")
		case <-ticker.C:
		}
	}
}

func sleepFixture(t *testing.T, manager *Manager, clock *atomic.Int64) {
	t.Helper()
	if err := manager.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(2 * time.Second))
	if err := manager.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitSaver(t, func() bool { return manager.State().Entries["synthetic-app"].Phase == "sleeping" })
}

func TestResourceSaverCoalescesConcurrentWakesAndBoundsRequests(t *testing.T) {
	manager, backend, clock := saverFixture(t)
	configuration := manager.State().Config
	configuration.MaxPending = 8
	configuration.WakeSeconds = 5
	if err := manager.Configure(configuration); err != nil {
		t.Fatal(err)
	}
	sleepFixture(t, manager, clock)
	backend.mu.Lock()
	backend.ready = false
	backend.mu.Unlock()
	results := make(chan error, 8)
	for range 8 {
		go func() {
			release, err := manager.Acquire(context.Background(), "synthetic-app")
			if err == nil {
				release()
			}
			results <- err
		}()
	}
	waitSaver(t, func() bool { return manager.State().Pending == 8 })
	waitSaver(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return backend.resumes == 1
	})
	if _, err := manager.Acquire(context.Background(), "synthetic-app"); err == nil {
		t.Fatal("unbounded wake queue accepted a ninth request")
	}
	backend.mu.Lock()
	if backend.resumes != 1 {
		t.Errorf("concurrent requests ran %d resumes", backend.resumes)
	}
	backend.ready = true
	backend.mu.Unlock()
	for range 8 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if manager.State().Pending != 0 {
		t.Fatal("completed requests leaked queue reservations")
	}
}

func TestResourceSaverCancellationReapsSharedWake(t *testing.T) {
	manager, backend, clock := saverFixture(t)
	sleepFixture(t, manager, clock)
	backend.mu.Lock()
	backend.ready = false
	backend.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := manager.Acquire(ctx, "synthetic-app")
		result <- err
	}()
	waitSaver(t, func() bool { return manager.State().Entries["synthetic-app"].Phase == "waking" })
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled upload returned %v", err)
	}
	waitSaver(t, func() bool { return manager.State().Entries["synthetic-app"].Phase == "failed" })
	if manager.State().Pending != 0 {
		t.Fatal("canceled request leaked a reservation")
	}
}

func TestResourceSaverIncompleteAndBusyInventoriesNeverSuspend(t *testing.T) {
	for _, scenario := range []string{"incomplete", "busy", "not opted in", "excluded", "active request"} {
		t.Run(scenario, func(t *testing.T) {
			manager, backend, clock := saverFixture(t)
			backend.mu.Lock()
			switch scenario {
			case "incomplete":
				backend.snapshot.Complete = false
			case "busy":
				backend.snapshot.Applications[0].Busy = true
			case "not opted in":
				backend.snapshot.Applications[0].Eligible = false
			}
			backend.mu.Unlock()
			if scenario == "excluded" {
				configuration := manager.State().Config
				configuration.Exclusions = []string{"synthetic*"}
				if err := manager.Configure(configuration); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "active request" {
				release, err := manager.Acquire(context.Background(), "synthetic-app")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			for range 3 {
				if err := manager.Tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				clock.Add(int64(10 * time.Second))
			}
			if len(manager.State().Entries) != 0 {
				t.Fatal("unsafe idle detection claimed a workload")
			}
			backend.mu.Lock()
			defer backend.mu.Unlock()
			if backend.sleeps != 0 || backend.suspends != 0 {
				t.Fatalf("unsafe inventory caused actions: sleep=%d engine=%d", backend.sleeps, backend.suspends)
			}
		})
	}
}

func TestResourceSaverResumeFailurePreservesOwnershipAndDiagnostics(t *testing.T) {
	manager, backend, clock := saverFixture(t)
	sleepFixture(t, manager, clock)
	backend.mu.Lock()
	backend.resumeError = errors.New("synthetic readiness or ownership failure")
	backend.mu.Unlock()
	if _, err := manager.Acquire(context.Background(), "synthetic-app"); err == nil {
		t.Fatal("failed resume released an initiating request")
	}
	state := manager.State()
	if state.Entries["synthetic-app"].Phase != "failed" || len(state.Events) < 2 || state.Events[len(state.Events)-1].Action != "wake-failed" {
		t.Fatalf("wake diagnostics were discarded: %+v", state)
	}
}

func TestResourceSaverReadinessDeadlineDoesNotReleaseQueuedTraffic(t *testing.T) {
	manager, backend, clock := saverFixture(t)
	sleepFixture(t, manager, clock)
	backend.mu.Lock()
	backend.ready = false
	backend.mu.Unlock()
	release, err := manager.Acquire(context.Background(), "synthetic-app")
	if release != nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "readiness") {
		t.Fatalf("unready application escaped its wake deadline: release=%t, error=%v", release != nil, err)
	}
	state := manager.State()
	if state.Pending != 0 || state.Entries["synthetic-app"].Phase != "failed" ||
		state.Events[len(state.Events)-1].Action != "wake-failed" {
		t.Fatalf("readiness timeout lost its diagnostics or reservation: %+v", state)
	}
}

type blockedEngineBackend struct {
	*syntheticBackend
	gate chan struct{}
}

func (b *blockedEngineBackend) WakeEngine(ctx context.Context, _ string) error {
	b.mu.Lock()
	b.engineWakes++
	b.mu.Unlock()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-b.gate:
		b.mu.Lock()
		b.snapshot.EngineRunning = true
		b.mu.Unlock()
		return nil
	}
}

func TestResourceSaverSharesEngineWakeAcrossApplicationsAndClientCancellation(t *testing.T) {
	manager, backend, clock := saverFixture(t)
	backend.snapshot.Applications = append(backend.snapshot.Applications, Application{
		ID: "synthetic-other-app", Name: "other", Eligible: true,
		Members: []Member{{ID: "synthetic-other-container", Identity: "synthetic-other-identity"}},
	})
	configuration := manager.State().Config
	configuration.SuspendEngine, configuration.WakeSeconds = true, 5
	if err := manager.Configure(configuration); err != nil {
		t.Fatal(err)
	}
	held := &blockedEngineBackend{syntheticBackend: backend, gate: make(chan struct{})}
	manager.backend = held
	sleepFixture(t, manager, clock)
	waitSaver(t, func() bool { return manager.State().Entries["synthetic-other-app"].Phase == "sleeping" })
	backend.mu.Lock()
	backend.snapshot.Applications = nil
	backend.mu.Unlock()
	if err := manager.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(2 * time.Second))
	if err := manager.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitSaver(t, func() bool {
		state := manager.State()
		return len(state.Events) > 0 && state.Events[len(state.Events)-1].Action == "engine-suspended"
	})
	canceled, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		release, err := manager.Acquire(canceled, "synthetic-app")
		if release != nil {
			release()
		}
		first <- err
	}()
	go func() {
		release, err := manager.Acquire(context.Background(), "synthetic-other-app")
		if release != nil {
			release()
		}
		second <- err
	}()
	waitSaver(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return manager.State().Pending == 2 && backend.engineWakes == 1
	})
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled engine-waiting client returned %v", err)
	}
	close(held.gate)
	if err := <-second; err != nil {
		t.Fatalf("one canceled request aborted another application's shared engine wake: %v", err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.engineWakes != 1 || backend.resumes != 1 || manager.State().Pending != 0 {
		t.Fatalf("engine wake/resume/reservations were not coalesced: wakes=%d resumes=%d pending=%d",
			backend.engineWakes, backend.resumes, manager.State().Pending)
	}
}

func TestResourceSaverRestartNeverAdoptsManualPauses(t *testing.T) {
	manager, backend, clock := saverFixture(t)
	configuration := manager.State().Config
	configuration.Mode = Pause
	if err := manager.Configure(configuration); err != nil {
		t.Fatal(err)
	}
	sleepFixture(t, manager, clock)
	restarted, err := New(context.Background(), manager.path, backend)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close(context.Background())
	if _, err := restarted.Acquire(context.Background(), "synthetic-app"); err == nil {
		t.Fatal("a pause during daemon downtime was claimed automatically")
	}
	if restarted.State().Config.Mode != Pause {
		t.Fatal("restart lost the configured policy")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.resumes != 0 {
		t.Fatal("quarantined pause reached the resume backend")
	}
}

func TestResourceSaverFailedSuspendDoesNotInterceptHealthyTraffic(t *testing.T) {
	manager, backend, clock := saverFixture(t)
	backend.sleepError = errors.New("synthetic active exec")
	if err := manager.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(2 * time.Second))
	if err := manager.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitSaver(t, func() bool {
		state := manager.State()
		return len(state.Events) != 0 && len(state.Entries) == 0
	})
	release, err := manager.Acquire(context.Background(), "synthetic-app")
	if err != nil {
		t.Fatal(err)
	}
	release()
}
