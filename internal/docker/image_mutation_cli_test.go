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
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestImageRemovalUsesStorageBudgetWithoutExtendingCallerDeadlines(t *testing.T) {
	for _, scenario := range []struct {
		name                     string
		configured, caller, want time.Duration
	}{
		{name: "storage cleanup", configured: defaultTimeout, want: imageTransferTimeout},
		{name: "longer configured operation", configured: 45 * time.Minute, want: 45 * time.Minute},
		{name: "caller deadline", configured: defaultTimeout, caller: 2 * time.Second, want: 2 * time.Second},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var remaining time.Duration
			runner := lifecycleDeadlineRunner(func(ctx context.Context, command runtimes.Command) ([]byte, error) {
				if strings.Join(command.Args, " ") != "image rm --force synthetic:latest" {
					return nil, fmt.Errorf("unexpected image-removal command: %+v", command)
				}
				deadline, ok := ctx.Deadline()
				if !ok {
					return nil, errors.New("image removal has no bounded storage deadline")
				}
				remaining = time.Until(deadline)
				return nil, nil
			})
			manager := New(runner)
			manager.timeout = scenario.configured
			ctx := context.Background()
			if scenario.caller > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, scenario.caller)
				defer cancel()
			}
			if err := manager.RemoveImage(ctx, "synthetic:latest", true); err != nil {
				t.Fatal(err)
			}
			if remaining <= scenario.want-time.Second || remaining > scenario.want {
				t.Fatalf("image removal budget = %s, want %s with caller deadline preserved", remaining, scenario.want)
			}
		})
	}
}

func TestDockerCLIImageLoadTagAndPushCompatibility(t *testing.T) {
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
	socketPath := dockerTestEndpoint(t)
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
	createContext := exec.Command("docker", "context", "create", "porto", "--docker", "host="+EndpointURL(socketPath))
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
