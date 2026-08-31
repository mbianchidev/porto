package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type fakeRunner struct {
	mu       sync.Mutex
	commands []runtimes.Command
	outputs  map[string][]byte
	errors   map[string]error
	ordered  map[string][]runtimes.OutputChunk
	handler  func(runtimes.Command) ([]byte, error)
	streamer func(runtimes.Command, func(runtimes.OutputChunk) error) ([]byte, error)
	starter  func(runtimes.Command) (runtimes.Process, error)
}

type engineInstallRunner struct {
	mu       sync.Mutex
	created  bool
	ownerID  string
	commands []runtimes.Command
}

func workingBuildKitDialer(context.Context) (net.Conn, error) {
	connection, peer := net.Pipe()
	_ = peer.Close()
	return connection, nil
}

func (f *fakeRunner) RunStreaming(
	_ context.Context,
	command runtimes.Command,
	emit func(runtimes.OutputChunk) error,
) ([]byte, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	if f.streamer != nil {
		f.mu.Unlock()
		return f.streamer(command, emit)
	}
	key := command.Name + " " + strings.Join(command.Args, " ")
	chunks := append([]runtimes.OutputChunk(nil), f.ordered[key]...)
	runErr := f.errors[key]
	f.mu.Unlock()
	var output []byte
	for _, chunk := range chunks {
		output = append(output, chunk.Data...)
		if err := emit(chunk); err != nil {
			return output, err
		}
	}
	return output, runErr
}

func (r *engineInstallRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, command)
	switch command.Name {
	case "/usr/local/bin/nerdctl":
		return []byte("containerd unavailable"), errors.New("exit 1")
	case "limactl":
		joined := strings.Join(command.Args, " ")
		switch {
		case joined == "list --json" && !r.created:
			return nil, nil
		case joined == "list --json" && r.created:
			return []byte(`{"name":"porto-engine","status":"Running"}` + "\n"), nil
		case strings.HasPrefix(joined, "start --tty=false"):
			r.created = true
			return nil, nil
		case joined == `shell porto-engine -- sh -c umask 077; cat > "$HOME/.porto-engine-owner"`:
			r.ownerID = strings.TrimSpace(string(command.Stdin))
			return nil, nil
		case joined == `shell porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`:
			return []byte(r.ownerID + "\n"), nil
		case joined == "shell porto-engine -- nerdctl version":
			return []byte("nerdctl version 2.1.0\n"), nil
		}
	}
	return nil, fmt.Errorf("unexpected command: %s %s", command.Name, strings.Join(command.Args, " "))
}

func (f *fakeRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	if f.handler != nil {
		return f.handler(command)
	}
	key := command.Name + " " + strings.Join(command.Args, " ")
	return f.outputs[key], f.errors[key]
}

func (f *fakeRunner) Start(_ context.Context, command runtimes.Command) (runtimes.Process, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	starter := f.starter
	f.mu.Unlock()
	if starter == nil {
		return nil, fmt.Errorf("unexpected streaming command: %s %s", command.Name, strings.Join(command.Args, " "))
	}
	return starter(command)
}

func TestManagerStatusAndInventory(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl version": []byte("nerdctl version 2.1.0\n"),
			"nerdctl ps -a --no-trunc --format {{json .}}":            []byte(`{"ID":"abc","Names":"api","Image":"porto/api","State":"running","Status":"Up","Ports":"8080/tcp","Networks":"porto","Mounts":"data","CreatedAt":"2026-08-31T12:00:00Z","Labels":"com.docker.compose.project=porto"}` + "\n"),
			"nerdctl images --digests --no-trunc --format {{json .}}": []byte(`{"ID":"sha256:1","Repository":"porto/api","Tag":"latest","Digest":"sha256:2","Size":"42MB","CreatedAt":"2026-08-31T12:00:00Z"}` + "\n"),
		},
		errors: map[string]error{},
	}

	manager := New(runner)

	status := manager.Status(context.Background(), "/tmp/porto.sock")
	if !status.Available || status.Context != "porto" || status.Endpoint != "unix:///tmp/porto.sock" {
		t.Fatalf("unexpected status: %+v", status)
	}
	containers, err := manager.Containers(context.Background())
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers) != 1 || containers[0].Name != "api" || containers[0].ComposeProject != "porto" {
		t.Fatalf("unexpected containers: %+v", containers)
	}
	images, err := manager.Images(context.Background())
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	if len(images) != 1 || images[0].Digest != "sha256:2" {
		t.Fatalf("unexpected images: %+v", images)
	}
}

func TestNormalizeNerdctlReferenceDropsTagBeforeDigest(t *testing.T) {
	got := normalizeNerdctlReference("kindest/node:v1.37.0@sha256:abcdef")
	if got != "kindest/node@sha256:abcdef" {
		t.Fatalf("normalized reference = %q", got)
	}
}

func TestContainerActionRejectsUnsupportedAction(t *testing.T) {
	err := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}).
		ContainerAction(context.Background(), "container", "explode")
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported action error, got %v", err)
	}
}

func TestManagerReportsMissingNativeRuntime(t *testing.T) {
	manager := NewWithStateDir(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}, t.TempDir())
	manager.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	status := manager.Status(context.Background(), "")
	if status.Available || !strings.Contains(status.Message, "engine-install") {
		t.Fatalf("unexpected unavailable status: %+v", status)
	}
}

func TestInstallDirectEnginePersistsState(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{"nerdctl version": []byte("nerdctl version 2.1.0\n")},
		errors:  map[string]error{},
	}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.dialBuildKit = workingBuildKitDialer
	manager.lookPath = func(name string) (string, error) {
		if name == "nerdctl" {
			return "/usr/local/bin/nerdctl", nil
		}
		return "", errors.New("not found")
	}
	status, err := manager.InstallEngine(context.Background())
	if err != nil {
		t.Fatalf("install engine: %v", err)
	}
	if !status.Available {
		t.Fatalf("engine unavailable after install: %+v", status)
	}
	state, err := manager.readEngineState()
	if err != nil {
		t.Fatalf("read engine state: %v", err)
	}
	if state.Mode != "direct" {
		t.Fatalf("engine mode = %q, want direct", state.Mode)
	}
}

func TestInstallEngineFallsBackToWritableLimaBackend(t *testing.T) {
	runner := &engineInstallRunner{}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.goos = "darwin"
	manager.dialBuildKit = workingBuildKitDialer
	manager.lookPath = func(name string) (string, error) {
		switch name {
		case "nerdctl":
			return "/usr/local/bin/nerdctl", nil
		case "limactl":
			return "/usr/local/bin/limactl", nil
		default:
			return "", errors.New("not found")
		}
	}
	status, err := manager.InstallEngine(context.Background())
	if err != nil {
		t.Fatalf("install engine: %v", err)
	}
	if !status.Available || !strings.Contains(status.Backend, "Lima") {
		t.Fatalf("unexpected engine status: %+v", status)
	}
	foundWritableMount := false
	for _, command := range runner.commands {
		if command.Name == "limactl" && strings.Contains(strings.Join(command.Args, " "), "--mount-writable") {
			foundWritableMount = true
			break
		}
	}
	if !foundWritableMount {
		t.Fatalf("Lima creation did not request writable mounts: %+v", runner.commands)
	}
}

func TestInstallEngineRejectsUnownedLimaNameCollision(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"limactl list --json": []byte(`{"name":"porto-engine","status":"Running"}` + "\n"),
		},
		errors: map[string]error{},
	}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.goos = "darwin"
	manager.lookPath = func(name string) (string, error) {
		if name == "limactl" {
			return "/usr/local/bin/limactl", nil
		}
		return "", errors.New("not found")
	}
	if _, err := manager.InstallEngine(context.Background()); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected ownership collision, got %v", err)
	}
}

func TestActivateAndDeactivateEndpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket endpoint test")
	}
	dir, err := os.MkdirTemp("/tmp", "porto-docker-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	target := filepath.Join(dir, "porto.sock")
	listener, err := net.Listen("unix", target)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	canonical := filepath.Join(dir, "docker.sock")
	previous := filepath.Join(dir, "previous.sock")
	if err := os.Symlink(previous, canonical); err != nil {
		t.Fatalf("create previous link: %v", err)
	}
	statePath := filepath.Join(dir, "state.json")
	state, err := ActivateEndpoint(canonical, target, statePath, true)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if state.PreviousLink != previous {
		t.Fatalf("expected previous link %q, got %q", previous, state.PreviousLink)
	}
	activeTarget, err := os.Readlink(canonical)
	if err != nil || activeTarget != target {
		t.Fatalf("unexpected active link %q: %v", activeTarget, err)
	}
	status := AddEndpointStatus(Status{ProxySocket: target}, canonical, statePath)
	if !status.Canonical || status.PreviousLink != previous {
		t.Fatalf("unexpected endpoint status: %+v", status)
	}
	if err := DeactivateEndpoint(statePath); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	restored, err := os.Readlink(canonical)
	if err != nil || restored != previous {
		t.Fatalf("unexpected restored link %q: %v", restored, err)
	}
}

func TestActivateAllowsCanonicalEndpointWithoutUpstream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket endpoint test")
	}
	dir, err := os.MkdirTemp("/tmp", "porto-native-endpoint-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	target := filepath.Join(dir, "porto.sock")
	listener, err := net.Listen("unix", target)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	canonical := filepath.Join(dir, "docker.sock")
	if _, err := ActivateEndpoint(canonical, target, filepath.Join(dir, "state.json"), false); err != nil {
		t.Fatalf("activate native engine endpoint: %v", err)
	}
}
