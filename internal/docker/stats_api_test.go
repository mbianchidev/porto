package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cgroup2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestDockerContainerStatsOneShotUsesRawCounters(t *testing.T) {
	read := time.Now().UTC()
	manager := New(&fakeRunner{})
	manager.metricReader = func(context.Context, string) (ContainerMetricSample, error) {
		return ContainerMetricSample{
			ID:   "container-id",
			Name: "demo",
			Read: read,
			CPU: &ContainerMetricCPU{
				TotalUsage:  4_000_000,
				UserUsage:   3_000_000,
				SystemUsage: 1_000_000,
				PerCPUUsage: []uint64{2_000_000, 2_000_000},
				Throttling:  ContainerMetricThrottling{Periods: 7, ThrottledPeriods: 2, ThrottledTime: 500},
			},
			Memory: &ContainerMetricMemory{
				Usage: metricUint64(2048), MaxUsage: metricUint64(4096), Limit: metricUint64(8192),
				Stats: map[string]uint64{"cache": 128, "pgfault": 4},
			},
			PIDs: &ContainerMetricPIDs{Current: 3, Limit: 64},
			Networks: map[string]ContainerMetricNetwork{
				"eth0": {RxBytes: 10, RxPackets: 2, TxBytes: 20, TxPackets: 3},
			},
			BlockIO: []ContainerMetricBlockIO{{Major: 8, Minor: 0, Op: "read", Value: 512}},
		}, nil
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1.47/containers/demo/stats?stream=false", nil)

	NewAPI(manager, "/tmp/porto.sock").ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("stats = %d: %s", response.Code, response.Body.String())
	}
	var document map[string]any
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	cpu := document["cpu_stats"].(map[string]any)
	usage := cpu["cpu_usage"].(map[string]any)
	if usage["total_usage"] != float64(4_000_000) || cpu["online_cpus"] != float64(2) {
		t.Fatalf("unexpected CPU stats: %+v", cpu)
	}
	memory := document["memory_stats"].(map[string]any)
	if memory["usage"] != float64(2048) || memory["limit"] != float64(8192) {
		t.Fatalf("unexpected memory stats: %+v", memory)
	}
	if document["networks"].(map[string]any)["eth0"] == nil {
		t.Fatalf("network stats missing: %+v", document["networks"])
	}
}

func TestDockerContainerStatsOmitsUnavailableMeasurements(t *testing.T) {
	manager := New(&fakeRunner{})
	manager.metricReader = func(context.Context, string) (ContainerMetricSample, error) {
		return ContainerMetricSample{ID: "container-id", Name: "demo", Read: time.Now().UTC()}, nil
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1.47/containers/demo/stats?stream=false", nil)

	NewAPI(manager, "/tmp/porto.sock").ServeHTTP(response, request)

	var document map[string]any
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	for _, key := range []string{"cpu_stats", "memory_stats", "pids_stats", "networks", "blkio_stats"} {
		if _, exists := document[key]; exists {
			t.Fatalf("unavailable %s was invented: %+v", key, document[key])
		}
	}
}

func TestDockerContainerStatsStreamStopsOnDisconnect(t *testing.T) {
	manager := New(&fakeRunner{})
	var calls atomic.Int32
	manager.metricReader = func(context.Context, string) (ContainerMetricSample, error) {
		calls.Add(1)
		return ContainerMetricSample{ID: "container-id", Read: time.Now().UTC()}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/v1.47/containers/demo/stats?stream=true", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		NewAPI(manager, "/tmp/porto.sock").ServeHTTP(response, request)
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stats stream did not stop after disconnect")
	}
}

func TestContainerMetricFromCgroup2PreservesRawUnits(t *testing.T) {
	sample, err := containerMetricFromCgroup2(&cgroup2.Metrics{
		CPU: &cgroup2.CPUStat{
			UsageUsec: 4_000, UserUsec: 3_000, SystemUsec: 1_000,
			NrPeriods: 7, NrThrottled: 2, ThrottledUsec: 5,
		},
		Memory: &cgroup2.MemoryStat{Usage: 2048, MaxUsage: 4096, UsageLimit: 8192, File: 128},
		Pids:   &cgroup2.PidsStat{Current: 3, Limit: 64},
		Io: &cgroup2.IOStat{Usage: []*cgroup2.IOEntry{{
			Major: 8, Minor: 0, Rbytes: 512, Wbytes: 1024,
		}}},
	})
	if err != nil {
		t.Fatalf("convert metrics: %v", err)
	}
	if sample.CPU == nil || sample.CPU.TotalUsage != 4_000_000 ||
		sample.CPU.Throttling.ThrottledTime != 5_000 {
		t.Fatalf("unexpected CPU conversion: %+v", sample.CPU)
	}
	if sample.Memory == nil || sample.Memory.Usage == nil || *sample.Memory.Usage != 2048 {
		t.Fatalf("unexpected memory conversion: %+v", sample.Memory)
	}
	if len(sample.BlockIO) != 2 || sample.Networks != nil {
		t.Fatalf("unexpected optional metrics: block=%+v networks=%+v", sample.BlockIO, sample.Networks)
	}
}

func TestDockerCLIStatsConsumesOneShotResponse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket compatibility test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Join(command.Args, " ") == "container inspect demo" {
			return []byte(`[{"Id":"container-id","Name":"/demo","State":{"Status":"running","Running":true},"Config":{"Tty":false}}]`), nil
		}
		return nil, fmt.Errorf("unexpected command: %v", command.Args)
	}
	manager := New(runner)
	manager.metricReader = func(context.Context, string) (ContainerMetricSample, error) {
		return ContainerMetricSample{
			ID: "container-id", Name: "demo", Read: time.Now().UTC(),
			CPU: &ContainerMetricCPU{TotalUsage: 1_000_000},
			Memory: &ContainerMetricMemory{
				Usage: metricUint64(2048), Limit: metricUint64(8192), Stats: map[string]uint64{},
			},
		}, nil
	}
	socketDir, err := os.MkdirTemp("/tmp", "porto-stats-cli-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := NewAPIServer(socketPath, NewAPI(manager, socketPath))
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
	command := exec.Command("docker", "--context", "porto", "stats", "--no-stream", "--format", "{{json .}}", "demo")
	command.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker stats: %v: %s", err, output)
	}
	if !strings.Contains(string(output), `"Name":"demo"`) {
		t.Fatalf("unexpected docker stats output: %s", output)
	}
}

func metricUint64(value uint64) *uint64 {
	return &value
}
