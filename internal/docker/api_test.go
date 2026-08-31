package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestDockerAPIHandlesVersionedCoreRoutes(t *testing.T) {
	manager := New(&fakeRunner{
		outputs: map[string][]byte{
			"nerdctl version": []byte("nerdctl version 2.1.0\n"),
			"nerdctl ps -a --no-trunc --format {{json .}}":            nil,
			"nerdctl images --digests --no-trunc --format {{json .}}": nil,
		},
		errors: map[string]error{},
	})
	handler := NewAPI(manager, "/tmp/porto.sock")

	ping := httptest.NewRecorder()
	handler.ServeHTTP(ping, httptest.NewRequest(http.MethodGet, "/v1.47/_ping", nil))
	if ping.Code != http.StatusOK || ping.Body.String() != "OK" {
		t.Fatalf("ping = %d %q", ping.Code, ping.Body.String())
	}

	info := httptest.NewRecorder()
	handler.ServeHTTP(info, httptest.NewRequest(http.MethodGet, "/v1.47/info", nil))
	if info.Code != http.StatusOK {
		t.Fatalf("info = %d: %s", info.Code, info.Body.String())
	}
	var document map[string]any
	if err := json.NewDecoder(info.Body).Decode(&document); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	if document["ID"] != "porto" || document["ServerVersion"] == "" {
		t.Fatalf("unexpected info: %+v", document)
	}
}

func TestDockerAPICreatesContainerThroughNativeBackend(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl create --name demo --network bridge --env MODE=test --label app=demo --publish 127.0.0.1:8080:80/tcp alpine:latest sleep 30": []byte("container-id\n"),
		},
		errors: map[string]error{},
	}
	handler := NewAPI(New(runner), "/tmp/porto.sock")
	body := bytes.NewBufferString(`{
		"Image":"alpine:latest",
		"Cmd":["sleep","30"],
		"Env":["MODE=test"],
		"Labels":{"app":"demo"},
		"HostConfig":{
			"NetworkMode":"bridge",
			"PortBindings":{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}
		}
	}`)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1.47/containers/create?name=demo", body))
	if response.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "container-id") {
		t.Fatalf("unexpected create response: %s", response.Body.String())
	}
}

func TestDockerAPIRejectsUnsupportedOperationsExplicitly(t *testing.T) {
	handler := NewAPI(New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}), "/tmp/porto.sock")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1.47/build", nil))
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "does not support") {
		t.Fatalf("unsupported = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerAPIRejectsUnsupportedMountAndFollowSemantics(t *testing.T) {
	handler := NewAPI(New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}), "/tmp/porto.sock")
	create := httptest.NewRecorder()
	handler.ServeHTTP(create, httptest.NewRequest(
		http.MethodPost,
		"/v1.47/containers/create",
		bytes.NewBufferString(`{"Image":"alpine","HostConfig":{"Mounts":[{"Type":"bind","Source":"/tmp","Target":"/data"}]}}`),
	))
	if create.Code != http.StatusNotImplemented || !strings.Contains(create.Body.String(), "Mounts") {
		t.Fatalf("mount response = %d: %s", create.Code, create.Body.String())
	}
	logs := httptest.NewRecorder()
	handler.ServeHTTP(logs, httptest.NewRequest(http.MethodGet, "/v1.47/containers/demo/logs?follow=1&stdout=1&stderr=1", nil))
	if logs.Code != http.StatusNotImplemented || !strings.Contains(logs.Body.String(), "following") {
		t.Fatalf("follow response = %d: %s", logs.Code, logs.Body.String())
	}
}

func TestDockerAPIMultiplexesContainerLogStreams(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl container inspect demo": []byte(`[{"Config":{"Tty":false}}]`),
		},
		errors: map[string]error{},
		ordered: map[string][]runtimes.OutputChunk{
			"nerdctl logs --tail all demo": {
				{Stream: "stdout", Data: []byte("out\n")},
				{Stream: "stderr", Data: []byte("err\n")},
				{Stream: "stdout", Data: []byte("out2\n")},
			},
		},
	}
	handler := NewAPI(New(runner), "/tmp/porto.sock")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet,
		"/v1.47/containers/demo/logs?stdout=1&stderr=1",
		nil,
	))
	if response.Code != http.StatusOK {
		t.Fatalf("logs response = %d: %s", response.Code, response.Body.String())
	}
	data := response.Body.Bytes()
	if len(data) != 37 ||
		data[0] != 1 || string(data[8:12]) != "out\n" ||
		data[12] != 2 || string(data[20:24]) != "err\n" ||
		data[24] != 1 || string(data[32:37]) != "out2\n" {
		t.Fatalf("unexpected multiplexed log frames: %v", data)
	}
}

func TestDockerCLIContextInfoCompatibility(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl version": []byte("nerdctl version 2.1.0\n"),
			"nerdctl ps -a --no-trunc --format {{json .}}":            nil,
			"nerdctl images --digests --no-trunc --format {{json .}}": nil,
			"nerdctl network ls --no-trunc --format {{json .}}":       nil,
			"nerdctl volume ls --format {{json .}}":                   nil,
			"nerdctl create --name compatibility alpine:latest true":  []byte("compatibility-id\n"),
		},
		errors: map[string]error{},
	}
	socketDir, err := os.MkdirTemp("/tmp", "porto-api-*")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(socketPath, NewAPI(New(runner), socketPath))
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})

	configDir := t.TempDir()
	create := exec.Command("docker", "context", "create", "porto", "--docker", "host=unix://"+socketPath)
	create.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	if output, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create Docker context: %v: %s", err, output)
	}
	info := exec.Command("docker", "--context", "porto", "info", "--format", "{{.ServerVersion}}")
	info.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	output, err := info.CombinedOutput()
	if err != nil {
		t.Fatalf("docker context info: %v: %s", err, output)
	}
	if strings.TrimSpace(string(output)) == "" {
		t.Fatal("docker context info returned an empty server version")
	}
	createContainer := exec.Command("docker", "--context", "porto", "create", "--name", "compatibility", "alpine:latest", "true")
	createContainer.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	output, err = createContainer.CombinedOutput()
	if err != nil {
		t.Fatalf("docker context create: %v: %s; backend commands: %+v", err, output, runner.commands)
	}
	if strings.TrimSpace(string(output)) != "compatibility-id" {
		t.Fatalf("unexpected Docker CLI create output: %s", output)
	}
	for _, args := range [][]string{
		{"--context", "porto", "ps", "-a"},
		{"--context", "porto", "images"},
		{"--context", "porto", "network", "ls"},
		{"--context", "porto", "volume", "ls"},
		{"--context", "porto", "start", "compatibility-id"},
		{"--context", "porto", "rm", "--force", "compatibility-id"},
	} {
		command := exec.Command("docker", args...)
		command.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}
}

func TestAPIServerRemovesSocketOnClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket cleanup test")
	}
	socketDir, err := os.MkdirTemp("/tmp", "porto-api-cleanup-*")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	server := NewAPIServer(socketPath, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}}
	response, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = response.Body.Close()
	cancel()
	closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	if err := server.Close(closeContext); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still exists: %v", err)
	}
}

func TestAPIServerRefusesToReplaceActiveSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket protection test")
	}
	socketDir, err := os.MkdirTemp("/tmp", "porto-api-active-*")
	if err != nil {
		t.Fatalf("create socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	first := NewAPIServer(socketPath, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	}))
	if err := first.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start first server: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		closeContext, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = first.Close(closeContext)
	})
	second := NewAPIServer(socketPath, http.NotFoundHandler())
	if err := second.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("expected active socket refusal, got %v", err)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}}
	response, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatalf("first server became unreachable: %v", err)
	}
	_ = response.Body.Close()
}
