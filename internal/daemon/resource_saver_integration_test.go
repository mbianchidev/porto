package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/acceleration"
	"github.com/mbianchidev/porto/internal/config"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/resourcesaver"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/store"
	"github.com/mbianchidev/porto/internal/syncshares"
	"golang.org/x/net/websocket"
)

type isolatedPolicyRunner struct{ runtimes.ExecRunner }

func (runner isolatedPolicyRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	if filepath.Base(command.Name) == "limactl" && len(command.Args) > 0 && command.Args[0] == "start" &&
		slices.Contains(command.Args, "--name=porto-engine") {
		arguments := make([]string, 0, len(command.Args)+1)
		for _, argument := range command.Args[:len(command.Args)-1] {
			if argument != "--mount-writable" {
				arguments = append(arguments, argument)
			}
		}
		command.Args = append(arguments, "--mount-none", command.Args[len(command.Args)-1])
	}
	return runner.ExecRunner.Run(ctx, command)
}

func TestNativeAppleSiliconRuntimePolicies(t *testing.T) {
	if os.Getenv("PORTO_TEST_NATIVE_RUNTIME_POLICIES") != "1" {
		t.Skip("set PORTO_TEST_NATIVE_RUNTIME_POLICIES=1 only after authorizing an isolated 2 GiB VZ acceptance VM")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Fatal("native Rosetta acceptance requires Apple Silicon macOS")
	}
	root, err := os.MkdirTemp("/tmp", "porto-native-policy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTO_HOME", filepath.Join(root, "porto"))
	t.Setenv("LIMA_HOME", filepath.Join(root, "lima"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	tools := filepath.Join(root, "tools")
	if err := os.MkdirAll(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	helper := process.NewCommand(ctx, repository, "go", "build", "-trimpath", "-o", filepath.Join(tools, "porto-runtime-helper"), "./cmd/porto-runtime-helper")
	helper.Env = process.WithEnvironment(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	if output, err := helper.CombinedOutput(); err != nil {
		t.Fatalf("build the required guest transport helper before creating the isolated VM: %v: %s", err, output)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	runner := isolatedPolicyRunner{}
	architecture := acceleration.New(filepath.Join(root, "porto"), runner)
	host := architecture.ProbeHost(ctx)
	if !host.Virtualization.Supported || !host.Rosetta.Supported {
		t.Fatalf("native acceptance cannot substitute QEMU for missing VZ/Rosetta: %+v", host)
	}
	if err := architecture.Save(ctx, acceleration.Settings{Policy: acceleration.RosettaPreferred, RosettaEnabled: true}); err != nil {
		t.Fatal(err)
	}
	manager := portodocker.NewWithStateDir(runner, filepath.Join(root, "porto", "docker"))
	manager.SetAccelerationManager(architecture)
	swap := 0
	if err := manager.SetEngineLimits(ctx, portodocker.EngineLimits{CPUs: 2, MemoryMiB: 2048, SwapMiB: &swap, DiskGiB: 20}); err != nil {
		t.Fatal(err)
	}
	installed := false
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		if installed {
			if err := manager.RemoveEngine(cleanupContext); err != nil {
				t.Errorf("remove only the isolated acceptance VM: %v", err)
				return
			}
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	provisionStarted := time.Now()
	if _, err := manager.InstallEngine(ctx); err != nil {
		t.Fatal(err)
	}
	installed = true
	t.Logf("fresh isolated engine provisioning completed in %s; cloud-image downloads may use Lima's host cache", time.Since(provisionStarted))
	capabilities, err := manager.AccelerationCapabilities(ctx)
	if err != nil || capabilities.VMType != "vz" || capabilities.GuestArchitecture != "arm64" || capabilities.AMD64Handler != "rosetta" {
		t.Fatalf("the real engine did not select Rosetta in native VZ: %+v, %v", capabilities, err)
	}
	budgets, err := manager.EngineResourceStatus(ctx)
	if err != nil || budgets.Current == nil || budgets.Current.CPUs != 2 || budgets.Current.Memory != 2<<30 || budgets.Current.Disk != 20<<30 {
		t.Fatalf("VM budgets did not match the configured provider allocation: %+v, %v", budgets, err)
	}
	t.Logf("native acceptance host=%s/%s Lima=%s VZ=true guest=arm64 amd64=%s CPUs=2 memoryMiB=2048 diskGiB=20",
		host.HostOS, host.HostArchitecture, host.LimaVersion, capabilities.AMD64Handler)
	runOwnedRuntimePolicyAcceptance(t, ctx, manager, "amd64", architecture)
}

func TestResourceSaverOwnedRuntimeAcceptance(t *testing.T) {
	if os.Getenv("PORTO_DOCKER_CLIENT_INTEGRATION") != "1" || os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("real acceptance runs only in the existing isolated development-client CI fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	manager := portodocker.New(nil)
	runOwnedRuntimePolicyAcceptance(t, ctx, manager, runtime.GOARCH, nil)
}

func runOwnedRuntimePolicyAcceptance(t *testing.T, ctx context.Context, manager *portodocker.Manager, target string, architecture *acceleration.Manager) {
	t.Helper()
	stateRoot, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(stateRoot, "synthetic-policy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	server := New(st, nil)
	server.docker = manager
	server.syncShares = syncshares.New(st.Directory(), nil, manager.SynchronizedShareProvider())
	manager.SetSynchronizedShareResolver(server.syncShares.Resolve)
	if err := server.initializeResourceSaver(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := server.resourceSaver.Close(cleanupContext); err != nil {
			t.Error(err)
		}
		if err := server.syncShares.Close(cleanupContext); err != nil {
			t.Error(err)
		}
		if err := manager.StopContainerInventory(cleanupContext); err != nil {
			t.Error(err)
		}
	})
	if err := manager.StartContainerInventory(ctx); err != nil {
		t.Fatal(err)
	}
	waitPolicy(t, ctx, func() bool { snapshot := manager.ContainerSnapshot(); return snapshot.Available && !snapshot.Stale }, "fresh owned runtime inventory")
	image := fmt.Sprintf("porto-synthetic-policy:%d", time.Now().UnixNano())
	archive := syntheticPolicyImage(t, ctx, target)
	var beforeBuildBytes int64
	if architecture != nil {
		_, beforeBuildBytes = policyEngineResidentMemory(t, ctx)
	}
	buildStarted := time.Now()
	if output, err := manager.Build(ctx, portodocker.BuildRequest{ContextReader: bytes.NewReader(archive), Tags: []string{image}, Platform: "linux/" + target, NoCache: true}); err != nil {
		t.Fatalf("build synthetic policy image: %v: %s", err, output)
	}
	if architecture != nil {
		buildDuration := time.Since(buildStarted)
		_, afterBuildBytes := policyEngineResidentMemory(t, ctx)
		time.Sleep(5 * time.Second)
		_, settledBytes := policyEngineResidentMemory(t, ctx)
		t.Logf("no-cache scratch BuildKit build=%s; host process subtree RSS before=%d immediately-after=%d after-five-seconds=%d bytes",
			buildDuration, beforeBuildBytes, afterBuildBytes, settledBytes)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := manager.RemoveImage(cleanupContext, image, true); err != nil {
			t.Error(err)
		}
	})
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("porto-policy-%d", time.Now().UnixNano())
	id, err := manager.RunContainer(ctx, portodocker.CreateContainerRequest{
		Name: name, Image: image, Platform: "linux/" + target,
		Publish: []string{fmt.Sprintf("127.0.0.1:%d:8080/tcp", port)},
		Labels: map[string]string{"dev.porto.http-port": "8080", "dev.porto.hostname": name,
			portodocker.ResourceSaverLabel: "stop", portodocker.ManagedWorkloadLabel: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := server.resourceSaver.Close(cleanupContext); err != nil {
			t.Error(err)
		}
		if err := manager.ContainerAction(cleanupContext, id, "remove-force"); err != nil {
			t.Error(err)
		}
	})
	hostname := name + ".docker"
	host := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	waitPolicy(t, ctx, func() bool {
		response, err := client.Get(host)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK && response.Header.Get("X-Synthetic-Arch") == target
	}, "actual guest HTTP listener")
	client.CloseIdleConnections()
	if architecture != nil {
		capabilities, err := manager.AccelerationCapabilities(ctx)
		if err != nil || capabilities.AMD64Handler != "rosetta" {
			t.Fatalf("amd64 fixture was not running under selected Rosetta: %+v, %v", capabilities, err)
		}
	}
	waitPolicy(t, ctx, func() bool {
		for _, container := range manager.ContainerSnapshot().Containers {
			if container.ID == id {
				return container.PID > 0 && len(container.NetworkDetails) > 0
			}
		}
		return false
	}, "real published-port inventory")
	for _, container := range manager.ContainerSnapshot().Containers {
		if container.ID == id {
			routes, warnings := containerHTTPRoutes(manager.ContainerSnapshot())
			t.Logf("actual synthetic HTTP route inputs: name=%s HTTP-port=%s state=%s task=%t mappings=%+v route-count=%d warnings=%v",
				container.Name, container.Labels["dev.porto.http-port"], container.State, container.TaskPresent, container.NetworkDetails, len(routes), warnings)
			if _, exists := routes[hostname]; !exists {
				t.Fatalf("the real runtime did not expose the required owned HTTP route: %v", warnings)
			}
		}
	}
	before, err := manager.ContainerMetric(ctx, id)
	if err != nil || before.Memory == nil || before.Memory.Usage == nil {
		t.Fatalf("real before-stop memory sample unavailable: %+v, %v", before, err)
	}
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		routes, _ := containerHTTPRoutes(manager.ContainerSnapshot())
		routes, _ = server.resourceSaverRoutes(routes)
		server.networkRoutesMu.Lock()
		server.networkRoutes = routes
		server.networkRoutesMu.Unlock()
		if !server.proxyContainerRoute(w, request, hostname) {
			http.Error(w, "synthetic route unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer router.Close()
	configuration := resourcesaver.Defaults()
	configuration.Mode, configuration.IdleSeconds, configuration.WakeSeconds = resourcesaver.ScaleZero, 1, 60
	if os.Getenv("PORTO_DOCKER_CLIENT_SOFTWARE_EMULATION") == "1" {
		configuration.WakeSeconds = 600
	}
	if err := server.resourceSaver.Configure(configuration); err != nil {
		t.Fatal(err)
	}
	applicationID := "container:" + id
	sleep := func() {
		t.Helper()
		previousDiagnostic := ""
		waitPolicy(t, ctx, func() bool {
			if err := server.resourceSaver.Tick(ctx); err != nil {
				t.Fatalf("idle sample: %v", err)
			}
			server.saverBackend.mu.Lock()
			diagnostic := fmt.Sprintf("%+v", server.saverBackend.latest.Applications)
			server.saverBackend.mu.Unlock()
			if diagnostic != previousDiagnostic {
				state := server.resourceSaver.State()
				last := ""
				if len(state.Events) > 0 {
					last = state.Events[len(state.Events)-1].Action + ": " + state.Events[len(state.Events)-1].Message
				}
				t.Logf("real idle eligibility: %s; owned-applications=%d; last-event=%s", diagnostic, len(state.Entries), last)
				previousDiagnostic = diagnostic
			}
			return server.resourceSaver.State().Entries[applicationID].Phase == "sleeping"
		}, "eligible synthetic service scale-to-zero")
	}
	sleep()
	waitPolicy(t, ctx, func() bool {
		for _, container := range manager.ContainerSnapshot().Containers {
			if container.ID == id {
				return !container.TaskPresent || container.State == "exited"
			}
		}
		return false
	}, "real task exit and memory release")
	payload := bytes.Repeat([]byte("synthetic\x00binary upload\xff\n"), 32768)
	expected := fmt.Sprintf("%x", sha256.Sum256(payload))
	started := time.Now()
	response, err := router.Client().Post(router.URL, "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(response.Body)
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if readErr != nil || response.StatusCode != http.StatusOK || strings.TrimSpace(string(data)) != expected {
		t.Fatalf("initiating upload changed or was released early: status=%d response=%s error=%v", response.StatusCode, data, readErr)
	}
	t.Logf("observed backend=%s target=%s task memory before=%d bytes; stopped task absent; original %d-byte POST wake=%s",
		manager.ContainerSnapshot().Backend, target, *before.Memory.Usage, len(payload), time.Since(started))
	sleep()
	beforeEvents := len(server.resourceSaver.State().Events)
	var requests sync.WaitGroup
	failures := make(chan error, 12)
	barrier := make(chan struct{})
	for range 12 {
		requests.Add(1)
		go func() {
			defer requests.Done()
			<-barrier
			response, err := router.Client().Post(router.URL, "application/octet-stream", bytes.NewReader(payload))
			if err != nil {
				failures <- err
				return
			}
			data, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || strings.TrimSpace(string(data)) != expected {
				failures <- fmt.Errorf("concurrent initiating POST: status=%d read=%v close=%v response=%s", response.StatusCode, readErr, closeErr, data)
			}
		}()
	}
	close(barrier)
	requests.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	resumes := 0
	for _, event := range server.resourceSaver.State().Events[beforeEvents:] {
		if event.Action == "resumed" {
			resumes++
		}
	}
	if resumes != 1 {
		t.Fatalf("12 simultaneous real requests caused %d resumes", resumes)
	}
	connection, err := websocket.Dial("ws"+strings.TrimPrefix(router.URL, "http")+"/socket", "", router.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte("synthetic websocket payload")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len("synthetic websocket payload"))
	if _, err := io.ReadFull(connection, echo); err != nil || string(echo) != "synthetic websocket payload" {
		t.Fatalf("WebSocket stream changed: %q, %v", echo, err)
	}
	time.Sleep(1200 * time.Millisecond)
	if err := server.resourceSaver.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, owned := server.resourceSaver.State().Entries[applicationID]; owned {
		t.Fatal("an active WebSocket was scaled to zero")
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.ContainerAction(ctx, id, "pause"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if err := server.resourceSaver.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, owned := server.resourceSaver.State().Entries[applicationID]; owned {
		t.Fatal("a real manual pause was adopted")
	}
	if err := manager.ContainerAction(ctx, id, "unpause"); err != nil {
		t.Fatal(err)
	}
	if architecture != nil || os.Getenv("PORTO_DOCKER_CLIENT_BACKEND") == "lima" {
		sleep()
		residentPID, residentBytes := policyEngineResidentMemory(t, ctx)
		if architecture != nil {
			cpuBefore := nativePolicyCPUSeconds(t, ctx, residentPID)
			sampleStarted := time.Now()
			time.Sleep(5 * time.Second)
			cpuAfter := nativePolicyCPUSeconds(t, ctx, residentPID)
			t.Logf("native VZ idle host-process CPU: %.3f seconds over %.3f seconds (%.3f%% of one core)",
				cpuAfter-cpuBefore, time.Since(sampleStarted).Seconds(),
				100*(cpuAfter-cpuBefore)/time.Since(sampleStarted).Seconds())
		}
		configuration.SuspendEngine = true
		if err := server.resourceSaver.Configure(configuration); err != nil {
			t.Fatal(err)
		}
		waitPolicy(t, ctx, func() bool {
			if err := server.resourceSaver.Tick(ctx); err != nil {
				t.Fatalf("engine idle sample: %v", err)
			}
			for _, event := range server.resourceSaver.State().Events {
				if event.Action == "engine-suspended" {
					return true
				}
			}
			return false
		}, "owned engine suspension after all tasks exit")
		processes, captureErr := process.CaptureResourceSnapshot(ctx)
		if captureErr != nil {
			t.Fatal(captureErr)
		}
		if _, err := processes.Stats(residentPID, true); err == nil {
			t.Fatal("the measured engine host process remained resident after complete suspension")
		}
		t.Logf("measured engine host-resident memory before idle suspension=%d bytes; after suspension PID %d absent", residentBytes, residentPID)
		wakeStarted := time.Now()
		response, err := router.Client().Post(router.URL, "application/octet-stream", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || response.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != expected {
			t.Fatalf("engine wake did not preserve its initiating request: status=%d read=%v close=%v body=%s", response.StatusCode, readErr, closeErr, body)
		}
		t.Logf("observed complete engine shutdown and traffic-triggered engine/task readiness wake: %s; software-emulation=%s",
			time.Since(wakeStarted), os.Getenv("PORTO_DOCKER_CLIENT_SOFTWARE_EMULATION"))
	}
	if architecture != nil {
		configuration.Mode, configuration.SuspendEngine = resourcesaver.Disabled, false
		if err := server.resourceSaver.Configure(configuration); err != nil {
			t.Fatal(err)
		}
		if err := manager.ContainerAction(ctx, id, "stop"); err != nil {
			t.Fatal(err)
		}
		if err := architecture.Save(ctx, acceleration.Settings{Policy: acceleration.NativeOnly}); err != nil {
			t.Fatal(err)
		}
		if err := manager.ContainerAction(ctx, id, "start"); err == nil {
			t.Fatal("native-only policy mutated a real amd64 workload on the arm64 guest")
		}
		if _, err := manager.Build(ctx, portodocker.BuildRequest{ContextReader: bytes.NewReader(archive), Platform: "linux/amd64", NoCache: true}); err == nil {
			t.Fatal("native-only policy allowed a real foreign-platform build")
		}
		if err := architecture.Save(ctx, acceleration.Settings{Policy: acceleration.QEMUCompatible}); err != nil {
			t.Fatal(err)
		}
		if err := manager.ApplyAcceleration(ctx, true); err != nil {
			t.Fatal(err)
		}
		capabilities, err := manager.AccelerationCapabilities(ctx)
		if err != nil || capabilities.AMD64Handler != "qemu" {
			t.Fatalf("real QEMU-compatible fallback was not selected: %+v, %v", capabilities, err)
		}
		if err := manager.ContainerAction(ctx, id, "start"); err != nil {
			t.Fatal(err)
		}
		waitPolicy(t, ctx, func() bool {
			for _, container := range manager.ContainerSnapshot().Containers {
				if container.ID == id {
					return container.Execution == "qemu" && container.State == "running"
				}
			}
			return false
		}, "updated real QEMU execution provenance")
		if err := manager.ContainerAction(ctx, id, "stop"); err != nil {
			t.Fatal(err)
		}
		if err := architecture.Save(ctx, acceleration.Settings{Policy: acceleration.RosettaPreferred, RosettaEnabled: true}); err != nil {
			t.Fatal(err)
		}
		if err := manager.ApplyAcceleration(ctx, true); err != nil {
			t.Fatal(err)
		}
		nativeArchive := syntheticPolicyImage(t, ctx, "arm64")
		if output, err := manager.Build(ctx, portodocker.BuildRequest{ContextReader: bytes.NewReader(nativeArchive), Platform: "linux/arm64", NoCache: true}); err != nil {
			t.Fatalf("native arm64 build failed after foreign-policy transitions: %v: %s", err, output)
		}
		capabilities, err = manager.AccelerationCapabilities(ctx)
		if err != nil || capabilities.AMD64Handler != "rosetta" {
			t.Fatalf("native arm64 build unexpectedly changed emulation setup: %+v, %v", capabilities, err)
		}
		if err := manager.SetEngineLimits(ctx, portodocker.EngineLimits{DiskGiB: 10}); err == nil {
			t.Fatal("a real existing VM disk accepted a destructive shrink")
		}
		t.Log("real policy precedence, native-only rejection, QEMU fallback, native arm64 execution, Rosetta restoration, and non-destructive disk rejection passed")
	}
	if runtime.GOOS != "windows" && (architecture != nil || os.Getenv("PORTO_DOCKER_CLIENT_BACKEND") == "lima") {
		configuration.Mode, configuration.SuspendEngine = resourcesaver.Disabled, false
		if err := server.resourceSaver.Configure(configuration); err != nil {
			t.Fatal(err)
		}
		testOwnedGuestSynchronizedShare(t, ctx, server, manager, image, target)
	}
}

func policyEngineResidentMemory(t *testing.T, ctx context.Context) (int, int64) {
	t.Helper()
	output, err := (runtimes.ExecRunner{}).Run(ctx, runtimes.Command{Name: "limactl", Args: []string{"list", "porto-engine", "--json"}})
	if err != nil {
		t.Fatalf("read only the isolated engine's host process identity: %v: %s", err, output)
	}
	var instance struct {
		DriverPID    int `json:"driverPID"`
		HostAgentPID int `json:"hostAgentPID"`
	}
	if err := json.Unmarshal(output, &instance); err != nil {
		t.Fatal(err)
	}
	pid := instance.HostAgentPID
	if pid <= 0 {
		pid = instance.DriverPID
	}
	if pid <= 0 {
		t.Fatal("the owned engine did not report an observable host process")
	}
	snapshot, err := process.CaptureResourceSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	usage, err := snapshot.Stats(pid, true)
	if err != nil {
		t.Fatal(err)
	}
	return pid, usage.MemoryBytes
}

func nativePolicyCPUSeconds(t *testing.T, ctx context.Context, pid int) float64 {
	t.Helper()
	output, err := (runtimes.ExecRunner{}).Run(ctx, runtimes.Command{
		Name: "ps", Args: []string{"-p", strconv.Itoa(pid), "-o", "time="}, Env: []string{"LC_ALL=C"},
	})
	if err != nil {
		t.Fatalf("measure only the isolated native VZ process CPU time: %v: %s", err, output)
	}
	var seconds float64
	fields := strings.Split(strings.TrimSpace(string(output)), ":")
	if len(fields) < 2 || len(fields) > 3 {
		t.Fatalf("native VZ process returned an invalid cumulative CPU time: %q", output)
	}
	for _, field := range fields {
		value, err := strconv.ParseFloat(field, 64)
		if err != nil || value < 0 {
			t.Fatalf("invalid native VZ CPU-time component: %q, %v", field, err)
		}
		seconds = 60*seconds + value
	}
	return seconds
}

func waitPolicy(t *testing.T, ctx context.Context, predicate func() bool, description string) {
	t.Helper()
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for !predicate() {
		select {
		case <-ctx.Done():
			t.Fatalf("%s: %v", description, context.Cause(ctx))
		case <-deadline.C:
			t.Fatalf("%s did not complete before its bounded deadline", description)
		case <-ticker.C:
		}
	}
}

func syntheticPolicyImage(t *testing.T, ctx context.Context, architecture string) []byte {
	t.Helper()
	const source = `package main
import ("crypto/sha256"; "fmt"; "io"; "net/http"; "os"; "runtime"; "syscall"; "golang.org/x/net/websocket")
func main() {
 if len(os.Args)>1 && os.Args[1]=="--selftest" { fmt.Print(runtime.GOARCH); return }
 http.Handle("/socket", websocket.Handler(func(connection *websocket.Conn) { _, _ = io.Copy(connection, connection) }))
 http.HandleFunc("/", func(w http.ResponseWriter,r *http.Request) {
  data,err:=io.ReadAll(io.LimitReader(r.Body,32<<20)); if err!=nil { http.Error(w,err.Error(),400); return }
  w.Header().Set("X-Synthetic-Arch",runtime.GOARCH)
  if r.URL.Path=="/read" { data,err=os.ReadFile("/workspace/host-note.txt"); if err!=nil {http.Error(w,err.Error(),500);return}; _,_=w.Write(data);return }
  if r.URL.Path=="/write" { if err=os.WriteFile("/workspace/guest-note.txt",data,0644);err!=nil {http.Error(w,err.Error(),500);return} }
  if r.URL.Path=="/watch" {
   fd,err:=syscall.InotifyInit1(syscall.IN_CLOEXEC);if err!=nil {http.Error(w,err.Error(),500);return};defer syscall.Close(fd)
   if _,err=syscall.InotifyAddWatch(fd,"/workspace",syscall.IN_CREATE|syscall.IN_MOVED_TO|syscall.IN_MODIFY);err!=nil {http.Error(w,err.Error(),500);return}
   fmt.Fprint(w,"watching\n");w.(http.Flusher).Flush();buffer:=make([]byte,4096)
   if _,err=syscall.Read(fd,buffer);err!=nil {return};fmt.Fprint(w,"notified\n");return
  }
  fmt.Fprintf(w,"%x",sha256.Sum256(data))
 })
 if err:=http.ListenAndServe(":8080",nil); err!=nil { panic(err) }
}`
	directory := t.TempDir()
	file := filepath.Join(directory, "fixture.go")
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(directory, "fixture")
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	command := process.NewCommand(ctx, root, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", executable, file)
	command.Env = process.WithEnvironment(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+architecture, "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build original synthetic Linux HTTP fixture: %v: %s", err, output)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, member := range []struct {
		name string
		data []byte
		mode int64
	}{
		{"Dockerfile", []byte("FROM scratch\nCOPY fixture /fixture\nRUN [\"/fixture\", \"--selftest\"]\nENTRYPOINT [\"/fixture\"]\n"), 0o600},
		{"fixture", binary, 0o755},
	} {
		if err := writer.WriteHeader(&tar.Header{Name: member.name, Size: int64(len(member.data)), Mode: member.mode}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(member.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}
