package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
)

func dockerTestEndpoint(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `\\.\pipe\porto-client-test-` + newInventoryInstanceID()
	}
	directory, err := os.MkdirTemp("/tmp", "porto-client-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(directory); err != nil {
			t.Errorf("remove test endpoint directory: %v", err)
		}
	})
	return filepath.Join(directory, "docker.sock")
}

func startDockerTestAPI(t *testing.T, manager *Manager) string {
	t.Helper()
	endpoint := dockerTestEndpoint(t)
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(endpoint, NewAPI(manager, endpoint))
	if err := server.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		ctx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := server.Close(ctx); err != nil {
			t.Errorf("close test API endpoint: %v", err)
		}
	})
	return EndpointURL(endpoint)
}

func dockerSDKClient(t *testing.T, host string, options ...dockerclient.Opt) *dockerclient.Client {
	t.Helper()
	options = append([]dockerclient.Opt{dockerclient.WithHost(host)}, options...)
	client, err := dockerclient.New(options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close Docker SDK client: %v", err)
		}
	})
	return client
}

func TestDockerSDKNegotiatesAndCreatesWithDefaultOptions(t *testing.T) {
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		switch command.Args[0] {
		case "create":
			return []byte("synthetic-sdk-container\n"), nil
		case "container":
			return []byte(`[{"Id":"synthetic-sdk-container","Name":"/synthetic-sdk","State":{"Status":"created"},"Config":{"Image":"synthetic:latest"}}]`), nil
		default:
			return nil, fmt.Errorf("unexpected SDK backend command: %+v", command)
		}
	}}
	client := dockerSDKClient(t, startDockerTestAPI(t, New(runner)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	created, err := client.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name:       "synthetic-sdk",
		Config:     &container.Config{Image: "synthetic:latest", Cmd: []string{"sleep", "30"}},
		HostConfig: &container.HostConfig{},
	})
	if err != nil {
		t.Fatalf("negotiated Docker SDK create: %v", err)
	}
	if created.ID != "synthetic-sdk-container" {
		t.Fatalf("SDK create = %+v", created)
	}
	inspected, err := client.ContainerInspect(ctx, created.ID, dockerclient.ContainerInspectOptions{})
	if err != nil || inspected.Container.Config.Image != "synthetic:latest" {
		t.Fatalf("SDK inspect = %+v, error=%v", inspected, err)
	}
}

func TestDockerSDKReportsUnsupportedVersionErrors(t *testing.T) {
	host := startDockerTestAPI(t, New(&fakeRunner{}))
	for _, version := range []string{"1.40", "1.48"} {
		client := dockerSDKClient(t, host, dockerclient.WithAPIVersion(version))
		_, err := client.ContainerCreate(context.Background(), dockerclient.ContainerCreateOptions{Image: "synthetic:latest"})
		if err == nil || !strings.Contains(err.Error(), "supported API version") {
			t.Fatalf("SDK version %s error = %v", version, err)
		}
	}
}

func TestDockerSDKStatsCalculatesBackendCPUUsage(t *testing.T) {
	var samples atomic.Uint64
	manager := New(&fakeRunner{})
	manager.metricReader = func(context.Context, string) (ContainerMetricSample, error) {
		index := samples.Add(1)
		systemCPU := index * 2_000_000_000
		// One undersampled tick interval must not change the fixed-window 40% result.
		if index == 2 {
			systemCPU -= 1_650_000_000
		}
		return ContainerMetricSample{
			ID: "synthetic-sdk-stats", Name: "sdk-stats", Read: time.Unix(int64(index), 0),
			CPU: &ContainerMetricCPU{
				TotalUsage:     index * 400_000_000,
				SystemCPUUsage: uint64Pointer(systemCPU),
				OnlineCPUs:     2,
			},
		}, nil
	}
	client := dockerSDKClient(t, startDockerTestAPI(t, manager))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, options := range []dockerclient.ContainerStatsOptions{
		{IncludePreviousSample: true},
		{},
		{Stream: true},
	} {
		before := samples.Load()
		stats, baseline := readDevelopmentClientStats(t, ctx, client, "synthetic-sdk-stats", options)
		if options.IncludePreviousSample || options.Stream {
			containerDelta := stats.CPUStats.CPUUsage.TotalUsage - baseline.CPUUsage.TotalUsage
			hostDelta := stats.CPUStats.SystemUsage - baseline.SystemUsage
			percentage := float64(containerDelta) / float64(hostDelta) * float64(stats.CPUStats.OnlineCPUs) * 100
			if percentage != 40 {
				t.Fatalf("SDK CPU percentage = %f, want 40", percentage)
			}
			wantSamples := uint64(6)
			if options.IncludePreviousSample {
				wantSamples = 10
			}
			if samples.Load()-before != wantSamples {
				t.Fatalf("CPU window used %d samples, want %d", samples.Load()-before, wantSamples)
			}
		} else if samples.Load()-before != 1 || stats.PreCPUStats.SystemUsage != 0 {
			t.Fatal("one-shot sampled twice or supplied invented previous counters")
		}
	}
}

func TestDockerSDKImagePushSurfacesPartialTransferErrors(t *testing.T) {
	runner := &fakeRunner{streamer: func(_ runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("synthetic layer uploaded")}); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("synthetic registry digest rejected")
	}}
	client := dockerSDKClient(t, startDockerTestAPI(t, New(runner)))
	response, err := client.ImagePush(context.Background(), "registry.example.test/synthetic:v1", dockerclient.ImagePushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Wait(context.Background()); err == nil || !strings.Contains(err.Error(), "synthetic registry digest rejected") {
		t.Fatalf("SDK push accepted partial failure: %v", err)
	}
}
