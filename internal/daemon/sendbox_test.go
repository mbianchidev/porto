//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/sendbox"
	"github.com/mbianchidev/porto/internal/store"
)

type fakeSendboxIntegration struct{}

func (fakeSendboxIntegration) Status([]app.Project) sendbox.Status {
	return sendbox.Status{State: "ready", Message: "ready", UpdatedAt: time.Now().UTC()}
}

func (fakeSendboxIntegration) Command(ctx context.Context, project app.Project) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	return process.Command(ctx, project.Path, "/bin/sh", "-c", `trap 'exit 0' TERM; echo ready; while :; do sleep 1; done`)
}

func TestSendboxLifecycleDoesNotOverwriteProjectRuntime(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, sendbox.ConfigFile), []byte("name: app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "app",
		Path:     root,
		Strategy: "package",
		Command:  "npm run dev",
	}); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	settings.SendboxEnabled = true
	if err := st.SetSettings(context.Background(), settings); err != nil {
		t.Fatalf("enable Sendbox: %v", err)
	}

	server := New(st, nil)
	server.sendbox = fakeSendboxIntegration{}
	mux := http.NewServeMux()
	server.routes(mux)

	startResponse := httptest.NewRecorder()
	mux.ServeHTTP(startResponse, httptest.NewRequest(http.MethodPost, "/api/projects/app/sendbox/start", nil))
	if startResponse.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", startResponse.Code, startResponse.Body.String())
	}
	var started app.Project
	if err := json.NewDecoder(startResponse.Body).Decode(&started); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if started.SendboxStatus != "running" {
		t.Fatalf("Sendbox status = %q", started.SendboxStatus)
	}

	persisted, err := st.GetProject(context.Background(), "app")
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if persisted.Status != "stopped" || persisted.PID != 0 || persisted.Port != 0 {
		t.Fatalf("normal runtime was overwritten: %+v", persisted)
	}

	stopResponse := httptest.NewRecorder()
	mux.ServeHTTP(stopResponse, httptest.NewRequest(http.MethodPost, "/api/projects/app/sendbox/stop", nil))
	if stopResponse.Code != http.StatusOK {
		t.Fatalf("stop status = %d: %s", stopResponse.Code, stopResponse.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		server.mu.Lock()
		_, running := server.sendboxRunning[persisted.ID]
		state := server.sendboxStates[persisted.ID]
		server.mu.Unlock()
		if !running {
			if state != "stopped" {
				t.Fatalf("final Sendbox state = %q", state)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Sendbox process did not stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
