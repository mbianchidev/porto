package docker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type fakeRunner struct {
	mu       sync.Mutex
	commands []runtimes.Command
	outputs  map[string][]byte
	errors   map[string]error
}

func (f *fakeRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	key := strings.Join(command.Args, " ")
	return f.outputs[key], f.errors[key]
}

func TestManagerStatusAndInventory(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"context show": []byte("default\n"),
			"context inspect default --format {{json .Endpoints.docker.Host}}": []byte(`"unix:///tmp/docker.sock"`),
			"version --format {{json .}}":                                      []byte(`{"Client":{"Version":"29.0.0"},"Server":{"Version":"29.0.1"}}`),
			"ps -a --no-trunc --format {{json .}}":                             []byte(`{"ID":"abc","Names":"api","Image":"porto/api","State":"running","Status":"Up","Ports":"8080/tcp","Networks":"porto","Mounts":"data","CreatedAt":"now"}` + "\n"),
			"image ls --digests --no-trunc --format {{json .}}":                []byte(`{"ID":"sha256:1","Repository":"porto/api","Tag":"latest","Digest":"sha256:2","Size":"42MB","CreatedAt":"now"}` + "\n"),
		},
		errors: map[string]error{},
	}
	manager := New(runner)

	status := manager.Status(context.Background(), "/tmp/porto.sock")
	if !status.Available || status.Context != "default" || status.ServerVersion != "29.0.1" {
		t.Fatalf("unexpected status: %+v", status)
	}
	containers, err := manager.Containers(context.Background())
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers) != 1 || containers[0].Name != "api" || containers[0].Networks != "porto" {
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

func TestContainerActionRejectsUnsupportedAction(t *testing.T) {
	err := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}).
		ContainerAction(context.Background(), "container", "explode")
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported action error, got %v", err)
	}
}

func TestManagerReportsCommandFailure(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{"context show": []byte("daemon offline")},
		errors:  map[string]error{"context show": errors.New("exit 1")},
	}
	status := New(runner).Status(context.Background(), "")
	if status.Available || !strings.Contains(status.Message, "daemon offline") {
		t.Fatalf("unexpected unavailable status: %+v", status)
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
	state, err := ActivateEndpoint(canonical, target, "unix://"+previous, statePath, true)
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

func TestActivateRejectsCanonicalSocketAsUpstream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket endpoint test")
	}
	dir, err := os.MkdirTemp("/tmp", "porto-docker-loop-*")
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
	_, err = ActivateEndpoint(canonical, target, "unix://"+canonical, filepath.Join(dir, "state.json"), false)
	if err == nil || !strings.Contains(err.Error(), "distinct upstream") {
		t.Fatalf("expected recursive upstream rejection, got %v", err)
	}
	if _, statErr := os.Lstat(canonical); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("canonical endpoint was modified: %v", statErr)
	}
}

func TestProxyForwardsDockerAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket proxy test")
	}
	dir, err := os.MkdirTemp("/tmp", "porto-proxy-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	upstreamPath := filepath.Join(dir, "upstream.sock")
	upstreamListener, err := net.Listen("unix", upstreamPath)
	if err != nil {
		t.Fatalf("listen upstream: %v", err)
	}
	upstreamServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_ping" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("OK"))
	})}
	go upstreamServer.Serve(upstreamListener)
	defer upstreamServer.Close()

	proxyPath := filepath.Join(dir, "porto.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxy := NewProxy(proxyPath, "unix://"+upstreamPath)
	if err := proxy.Start(ctx); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer proxy.Close(context.Background())

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", proxyPath)
		},
	}}
	client.Timeout = 2 * time.Second
	response, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("get proxy: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %s", response.Status)
	}
}
