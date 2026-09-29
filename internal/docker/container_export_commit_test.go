package docker

import (
	"bytes"
	"context"
	"errors"
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

func TestDockerAPIExportsContainerFilesystem(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.starter = func(command runtimes.Command) (runtimes.Process, error) {
		if strings.Join(command.Args, " ") != "container export demo" {
			return nil, errors.New("unexpected export command")
		}
		return newFakeProcess(func([]byte) ([]byte, []byte, error) {
			return []byte("synthetic-container-tar"), nil, nil
		}), nil
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1.47/containers/demo/export", nil)

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "synthetic-container-tar" {
		t.Fatalf("export = %d: %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/x-tar" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
}

func TestDockerAPICommitsContainerImage(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		for _, expected := range []string{
			"commit", "--author Porto", "--message snapshot", "--pause=false",
			"--change CMD [\"sleep\",\"30\"]", "demo", "example/app:v1",
		} {
			if !strings.Contains(joined, expected) {
				return nil, errors.New("missing commit arguments: " + joined)
			}
		}
		return []byte("sha256:committed\n"), nil
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		`/v1.47/commit?container=demo&repo=example%2Fapp&tag=v1&author=Porto&comment=snapshot&pause=false&changes=CMD+%5B%22sleep%22%2C%2230%22%5D`,
		bytes.NewReader(nil),
	)

	NewAPI(New(runner), "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), "sha256:committed") {
		t.Fatalf("commit = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerCLIContainerExportAndCommitCompatibility(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if len(command.Args) == 0 || command.Args[0] != "commit" {
			return nil, errors.New("unexpected commit command: " + strings.Join(command.Args, " "))
		}
		return []byte("sha256:committed\n"), nil
	}
	runner.starter = func(command runtimes.Command) (runtimes.Process, error) {
		if strings.Join(command.Args, " ") != "container export demo" {
			return nil, errors.New("unexpected export command")
		}
		return newFakeProcess(func([]byte) ([]byte, []byte, error) {
			return []byte("synthetic-container-tar"), nil, nil
		}), nil
	}
	socketDir, err := os.MkdirTemp("/tmp", "porto-export-commit-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(socketPath, NewAPI(New(runner), socketPath))
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
	exportPath := filepath.Join(t.TempDir(), "container.tar")
	for _, args := range [][]string{
		{"--context", "porto", "export", "--output", exportPath, "demo"},
		{"--context", "porto", "commit", "--author", "Porto", "--message", "snapshot", "demo", "example/app:v1"},
	} {
		command := exec.Command("docker", args...)
		command.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}
	data, err := os.ReadFile(exportPath)
	if err != nil || string(data) != "synthetic-container-tar" {
		t.Fatalf("exported archive = %q, %v", data, err)
	}
}
