package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/kubernetes"
)

func TestAllowedShell(t *testing.T) {
	for _, shell := range []string{"sh", "bash", "ash", "/bin/sh", "/bin/bash", "/bin/ash"} {
		if !allowedShell(shell) {
			t.Errorf("expected %q to be allowed", shell)
		}
	}
	for _, shell := range []string{"zsh", "sh -c id", "../../bin/sh", ""} {
		if allowedShell(shell) {
			t.Errorf("expected %q to be rejected", shell)
		}
	}
}

func TestContainerTerminalStateUsesUnwrappedInspectDocument(t *testing.T) {
	ready, err := containerTerminalReady(json.RawMessage(`{"State":{"Running":true,"Paused":false}}`))
	if err != nil {
		t.Fatalf("decode container terminal state: %v", err)
	}
	if !ready {
		t.Fatal("running container was not terminal-ready")
	}
}

func TestK9sTerminalCommandScopesManagedCluster(t *testing.T) {
	command := k9sTerminalCommand(context.Background(), kubernetes.Cluster{
		Context:        "porto-dev",
		KubeconfigPath: "/tmp/dev.yaml",
	})
	want := []string{
		"k9s",
		"--kubeconfig", "/tmp/dev.yaml",
		"--context", "porto-dev",
		"--all-namespaces",
	}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("command args = %q, want %q", command.Args, want)
	}
	if !slices.Contains(command.Env, "KUBECONFIG=/tmp/dev.yaml") {
		t.Fatalf("command environment does not contain managed kubeconfig: %q", command.Env)
	}
	if !slices.Contains(command.Env, "TERM=xterm-256color") || !slices.Contains(command.Env, "COLORTERM=truecolor") {
		t.Fatalf("command environment does not contain terminal capabilities: %q", command.Env)
	}
}

func TestDiveTerminalCommandTargetsPortoSocket(t *testing.T) {
	command := diveTerminalCommand(context.Background(), "/opt/porto/dive", "example/app:v1", "/tmp/porto docker.sock")
	want := []string{"/opt/porto/dive", "example/app:v1", "--source", "docker"}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("command args = %q, want %q", command.Args, want)
	}
	for _, expected := range []string{
		"DOCKER_HOST=" + portodocker.EndpointURL("/tmp/porto docker.sock"),
		"DOCKER_CONTEXT=",
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	} {
		if !slices.Contains(command.Env, expected) {
			t.Fatalf("command environment missing %q: %q", expected, command.Env)
		}
	}
}

func TestDockerImageDiveTerminalRejectsUnsafeImageReference(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/docker/images/invalid/dive", nil)
	request.SetPathValue("id", "-invalid")
	response := httptest.NewRecorder()

	(&Server{}).dockerImageDiveTerminal(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestDockerImageDiveTerminalReportsMissingDive(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	request := httptest.NewRequest(http.MethodGet, "/api/docker/images/alpine/dive", nil)
	request.SetPathValue("id", "alpine:latest")
	response := httptest.NewRecorder()

	(&Server{}).dockerImageDiveTerminal(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestDiveTerminalRoutePreservesSlashedImageReference(t *testing.T) {
	mux := http.NewServeMux()
	var image string
	mux.HandleFunc("GET /api/docker/images/{id}/dive", func(w http.ResponseWriter, r *http.Request) {
		image = r.PathValue("id")
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/docker/images/example%2Fapp%3Av1/dive", nil)
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if image != "example/app:v1" {
		t.Fatalf("image = %q, want %q", image, "example/app:v1")
	}
}

func TestVMTerminalCommandUsesInteractiveLimaShell(t *testing.T) {
	command := vmTerminalCommand(context.Background(), "test-vm")
	want := []string{
		"limactl", "shell", "--tty=true", "test-vm", "--",
		"sh", "-lc", `cd "$HOME" && exec env PS1="$1 $ " sh -i`, "porto-shell", "test-vm",
	}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("command args = %q, want %q", command.Args, want)
	}
	if !slices.Contains(command.Env, "TERM=xterm-256color") || !slices.Contains(command.Env, "COLORTERM=truecolor") {
		t.Fatalf("command environment does not contain terminal capabilities: %q", command.Env)
	}
}

func TestPodTerminalCommandUsesKubectlExec(t *testing.T) {
	args := []string{
		"--context", "porto-dev",
		"exec", "--stdin", "--tty", "--namespace", "default", "api",
		"--container", "app", "--",
	}
	args = append(args, podTerminalShellCommand("sh")...)
	command := podTerminalCommand(context.Background(), args)
	want := []string{
		"kubectl",
		"--context", "porto-dev",
		"exec", "--stdin", "--tty", "--namespace", "default", "api",
		"--container", "app", "--",
		"sh", "-c", `TERM=xterm-256color COLORTERM=truecolor exec "$0" -i`, "sh",
	}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("command args = %q, want %q", command.Args, want)
	}
	if !slices.Contains(command.Env, "TERM=xterm-256color") || !slices.Contains(command.Env, "COLORTERM=truecolor") {
		t.Fatalf("command environment does not contain terminal capabilities: %q", command.Env)
	}
}

func TestPodTerminalShellDoesNotRequireEnvExecutable(t *testing.T) {
	want := []string{"ash", "-c", `TERM=xterm-256color COLORTERM=truecolor exec "$0" -i`, "ash"}
	if got := podTerminalShellCommand("ash"); !reflect.DeepEqual(got, want) {
		t.Fatalf("pod terminal shell command = %q, want %q", got, want)
	}
}
