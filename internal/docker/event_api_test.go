package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDockerEventsReplaysFilteredContainerHistory(t *testing.T) {
	now := time.Now().UTC()
	inventory := newContainerInventory(
		func(context.Context) (containerRuntime, error) { return nil, nil },
		defaultInventoryOptions(),
	)
	inventory.snapshot = ContainerSnapshot{
		Available: true,
		Containers: []Container{{
			ID: "container-id", Name: "demo", Image: "alpine:latest",
			Labels: map[string]string{"team": "platform"},
		}},
		Events: []ContainerLifecycleEvent{
			{Sequence: 1, Type: "container-create", ContainerID: "container-id", Timestamp: now.Add(-3 * time.Minute)},
			{Sequence: 2, Type: "task-start", ContainerID: "container-id", Timestamp: now.Add(-2 * time.Minute)},
			{Sequence: 3, Type: "task-exit", ContainerID: "container-id", Timestamp: now.Add(-time.Minute)},
		},
	}
	manager := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}})
	manager.inventory = inventory
	manager.inventoryCancel = func() {}
	filters := url.QueryEscape(`{"type":{"container":true},"event":{"start":true},"container":{"demo":true},"label":{"team=platform":true}}`)
	request := httptest.NewRequest(
		http.MethodGet,
		"/v1.47/events?since=0&until="+url.QueryEscape(now.Add(-30*time.Second).Format(time.RFC3339Nano))+"&filters="+filters,
		nil,
	)
	response := httptest.NewRecorder()

	NewAPI(manager, "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("events = %d: %s", response.Code, response.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(response.Body.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("event lines = %d: %s", len(lines), response.Body.String())
	}
	var event struct {
		Type   string `json:"Type"`
		Action string `json:"Action"`
		Actor  struct {
			ID         string            `json:"ID"`
			Attributes map[string]string `json:"Attributes"`
		} `json:"Actor"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if event.Type != "container" || event.Action != "start" || event.Actor.ID != "container-id" ||
		event.Actor.Attributes["name"] != "demo" || event.Actor.Attributes["image"] != "alpine:latest" {
		t.Fatalf("unexpected event: %+v", event)
	}
	if response.Header().Get("X-Porto-Event-Replay-Limit") != "200" {
		t.Fatalf("replay limit header = %q", response.Header().Get("X-Porto-Event-Replay-Limit"))
	}
}

func TestDockerEventsUnsubscribesWhenClientDisconnects(t *testing.T) {
	inventory := newContainerInventory(
		func(context.Context) (containerRuntime, error) { return nil, nil },
		defaultInventoryOptions(),
	)
	inventory.snapshot = ContainerSnapshot{Available: true}
	manager := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}})
	manager.inventory = inventory
	manager.inventoryCancel = func() {}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/v1.47/events", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		NewAPI(manager, "/tmp/porto.sock").ServeHTTP(response, request)
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		inventory.mu.RLock()
		subscribers := len(inventory.subscribers)
		inventory.mu.RUnlock()
		if subscribers == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("event client did not subscribe")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("event stream did not stop after disconnect")
	}
	inventory.mu.RLock()
	defer inventory.mu.RUnlock()
	if len(inventory.subscribers) != 0 {
		t.Fatalf("event subscribers leaked: %d", len(inventory.subscribers))
	}
}

func TestDockerEventsRejectsInvalidTimeBounds(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1.47/events?since=tomorrow", nil)
	response := httptest.NewRecorder()
	NewAPI(New(&fakeRunner{}), "/tmp/porto.sock").ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("events = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerCLIEventsConsumesFilteredReplay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	now := time.Now().UTC()
	inventory := newContainerInventory(
		func(context.Context) (containerRuntime, error) { return nil, nil },
		defaultInventoryOptions(),
	)
	inventory.snapshot = ContainerSnapshot{
		Available:  true,
		Containers: []Container{{ID: "container-id", Name: "demo", Image: "alpine:latest"}},
		Events: []ContainerLifecycleEvent{{
			Sequence: 1, Type: "task-start", ContainerID: "container-id", Timestamp: now.Add(-time.Minute),
		}},
	}
	manager := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}})
	manager.inventory = inventory
	manager.inventoryCancel = func() {}
	socketDir, err := os.MkdirTemp("/tmp", "porto-events-cli-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(socketPath, NewAPI(manager, socketPath))
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})
	configDir := t.TempDir()
	createContext := exec.Command("docker", "context", "create", "porto", "--docker", "host=unix://"+socketPath)
	createContext.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := createContext.CombinedOutput(); err != nil {
		t.Fatalf("create Docker context: %v: %s", err, output)
	}
	command := exec.Command(
		"docker", "--context", "porto", "events",
		"--since", "0", "--until", strconv.FormatInt(now.Unix(), 10),
		"--filter", "event=start", "--format", "{{json .}}",
	)
	command.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker events: %v: %s", err, output)
	}
	if !strings.Contains(string(output), `"Action":"start"`) ||
		!strings.Contains(string(output), `"ID":"container-id"`) {
		t.Fatalf("unexpected docker events output: %s", output)
	}
}
