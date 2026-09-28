package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestDockerCLIImageLoadTagAndPushCompatibility(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch joined {
		case "tag alpine:latest docker.io/example/app:v1", "tag alpine:latest example/app:v1":
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected image command: %v", command.Args)
		}
	}
	runner.streamer = func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		switch {
		case len(command.Args) > 0 && command.Args[0] == "load":
			data, err := io.ReadAll(command.StdinReader)
			if err != nil || string(data) != "synthetic-docker-archive" {
				return nil, errors.New("Docker CLI did not upload the image archive")
			}
			return nil, emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("Loaded image: example/app:v1\n")})
		case len(command.Args) > 0 && command.Args[0] == "push":
			return nil, emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("pushed example/app:v1\n")})
		default:
			return nil, fmt.Errorf("unexpected streaming image command: %v", command.Args)
		}
	}
	socketDir, err := os.MkdirTemp("/tmp", "porto-image-mutations-*")
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
	auth := base64.StdEncoding.EncodeToString([]byte("user:token"))
	config, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			"https://index.docker.io/v1/": map[string]string{"auth": auth},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(archivePath, []byte("synthetic-docker-archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--context", "porto", "tag", "alpine:latest", "example/app:v1"},
		{"--context", "porto", "load", "--input", archivePath},
		{"--context", "porto", "push", "example/app:v1"},
	} {
		command := exec.Command("docker", args...)
		command.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}
}
