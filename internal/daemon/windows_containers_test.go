package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/store"
)

type nativeRoundTripper func(*http.Request) (*http.Response, error)

func (f nativeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNativeWindowsResourceSelectionIsExplicitAndPreservesOriginGuards(t *testing.T) {
	for _, selected := range []string{"header", "query", "both", "none", "hostile"} {
		t.Run(selected, func(t *testing.T) {
			native, linux := 0, 0
			server := &Server{windowsTransport: nativeRoundTripper(func(r *http.Request) (*http.Response, error) {
				native++
				if r.Header.Get("X-Porto-Container-Mode") != "" || r.URL.Query().Has("containerMode") ||
					r.URL.Query().Get("tail") != "17" || r.Host != "localhost" {
					t.Fatalf("native proxy boundary = %s %v", r.URL, r.Header)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"backend":"native-fixture"}`))}, nil
			})}
			handler := server.secureHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { linux++; w.WriteHeader(http.StatusNoContent) }))
			url := "http://localhost/api/docker/containers?tail=17"
			if selected == "query" || selected == "both" {
				url += "&containerMode=windows"
			}
			request := httptest.NewRequest(http.MethodGet, url, nil)
			if selected == "header" || selected == "both" || selected == "hostile" {
				request.Header.Set("X-Porto-Container-Mode", "windows")
			}
			if selected == "hostile" {
				request.Header.Set("Origin", "https://foreign.example.test")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			switch selected {
			case "none":
				if native != 0 || linux != 1 {
					t.Fatal("default resource view did not retain Linux")
				}
			case "hostile":
				if response.Code != http.StatusForbidden || native != 0 || linux != 0 {
					t.Fatal("native selection bypassed normal origin guard")
				}
			default:
				if native != 1 || linux != 0 || response.Code != http.StatusOK {
					t.Fatalf("explicit native view = %d native=%d Linux=%d", response.Code, native, linux)
				}
			}
		})
	}
}

func TestNativeWindowsModeConflictDoesNotFallBackToLinux(t *testing.T) {
	server := &Server{}
	called := false
	handler := server.secureHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	request := httptest.NewRequest(http.MethodPost, "http://localhost/api/docker/containers?containerMode=windows", strings.NewReader(`{}`))
	request.Header.Set("X-Porto-Container-Mode", "linux")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || called {
		t.Fatalf("conflicting backend selection reached Linux: %d %v", response.Code, called)
	}
}

func TestNativeWindowsRuntimeViewOnlyReplacesDockerStatus(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "ordinary-user.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings.DockerEnabled, settings.KubernetesEnabled, settings.VMsEnabled = false, false, false
	if err := st.SetSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: st, windowsTransport: nativeRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/docker/status" {
			t.Fatalf("unexpected native path %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"enabled":true,"available":true,"context":"porto-windows","backend":"owned-native"}`))}, nil
	})}
	request := httptest.NewRequest(http.MethodGet, "http://localhost/api/runtime?containerMode=windows", nil)
	response := httptest.NewRecorder()
	server.runtimeStatus(response, request)
	var status struct {
		Docker     portodocker.Status
		Kubernetes struct{ Enabled bool }
		VMs        struct{ Enabled bool }
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Docker.Available || status.Docker.Context != "porto-windows" || status.Kubernetes.Enabled || status.VMs.Enabled {
		t.Fatalf("selected runtime status = %s", response.Body.String())
	}
}

func TestNativeWindowsWorkerRouteGatesNeverExposeUserOrLinuxHelpers(t *testing.T) {
	for _, path := range []string{"/api/docker/containers", "/api/docker/images", "/api/docker/images/namespace%2Ffile", "/api/docker/containers/file-server", "/api/docker/containers/a/logs/stream", "/api/docker/containers/a/terminal", "/api/docker/networks", "/api/docker/volumes"} {
		if !nativeDashboardAllowed(path) {
			t.Errorf("native resource rejected: %s", path)
		}
	}
	for _, path := range []string{"/api/projects", "/api/settings", "/api/kubernetes/status", "/api/vms/instances",
		"/api/docker/engine/install", "/api/docker/builds", "/api/docker/cleanup", "/api/docker/migration/inventory",
		"/api/data/operations", "/api/docker/containers/a/files", "/api/docker/containers/a/file", "/api/docker/volumes/a/storage"} {
		if nativeDashboardAllowed(path) {
			t.Errorf("native worker exposed unsafe route: %s", path)
		}
	}
}

func TestNativeWindowsActionsRequireExplicitConfirmation(t *testing.T) {
	server := &Server{}
	mux := http.NewServeMux()
	server.windowsContainerRoutes(mux)
	for _, action := range []string{"stop", "remove", "context"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, windowsIntegrationPrefix+"/"+action, strings.NewReader(`{"confirm":false}`)))
		if response.Code != http.StatusConflict {
			t.Fatalf("%s without confirmation = %d: %s", action, response.Code, response.Body.String())
		}
	}
}
