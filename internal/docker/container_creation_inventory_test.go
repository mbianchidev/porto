package docker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type creationVisibilityRuntime struct {
	*fakeContainerRuntime
	calls   atomic.Int32
	reading chan struct{}
	release chan struct{}
}

func (r *creationVisibilityRuntime) Snapshot(ctx context.Context) ([]Container, error) {
	if r.calls.Add(1) > 1 {
		select {
		case r.reading <- struct{}{}:
		default:
		}
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	return r.fakeContainerRuntime.Snapshot(ctx)
}

type inventoryCreationClient struct{}

func (inventoryCreationClient) Create(context.Context, CreateContainerRequest) (string, error) {
	return "synthetic-front", nil
}

func (inventoryCreationClient) Close() error { return nil }

func TestContainerCreationWaitsForItsOwnInventoryVisibility(t *testing.T) {
	for _, direct := range []bool{false, true} {
		name := "compatibility"
		if direct {
			name = "direct"
		}
		t.Run(name, func(t *testing.T) {
			testContainerCreationInventory(t, direct, false)
		})
	}
}

func TestContainerCreationInventoryCancellationRetainsCreatedIdentity(t *testing.T) {
	testContainerCreationInventory(t, false, true)
}

func testContainerCreationInventory(t *testing.T, direct, cancelCreation bool) {
	t.Helper()
	dependency := Container{ID: "synthetic-dependency", State: "running"}
	front := Container{ID: "synthetic-front", State: "created"}
	client := &creationVisibilityRuntime{
		fakeContainerRuntime: newFakeContainerRuntime([]Container{dependency}, []Container{dependency, front}),
		reading:              make(chan struct{}, 1),
		release:              make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(client.release) }) }
	defer release()
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if len(command.Args) == 0 || command.Args[0] != "create" {
			t.Errorf("unexpected command during creation: %s %s", command.Name, strings.Join(command.Args, " "))
			return nil, errors.New("unexpected synthetic command")
		}
		return []byte("synthetic-front\n"), nil
	}}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.lookPath = func(name string) (string, error) { return name, nil }
	manager.creationConnector = nil
	if direct {
		manager.creationConnector = func(context.Context) (containerCreation, error) {
			return inventoryCreationClient{}, nil
		}
	}
	manager.runtimeConnector = func(context.Context) (containerRuntime, error) { return client, nil }
	inventoryContext, stopInventory := context.WithCancel(context.Background())
	defer stopInventory()
	if err := manager.StartContainerInventory(inventoryContext); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release()
		stopInventory()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.StopContainerInventory(ctx); err != nil {
			t.Error(err)
		}
	})
	waitForInventorySnapshot(t, manager.inventory, func(snapshot ContainerSnapshot) bool {
		return snapshot.Available
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		id  string
		err error
	}
	completed := make(chan result, 1)
	go func() {
		id, err := manager.CreateContainer(ctx, CreateContainerRequest{Image: "synthetic-image"})
		completed <- result{id, err}
	}()
	select {
	case <-client.reading:
	case <-time.After(time.Second):
		t.Fatal("creation did not request a fresh inventory")
	}
	if cancelCreation {
		cancel()
	} else {
		select {
		case returned := <-completed:
			t.Fatalf("creation acknowledged before Compose could list the new container: %+v", returned)
		case <-time.After(25 * time.Millisecond):
		}
		release()
	}
	select {
	case returned := <-completed:
		if returned.id != front.ID || (cancelCreation && !errors.Is(returned.err, context.Canceled)) ||
			(!cancelCreation && returned.err != nil) {
			t.Fatalf("creation result lost its identity or inventory outcome: %+v", returned)
		}
	case <-time.After(time.Second):
		t.Fatal("creation did not settle after inventory completion or caller cancellation")
	}
	if !cancelCreation {
		containers, err := manager.DockerContainers(context.Background(), true)
		if err != nil || len(containers) != 2 || containers[1].ID != front.ID {
			t.Fatalf("immediate Compose list omitted its newly created dependency member: %+v, %v", containers, err)
		}
	}
}
