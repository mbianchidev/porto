package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestContainerReplacementWaitsForInventoryVisibility(t *testing.T) {
	for _, action := range []string{"remove", "remove-force", "remove-volumes", "remove-force-volumes", "rename"} {
		for _, direct := range []bool{false, true} {
			for _, byName := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/direct=%t/name=%t", action, direct, byName), func(t *testing.T) {
					testContainerMutationInventory(t, action, direct, byName, false)
				})
			}
		}
	}
}

func TestContainerReplacementInventoryCancellationIsReported(t *testing.T) {
	for _, action := range []string{"remove", "rename"} {
		t.Run(action, func(t *testing.T) {
			testContainerMutationInventory(t, action, true, true, true)
		})
	}
}

func TestContainerReplacementFailureDoesNotWaitForInventory(t *testing.T) {
	for _, action := range []string{"remove", "rename"} {
		t.Run(action, func(t *testing.T) {
			call := "delete synthetic-old false false"
			if action == "rename" {
				call = "rename synthetic-old synthetic-app"
			}
			manager := managerWithContainerOperations(&fakeContainerOperations{
				errs: map[string]error{call: ErrConflict},
			})
			manager.inventory = newContainerInventory(nil, defaultInventoryOptions())
			manager.inventory.snapshot.Available = true
			manager.inventory.snapshot.Containers = []Container{{ID: "synthetic-old", Name: "synthetic-temporary"}}
			manager.inventoryCancel = func() {}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var err error
			if action == "rename" {
				err = manager.RenameContainer(ctx, "synthetic-old", "synthetic-app")
			} else {
				err = manager.ContainerAction(ctx, "synthetic-old", action)
			}
			if !errors.Is(err, ErrConflict) || len(manager.inventory.refresh) != 0 {
				t.Fatalf("failed runtime mutation was replaced with inventory confirmation: %v", err)
			}
		})
	}
}

func testContainerMutationInventory(t *testing.T, action string, direct, byName, cancelMutation bool) {
	t.Helper()
	old := Container{ID: "synthetic-old", Name: "synthetic-app", State: "exited"}
	replacement := Container{ID: "synthetic-new", Name: "synthetic-temporary", State: "created"}
	initial := []Container{old, replacement}
	expected := []Container{replacement}
	target := old
	if action == "rename" {
		initial = []Container{replacement}
		target = replacement
		expected[0].Name = "synthetic-app"
	}
	client := &creationVisibilityRuntime{
		fakeContainerRuntime: newFakeContainerRuntime(initial, expected),
		reading:              make(chan struct{}, 1),
		release:              make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(client.release) }) }
	defer release()
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if len(command.Args) == 3 && command.Args[0] == "container" && command.Args[1] == "inspect" {
			return fmt.Appendf(nil, `[{"Id":%q,"Name":%q}]`, target.ID, "/"+target.Name), nil
		}
		if len(command.Args) == 0 || (command.Args[0] != "rm" && command.Args[0] != "rename") {
			t.Errorf("unexpected command during replacement: %s %s", command.Name, strings.Join(command.Args, " "))
			return nil, errors.New("unexpected synthetic command")
		}
		return nil, nil
	}}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.lookPath = func(name string) (string, error) { return name, nil }
	manager.operationsConnector = func(context.Context) (containerOperations, error) {
		if !direct {
			return nil, ErrUnsupported
		}
		return &fakeContainerOperations{}, nil
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
	identifier := target.ID
	if byName {
		identifier = target.Name
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		if action == "rename" {
			completed <- manager.RenameContainer(ctx, identifier, "synthetic-app")
		} else {
			completed <- manager.ContainerAction(ctx, identifier, action)
		}
	}()
	select {
	case <-client.reading:
	case <-time.After(time.Second):
		t.Fatal("replacement did not request a fresh inventory")
	}
	if cancelMutation {
		cancel()
	} else {
		select {
		case err := <-completed:
			t.Fatalf("replacement acknowledged before Compose could list its committed result: %v", err)
		case <-time.After(25 * time.Millisecond):
		}
		release()
	}
	select {
	case err := <-completed:
		if cancelMutation {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("committed replacement hid inventory cancellation: %v", err)
			}
		} else if err != nil {
			t.Fatalf("replacement confirmation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement did not settle after inventory completion or caller cancellation")
	}
	if !cancelMutation {
		containers, err := manager.DockerContainers(context.Background(), true)
		if err != nil || len(containers) != 1 || containers[0].ID != expected[0].ID ||
			containers[0].Name != expected[0].Name {
			t.Fatalf("immediate Compose list retained a removed or temporary container: %+v, %v", containers, err)
		}
	}
}
