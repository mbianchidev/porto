package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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
				TotalUsage:     4_000_000,
				UserUsage:      3_000_000,
				SystemUsage:    1_000_000,
				PerCPUUsage:    []uint64{2_000_000, 2_000_000},
				SystemCPUUsage: metricUint64(1_000_000_000),
				OnlineCPUs:     2,
				Throttling:     ContainerMetricThrottling{Periods: 7, ThrottledPeriods: 2, ThrottledTime: 500},
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
	request := httptest.NewRequest(http.MethodGet, "/v1.47/containers/demo/stats?stream=false&one-shot=true", nil)

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

func TestDockerContainerStatsIncludesPreviousBackendCPUSample(t *testing.T) {
	var calls atomic.Uint64
	manager := New(&fakeRunner{})
	manager.metricReader = func(context.Context, string) (ContainerMetricSample, error) {
		index := calls.Add(1)
		return ContainerMetricSample{
			ID: "synthetic", Read: time.Unix(int64(index), 0),
			CPU: &ContainerMetricCPU{
				TotalUsage:     index * 500_000_000,
				SystemCPUUsage: metricUint64(index * 2_000_000_000),
				OnlineCPUs:     2,
			},
		}, nil
	}
	response := httptest.NewRecorder()
	NewAPI(manager, "/tmp/porto.sock").ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/v1.47/containers/synthetic/stats?stream=false", nil))
	var stats struct {
		Read    string `json:"read"`
		PreRead string `json:"preread"`
		CPU     struct {
			System uint64 `json:"system_cpu_usage"`
			Online uint32 `json:"online_cpus"`
			Usage  struct {
				Total uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"cpu_stats"`
		Previous struct {
			System uint64 `json:"system_cpu_usage"`
			Usage  struct {
				Total uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
		} `json:"precpu_stats"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || stats.Read == stats.PreRead || stats.Previous.System == 0 {
		t.Fatalf("previous sample missing: calls=%d; stats=%+v", calls.Load(), stats)
	}
	percent := float64(stats.CPU.Usage.Total-stats.Previous.Usage.Total) /
		float64(stats.CPU.System-stats.Previous.System) * float64(stats.CPU.Online) * 100
	if percent != 50 {
		t.Fatalf("Docker CPU percentage = %f, want 50", percent)
	}
}

func TestDockerContainerStatsRejectsStreamingOneShotWithoutSampling(t *testing.T) {
	manager := New(&fakeRunner{})
	var calls atomic.Int32
	manager.metricReader = func(context.Context, string) (ContainerMetricSample, error) {
		calls.Add(1)
		return ContainerMetricSample{}, nil
	}
	for _, query := range []string{"one-shot=true", "stream=true&one-shot=true", "stream=invalid", "stream=false&one-shot=invalid"} {
		response := httptest.NewRecorder()
		NewAPI(manager, "/tmp/porto.sock").ServeHTTP(response,
			httptest.NewRequest(http.MethodGet, "/v1.47/containers/synthetic/stats?"+query, nil))
		if response.Code != http.StatusBadRequest || calls.Load() != 0 {
			t.Fatalf("%s = %d; samples=%d", query, response.Code, calls.Load())
		}
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
			CPU: &ContainerMetricCPU{TotalUsage: 1_000_000, SystemCPUUsage: metricUint64(2_000_000), OnlineCPUs: 2},
			Memory: &ContainerMetricMemory{
				Usage: metricUint64(2048), Limit: metricUint64(8192), Stats: map[string]uint64{},
			},
		}, nil
	}
	socketPath := dockerTestEndpoint(t)
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
	createContext := exec.Command("docker", "context", "create", "porto", "--docker", "host="+EndpointURL(socketPath))
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

type persistentMetricRuntime struct {
	*fakeContainerRuntime
}

func (runtime *persistentMetricRuntime) ContainerMetric(context.Context, string) (ContainerMetricSample, error) {
	return ContainerMetricSample{ID: "synthetic-persistent", Read: time.Now().UTC()}, nil
}

func TestContainerMetricsReuseInventoryConnectionWithoutClosingIt(t *testing.T) {
	runtime := &persistentMetricRuntime{newFakeContainerRuntime()}
	manager := New(&fakeRunner{})
	manager.inventory = newContainerInventory(nil, defaultInventoryOptions())
	manager.inventory.snapshot.Available = true
	manager.inventory.runtimeClient = runtime
	manager.inventoryCancel = func() {}
	manager.runtimeConnector = func(context.Context) (containerRuntime, error) {
		t.Fatal("a new SSH connection was opened for an active inventory metric sample")
		return nil, nil
	}
	for range 4 {
		sample, err := manager.ContainerMetric(context.Background(), "synthetic-persistent")
		if err != nil || sample.ID != "synthetic-persistent" {
			t.Fatalf("persistent metric = %+v, error=%v", sample, err)
		}
	}
	if runtime.closed {
		t.Fatal("a metric request closed the inventory-owned connection")
	}
}

func TestLimaCPUUsageComesFromVerifiedGuestProcCounters(t *testing.T) {
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if command.Name != "limactl" || strings.Join(command.Args, " ") != "shell --workdir=/ synthetic-owned -- cat /proc/stat" {
			t.Fatalf("CPU metrics were not read in the selected backend: %+v", command)
		}
		return []byte("cpu 100 0 0 100 0 0 0 0 0 0\ncpu0 50 0 0 50\ncpu1 50 0 0 50\n"), nil
	}}
	runtime := &grpcContainerRuntime{lima: "synthetic-owned", runner: runner}
	usage, err := runtime.hostCPUUsage(context.Background())
	if err != nil || usage.TotalUsage != 2_000_000_000 || usage.OnlineCPUs != 2 {
		t.Fatalf("backend CPU sample = %+v, error=%v", usage, err)
	}
}
