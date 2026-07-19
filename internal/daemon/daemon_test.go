package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/killswitch"
	"github.com/mbianchidev/porto/internal/store"
)

type killSwitchRunner struct {
	mu       sync.Mutex
	syncArgs chan []string
}

func (r *killSwitchRunner) LookPath(name string) (string, error) {
	if name == "killswitchctl" {
		return "/Users/test/bin/killswitchctl", nil
	}
	return "", errors.New("not found")
}

func (r *killSwitchRunner) Run(_ context.Context, _ string, args ...string) (killswitch.CommandOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case len(args) > 1 && args[1] == "sync-ports":
		r.syncArgs <- append([]string(nil), args...)
		return killswitch.CommandOutput{
			Stdout: []byte(`{"version":"1.4.0","autoKillEnabled":true,"userPorts":[3000],"integrationPorts":{"porto":[41001]},"effectivePorts":[3000,41001]}`),
		}, nil
	case len(args) > 1 && args[1] == "cleanup":
		return killswitch.CommandOutput{
			Stdout: []byte(`{"version":"1.4.0","autoKillEnabled":true,"candidateCount":1,"killedCount":1,"killedProcesses":[{"pid":123,"command":"node vite","runtime":"node","ageHours":14}]}`),
		}, nil
	default:
		return killswitch.CommandOutput{}, errors.New("unexpected command")
	}
}

type noopKillSwitchInstaller struct{}

func (noopKillSwitchInstaller) Install(context.Context) error { return nil }

func TestKillSwitchRoutesSyncActivePortsAndRunCleanup(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	projectPath := t.TempDir()
	id, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "web",
		Path:     projectPath,
		Strategy: "package.json",
		Command:  "npm run dev",
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.SetRuntime(context.Background(), id, "running", 123, 41001); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	settings.KillSwitchEnabled = true
	if err := st.SetSettings(context.Background(), settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	runner := &killSwitchRunner{syncArgs: make(chan []string, 1)}
	server := New(st, nil)
	server.killSwitch = killswitch.NewManager(runner, noopKillSwitchInstaller{})
	server.running[id] = &exec.Cmd{}
	mux := http.NewServeMux()
	server.routes(mux)

	syncResponse := httptest.NewRecorder()
	mux.ServeHTTP(syncResponse, httptest.NewRequest(http.MethodPost, "/api/integrations/kill-switch/sync", nil))
	if syncResponse.Code != http.StatusAccepted {
		t.Fatalf("sync status = %d, body = %s", syncResponse.Code, syncResponse.Body.String())
	}
	select {
	case args := <-runner.syncArgs:
		want := []string{"dev-cleanup", "sync-ports", "--source", "porto", "--ports", "41001", "--json"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("sync args = %q, want %q", args, want)
		}
	case <-time.After(time.Second):
		t.Fatal("sync command did not run")
	}
	waitForKillSwitchReady(t, server.killSwitch)

	cleanupResponse := httptest.NewRecorder()
	mux.ServeHTTP(cleanupResponse, httptest.NewRequest(http.MethodPost, "/api/integrations/kill-switch/cleanup", nil))
	if cleanupResponse.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d, body = %s", cleanupResponse.Code, cleanupResponse.Body.String())
	}
	var result killswitch.CleanupResult
	if err := json.NewDecoder(cleanupResponse.Body).Decode(&result); err != nil {
		t.Fatalf("decode cleanup: %v", err)
	}
	if result.CandidateCount != 1 || result.KilledCount != 1 || len(result.KilledProcesses) != 1 || result.KilledProcesses[0].PID != 123 {
		t.Fatalf("cleanup result = %+v", result)
	}
}

func waitForKillSwitchReady(t *testing.T, manager *killswitch.Manager) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if manager.Snapshot().State == "ready" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("KillSwitch status = %+v", manager.Snapshot())
}

func TestLogRoutesFilterAndClearStreams(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	id, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "app",
		Path:     t.TempDir(),
		Strategy: "package",
		Command:  "npm run dev",
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.AddLog(context.Background(), id, "stdout", "ready"); err != nil {
		t.Fatalf("add stdout: %v", err)
	}
	if err := st.AddLog(context.Background(), id, "stderr", "warning"); err != nil {
		t.Fatalf("add stderr: %v", err)
	}

	mux := http.NewServeMux()
	New(st, nil).routes(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/app/logs?stream=stdout", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get logs status = %d: %s", response.Code, response.Body.String())
	}
	var lines []app.LogLine
	if err := json.NewDecoder(response.Body).Decode(&lines); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	if len(lines) != 1 || lines[0].Stream != "stdout" || lines[0].Line != "ready" {
		t.Fatalf("stdout logs = %+v", lines)
	}

	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/app/logs/clear?stream=stderr", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("clear logs status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Deleted int64 `json:"deleted"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode clear response: %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", result.Deleted)
	}
}

func TestProxyUsesConfiguredHostname(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("proxied"))
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	_, rawPort, err := net.SplitHostPort(backendURL.Host)
	if err != nil {
		t.Fatalf("parse backend address: %v", err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	id, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "application",
		Hostname: "custom-app",
		Path:     t.TempDir(),
		Strategy: "package",
		Command:  "npm run dev",
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.SetRuntime(context.Background(), id, "running", 123, port); err != nil {
		t.Fatalf("set runtime: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "https://custom-app.porto.local/", nil)
	request.Host = "custom-app.porto.local:37681"
	response := httptest.NewRecorder()
	New(st, nil).proxyByHost(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "proxied" {
		t.Fatalf("proxy response = %d %q", response.Code, response.Body.String())
	}
}

func TestLocalHostnameSupportsTLSAndLegacyDomains(t *testing.T) {
	for host, want := range map[string]string{
		"app.porto.local:37681":     "app",
		"app.porto.localhost:37680": "app",
		"PORTO.LOCAL.":              "",
	} {
		got, ok := localHostname(host)
		if !ok || got != want {
			t.Fatalf("localHostname(%q) = %q, %t; want %q, true", host, got, ok, want)
		}
	}
	if _, ok := localHostname("example.com"); ok {
		t.Fatal("non-Porto hostname was accepted")
	}
}
