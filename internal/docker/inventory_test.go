package docker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeContainerRuntime struct {
	namespace string
	backend   string
	events    chan ContainerLifecycleEvent
	errs      chan error

	mu             sync.Mutex
	subscribed     bool
	snapshotCalls  int
	snapshots      [][]Container
	snapshotErrors []error
	closed         bool
}

func newFakeContainerRuntime(snapshots ...[]Container) *fakeContainerRuntime {
	return &fakeContainerRuntime{
		namespace: "default",
		backend:   "test containerd",
		events:    make(chan ContainerLifecycleEvent, 10),
		errs:      make(chan error, 1),
		snapshots: snapshots,
	}
}

func (f *fakeContainerRuntime) Namespace() string { return f.namespace }
func (f *fakeContainerRuntime) Backend() string   { return f.backend }

func (f *fakeContainerRuntime) Snapshot(context.Context) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.subscribed {
		return nil, errors.New("snapshot happened before subscription")
	}
	index := f.snapshotCalls
	f.snapshotCalls++
	if index < len(f.snapshotErrors) && f.snapshotErrors[index] != nil {
		return nil, f.snapshotErrors[index]
	}
	if len(f.snapshots) == 0 {
		return []Container{}, nil
	}
	if index >= len(f.snapshots) {
		index = len(f.snapshots) - 1
	}
	return append([]Container(nil), f.snapshots[index]...), nil
}

func (f *fakeContainerRuntime) Subscribe(context.Context) (<-chan ContainerLifecycleEvent, <-chan error) {
	f.mu.Lock()
	f.subscribed = true
	f.mu.Unlock()
	return f.events, f.errs
}

func (f *fakeContainerRuntime) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func TestContainerInventorySubscribesBeforeSnapshotAndRefreshesOnEvent(t *testing.T) {
	runtimeClient := newFakeContainerRuntime(
		[]Container{{ID: "one", Name: "api", State: "created"}},
		[]Container{{ID: "one", Name: "api", State: "running"}},
	)
	inventory := newContainerInventory(
		func(context.Context) (containerRuntime, error) { return runtimeClient, nil },
		inventoryOptions{
			debounce:          5 * time.Millisecond,
			reconcileInterval: time.Hour,
			connectBackoff:    time.Millisecond,
			maxBackoff:        time.Millisecond,
			operationTimeout:  time.Second,
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		inventory.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	initial := waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Available && snapshot.Revision > 0
	})
	if initial.Containers[0].State != "created" {
		t.Fatalf("initial state = %q, want created", initial.Containers[0].State)
	}

	runtimeClient.events <- ContainerLifecycleEvent{
		Topic:       "/tasks/start",
		Type:        "task-start",
		ContainerID: "one",
		Timestamp:   time.Now().UTC(),
	}
	updated := waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Revision > initial.Revision && snapshot.Containers[0].State == "running"
	})
	if len(updated.Containers[0].History) != 1 || updated.Containers[0].History[0].Type != "task-start" {
		t.Fatalf("unexpected lifecycle history: %+v", updated.Containers[0].History)
	}
	runtimeClient.mu.Lock()
	defer runtimeClient.mu.Unlock()
	if runtimeClient.snapshotCalls != 2 {
		t.Fatalf("snapshot calls = %d, want 2", runtimeClient.snapshotCalls)
	}
}

func TestContainerInventoryCoalescesDuplicateEvents(t *testing.T) {
	runtimeClient := newFakeContainerRuntime(
		[]Container{{ID: "one", Name: "api", State: "running"}},
		[]Container{{ID: "one", Name: "api", State: "exited"}},
	)
	inventory := newContainerInventory(
		func(context.Context) (containerRuntime, error) { return runtimeClient, nil },
		inventoryOptions{
			debounce:          10 * time.Millisecond,
			reconcileInterval: time.Hour,
			connectBackoff:    time.Millisecond,
			maxBackoff:        time.Millisecond,
			operationTimeout:  time.Second,
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		inventory.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	initial := waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Available
	})
	exitCode := uint32(137)
	exitSignal := uint32(9)
	event := ContainerLifecycleEvent{
		Topic:       "/tasks/exit",
		Type:        "task-exit",
		ContainerID: "one",
		Timestamp:   time.Now().UTC(),
		ExitCode:    &exitCode,
		ExitSignal:  &exitSignal,
		Reason:      "signal",
	}
	runtimeClient.events <- event
	runtimeClient.events <- event
	updated := waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Revision > initial.Revision
	})
	if len(updated.Events) != 1 {
		t.Fatalf("events = %d, want one deduplicated event", len(updated.Events))
	}
	if updated.Containers[0].ExitSignal == nil || *updated.Containers[0].ExitSignal != 9 {
		t.Fatalf("unexpected exit signal: %+v", updated.Containers[0].ExitSignal)
	}
}

func TestContainerInventoryPreservesStaleSnapshotAcrossReconnect(t *testing.T) {
	first := newFakeContainerRuntime([]Container{{ID: "one", Name: "api", State: "running"}})
	second := newFakeContainerRuntime([]Container{{ID: "one", Name: "api", State: "exited"}})
	var connectCalls int
	inventory := newContainerInventory(
		func(context.Context) (containerRuntime, error) {
			connectCalls++
			switch connectCalls {
			case 1:
				return first, nil
			case 2:
				return nil, errors.New("temporary socket failure")
			default:
				return second, nil
			}
		},
		inventoryOptions{
			debounce:          time.Millisecond,
			reconcileInterval: time.Hour,
			connectBackoff:    20 * time.Millisecond,
			maxBackoff:        20 * time.Millisecond,
			operationTimeout:  time.Second,
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		inventory.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	initial := waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Available && snapshot.Containers[0].State == "running"
	})
	first.errs <- errors.New("stream lost")
	stale := waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Revision > initial.Revision && snapshot.Stale && !snapshot.Available
	})
	if len(stale.Containers) != 1 || stale.Containers[0].State != "running" {
		t.Fatalf("stale snapshot was not preserved: %+v", stale)
	}
	reconnected := waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Available && snapshot.Containers[0].State == "exited"
	})
	if reconnected.Stale || connectCalls < 3 {
		t.Fatalf("unexpected reconnect state: %+v, calls=%d", reconnected, connectCalls)
	}
}

func TestContainerInventoryWaitCapturesRapidExit(t *testing.T) {
	runtimeClient := newFakeContainerRuntime(
		[]Container{{ID: "one", Name: "api", State: "created"}},
		[]Container{{ID: "one", Name: "api", State: "running"}},
	)
	inventory := newContainerInventory(
		func(context.Context) (containerRuntime, error) { return runtimeClient, nil },
		inventoryOptions{
			debounce:          5 * time.Millisecond,
			reconcileInterval: time.Hour,
			connectBackoff:    time.Millisecond,
			maxBackoff:        time.Millisecond,
			operationTimeout:  time.Second,
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		inventory.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Available
	})
	result := make(chan int, 1)
	errs := make(chan error, 1)
	go func() {
		code, err := inventory.wait(ctx, "one", "next-exit")
		result <- code
		errs <- err
	}()
	waitForInventorySubscriber(t, inventory)
	exitCode := uint32(7)
	runtimeClient.events <- ContainerLifecycleEvent{
		Topic:       "/tasks/exit",
		Type:        "task-exit",
		ContainerID: "one",
		Timestamp:   time.Now().UTC(),
		ExitCode:    &exitCode,
		Reason:      "error",
	}
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if code := <-result; code != 7 {
			t.Fatalf("exit code = %d, want 7", code)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lifecycle exit")
	}
}

func TestContainerInventoryWaitAllowsNewContainerDiscovery(t *testing.T) {
	runtimeClient := newFakeContainerRuntime(
		[]Container{{ID: "existing", Name: "worker", State: "running", TaskPresent: true}},
		[]Container{
			{ID: "existing", Name: "worker", State: "running", TaskPresent: true},
			{ID: "one", Name: "api", State: "running", TaskPresent: true},
		},
	)
	inventory := newContainerInventory(
		func(context.Context) (containerRuntime, error) { return runtimeClient, nil },
		inventoryOptions{
			debounce:          5 * time.Millisecond,
			reconcileInterval: time.Hour,
			connectBackoff:    time.Millisecond,
			maxBackoff:        time.Millisecond,
			operationTimeout:  time.Second,
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		inventory.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Available && len(snapshot.Containers) == 1
	})
	result := make(chan int, 1)
	errs := make(chan error, 1)
	go func() {
		code, err := inventory.wait(ctx, "one", "next-exit")
		result <- code
		errs <- err
	}()
	waitForInventorySubscriber(t, inventory)
	inventory.triggerRefresh()
	waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return len(snapshot.Containers) == 2
	})
	exitCode := uint32(0)
	runtimeClient.events <- ContainerLifecycleEvent{
		Topic:       "/tasks/exit",
		Type:        "task-exit",
		ContainerID: "one",
		Timestamp:   time.Now().UTC(),
		ExitCode:    &exitCode,
	}
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if code := <-result; code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for newly discovered container")
	}
}

func TestContainerInventoryWaitCapturesExitDuringDiscovery(t *testing.T) {
	exitCode := uint32(9)
	runtimeClient := newFakeContainerRuntime(
		[]Container{{ID: "existing", Name: "worker", State: "running", TaskPresent: true}},
		[]Container{
			{ID: "existing", Name: "worker", State: "running", TaskPresent: true},
			{ID: "one", Name: "job", State: "exited", ExitCode: &exitCode},
		},
	)
	inventory := newContainerInventory(
		func(context.Context) (containerRuntime, error) { return runtimeClient, nil },
		inventoryOptions{
			debounce:          5 * time.Millisecond,
			reconcileInterval: time.Hour,
			connectBackoff:    time.Millisecond,
			maxBackoff:        time.Millisecond,
			operationTimeout:  time.Second,
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		inventory.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Available && len(snapshot.Containers) == 1
	})
	result := make(chan int, 1)
	errs := make(chan error, 1)
	go func() {
		code, err := inventory.wait(ctx, "one", "next-exit")
		result <- code
		errs <- err
	}()
	waitForInventorySubscriber(t, inventory)
	runtimeClient.events <- ContainerLifecycleEvent{
		Topic:       "/tasks/exit",
		Type:        "task-exit",
		ContainerID: "one",
		Timestamp:   time.Now().UTC(),
		ExitCode:    &exitCode,
	}
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if code := <-result; code != 9 {
			t.Fatalf("exit code = %d, want 9", code)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for exit during discovery")
	}
}

func TestContainerInventoryWaitReconcilesPartialExit(t *testing.T) {
	exitCode := uint32(17)
	runtimeClient := newFakeContainerRuntime(
		[]Container{{ID: "one", Name: "api", State: "running"}},
		[]Container{{ID: "one", Name: "api", State: "exited", ExitCode: &exitCode}},
	)
	inventory := newContainerInventory(
		func(context.Context) (containerRuntime, error) { return runtimeClient, nil },
		inventoryOptions{
			debounce:          5 * time.Millisecond,
			reconcileInterval: time.Hour,
			connectBackoff:    time.Millisecond,
			maxBackoff:        time.Millisecond,
			operationTimeout:  time.Second,
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		inventory.run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Available
	})
	result := make(chan int, 1)
	errs := make(chan error, 1)
	go func() {
		code, err := inventory.wait(ctx, "one", "next-exit")
		result <- code
		errs <- err
	}()
	waitForInventorySubscriber(t, inventory)
	runtimeClient.events <- ContainerLifecycleEvent{
		Topic:       "/tasks/exit",
		Type:        "task-exit",
		ContainerID: "one",
		Timestamp:   time.Now().UTC(),
		Reason:      "partial event payload",
	}
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		if code := <-result; code != 17 {
			t.Fatalf("exit code = %d, want reconciled code 17", code)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconciled lifecycle exit")
	}
}

func TestManagerUsesContainerInventoryWithoutNerdctlPolling(t *testing.T) {
	runtimeClient := newFakeContainerRuntime(
		[]Container{{ID: "one", Name: "api", State: "running"}},
		[]Container{{ID: "one", Name: "api", State: "exited"}},
	)
	runner := &fakeRunner{
		outputs: map[string][]byte{"nerdctl stop one": nil},
		errors:  map[string]error{},
	}
	manager := New(runner)
	manager.runtimeConnector = func(context.Context) (containerRuntime, error) {
		return runtimeClient, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.StartContainerInventory(ctx); err != nil {
		t.Fatalf("start inventory: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stopContext, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		if err := manager.StopContainerInventory(stopContext); err != nil {
			t.Fatalf("stop inventory: %v", err)
		}
	})
	initial := waitForInventorySnapshot(t, manager.inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Available
	})
	for range 3 {
		containers, err := manager.Containers(ctx)
		if err != nil {
			t.Fatalf("list containers: %v", err)
		}
		if len(containers) != 1 || containers[0].ID != "one" {
			t.Fatalf("unexpected containers: %+v", containers)
		}
	}
	runtimeClient.mu.Lock()
	if runtimeClient.snapshotCalls != 1 {
		t.Fatalf("snapshot calls after cached reads = %d, want 1", runtimeClient.snapshotCalls)
	}
	runtimeClient.mu.Unlock()
	runner.mu.Lock()
	if len(runner.commands) != 0 {
		t.Fatalf("cached reads spawned commands: %+v", runner.commands)
	}
	runner.mu.Unlock()

	if err := manager.ContainerAction(ctx, "one", "stop"); err != nil {
		t.Fatalf("stop container: %v", err)
	}
	updated := waitForInventorySnapshot(t, manager.inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Revision > initial.Revision && snapshot.Containers[0].State == "exited"
	})
	if updated.Stale {
		t.Fatalf("action refresh produced stale snapshot: %+v", updated)
	}
	status := manager.Status(ctx, "/tmp/porto.sock")
	if !status.Available || status.Inventory != "containerd-events" || status.Revision != updated.Revision {
		t.Fatalf("unexpected inventory status: %+v", status)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 1 || strings.Join(runner.commands[0].Args, " ") != "stop one" {
		t.Fatalf("unexpected action commands: %+v", runner.commands)
	}
}

func TestContainerInventoryClosesSubscribersOnStop(t *testing.T) {
	runtimeClient := newFakeContainerRuntime([]Container{{ID: "one", Name: "api", State: "running"}})
	inventory := newContainerInventory(
		func(context.Context) (containerRuntime, error) { return runtimeClient, nil },
		inventoryOptions{
			debounce:          time.Millisecond,
			reconcileInterval: time.Hour,
			connectBackoff:    time.Millisecond,
			maxBackoff:        time.Millisecond,
			operationTimeout:  time.Second,
		},
	)
	updates, unsubscribe := inventory.subscribe()
	defer unsubscribe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		inventory.run(ctx)
		close(done)
	}()
	waitForInventorySnapshot(t, inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Available
	})
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("inventory did not stop")
	}
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-updates:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("inventory subscriber stayed open after stop")
		}
	}
}

func TestContainerInventoryUsesUniqueInstanceIDs(t *testing.T) {
	first := newContainerInventory(nil, defaultInventoryOptions()).snapshotValue().InstanceID
	second := newContainerInventory(nil, defaultInventoryOptions()).snapshotValue().InstanceID
	if first == "" || second == "" || first == second {
		t.Fatalf("inventory instance IDs are not unique: %q %q", first, second)
	}
}

func waitForInventorySnapshot(
	t *testing.T,
	inventory *containerInventory,
	condition func(ContainerSnapshot) bool,
) ContainerSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := inventory.snapshotValue()
		if len(snapshot.Containers) > 0 && condition(snapshot) {
			return snapshot
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for inventory snapshot: %+v", inventory.snapshotValue())
	return ContainerSnapshot{}
}

func waitForInventorySubscriber(t *testing.T, inventory *containerInventory) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		inventory.mu.RLock()
		count := len(inventory.subscribers)
		inventory.mu.RUnlock()
		if count > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for inventory subscriber")
}
