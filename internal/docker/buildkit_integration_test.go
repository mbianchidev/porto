package docker

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/process"
)

func TestDockerMultiPlatformBuildIntegration(t *testing.T) {
	if os.Getenv("PORTO_DOCKER_MULTIPLATFORM_INTEGRATION") != "1" {
		t.Skip("set PORTO_DOCKER_MULTIPLATFORM_INTEGRATION=1 to provision and test a running Porto engine")
	}
	if runtime.GOOS == "windows" {
		t.Skip("this live Docker CLI test uses a host Unix socket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	manager := New(nil)
	state, err := manager.readEngineState()
	if err != nil || state.Mode != "lima" || state.Instance != engineInstanceName {
		t.Fatalf("this test requires an existing Porto-owned Lima engine: %v", err)
	}
	exists, running, err := manager.limaInstanceStatus(ctx)
	if err != nil || !exists || !running {
		t.Fatalf("this test requires an already running engine: exists=%t, running=%t, error=%v", exists, running, err)
	}
	if err := manager.StartEngine(ctx); err != nil {
		t.Fatalf("reconcile multi-platform support without restarting the VM: %v", err)
	}
	if missing, err := manager.limaBuildKitMissingPlatforms(ctx, false); err != nil || len(missing) != 0 {
		t.Fatalf("BuildKit platform listing remains incomplete: missing=%v, error=%v", missing, err)
	}

	socketDirectory, err := os.MkdirTemp("/tmp", "porto-buildx-integration-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDirectory); err != nil {
			t.Errorf("remove integration socket directory: %v", err)
		}
	})
	socketPath := filepath.Join(socketDirectory, "docker.sock")
	server := NewAPIServer(socketPath, NewAPI(manager, socketPath))
	if err := server.Start(ctx); err != nil {
		t.Fatalf("start test Docker API: %v", err)
	}
	t.Cleanup(func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := server.Close(closeContext); err != nil {
			t.Errorf("close test Docker API: %v", err)
		}
	})

	buildDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(buildDirectory, "Dockerfile"), []byte(`FROM alpine:latest AS probe
ARG TARGETPLATFORM
RUN uname -m > /architecture && printf '%s\n' "$TARGETPLATFORM" > /target-platform
FROM scratch
COPY --from=probe /architecture /target-platform /
`), 0o600); err != nil {
		t.Fatal(err)
	}
	expected := map[string][]string{
		"linux/amd64":   {"x86_64"},
		"linux/arm64":   {"aarch64"},
		"linux/arm/v6":  {"armv6l", "armv7l", "armv8l"},
		"linux/arm/v7":  {"armv7l", "armv8l"},
		"linux/386":     {"i386", "i686"},
		"linux/ppc64le": {"ppc64le"},
		"linux/riscv64": {"riscv64"},
		"linux/s390x":   {"s390x"},
	}
	targets := slices.Sorted(maps.Keys(expected))
	outputDirectory := t.TempDir()
	command := process.NewCommand(ctx, buildDirectory, "docker",
		"buildx", "build", "--builder", "default",
		"--no-cache", "--progress=plain",
		"--platform", strings.Join(targets, ","),
		"--output", "type=local,dest="+outputDirectory,
		".",
	)
	command.Env = process.WithEnvironment(os.Environ(),
		"DOCKER_CONTEXT=",
		"DOCKER_HOST="+EndpointURL(socketPath),
		"BUILDX_CONFIG="+t.TempDir(),
		"BUILDX_BUILDER=default",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("multi-platform Docker build: %v: %s", err, output)
	}
	for _, target := range targets {
		directory := filepath.Join(outputDirectory, strings.ReplaceAll(target, "/", "_"))
		platform, err := os.ReadFile(filepath.Join(directory, "target-platform"))
		if err != nil || strings.TrimSpace(string(platform)) != target {
			t.Fatalf("exported target for %s = %q, error=%v", target, platform, err)
		}
		architecture, err := os.ReadFile(filepath.Join(directory, "architecture"))
		if err != nil || !slices.Contains(expected[target], strings.TrimSpace(string(architecture))) {
			t.Fatalf("executed architecture for %s = %q, error=%v", target, architecture, err)
		}
		t.Logf("%s executed as %s", target, strings.TrimSpace(string(architecture)))
	}
}
