package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	corecontainers "github.com/containerd/containerd/v2/core/containers"
	coreimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestDirectCreateValidationSupportsHostAndNoneNetworks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows direct creation is capability-gated")
	}
	runtimeClient := &grpcContainerRuntime{}
	for _, network := range []string{directNetworkNone, directNetworkHost} {
		hostname, err := runtimeClient.validateDirectCreateRequest(CreateContainerRequest{
			Name:     "demo",
			Image:    "alpine:latest",
			Networks: []ContainerNetwork{{Name: network}},
		})
		if err != nil || hostname != "" {
			t.Fatalf("network %s validation = hostname %q, err %v", network, hostname, err)
		}
	}
}

func TestDirectCreateValidationRejectsUnownedResourcesBeforeMutation(t *testing.T) {
	_, err := (&grpcContainerRuntime{}).validateDirectCreateRequest(CreateContainerRequest{
		Name:     "demo",
		Image:    "alpine:latest",
		Networks: []ContainerNetwork{{Name: directNetworkNone}},
		Volumes:  []string{"data:/data"},
	})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "volume lifecycle") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestDirectCreateValidationFallsBackForAutoRemove(t *testing.T) {
	_, err := (&grpcContainerRuntime{}).validateDirectCreateRequest(CreateContainerRequest{
		Name:     "demo",
		Image:    "alpine:latest",
		Networks: []ContainerNetwork{{Name: directNetworkNone}},
		Remove:   true,
	})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "auto-remove") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestDirectCreateValidationFallsBackForOpenStdin(t *testing.T) {
	_, err := (&grpcContainerRuntime{}).validateDirectCreateRequest(CreateContainerRequest{
		Name:        "demo",
		Image:       "alpine:latest",
		Networks:    []ContainerNetwork{{Name: directNetworkNone}},
		Interactive: true,
	})
	if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "OpenStdin") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestDirectContainerLabelsPersistOwnedRuntimeState(t *testing.T) {
	image := containerd.NewImage(new(containerd.Client), coreimages.Image{
		Name: "docker.io/library/alpine:latest",
		Target: ocispec.Descriptor{
			Digest: digest.FromString("image"),
		},
	})
	runtimeClient := &grpcContainerRuntime{logDir: t.TempDir()}
	labels, err := runtimeClient.directContainerLabels(
		CreateContainerRequest{
			Name:     "demo",
			Image:    "alpine:latest",
			Networks: []ContainerNetwork{{Name: directNetworkNone}},
			Restart:  "always",
			Healthcheck: &ContainerHealthcheck{
				Test: []string{"CMD", "true"},
			},
		},
		image,
		"",
		"container-id",
	)
	if err != nil {
		t.Fatalf("create labels: %v", err)
	}
	if labels[portoManagedLabel] != portoRuntimeVersion ||
		labels[restartPolicyLabel] != "always" ||
		!strings.Contains(labels[nerdctlHealthStateLabel], "starting") ||
		!strings.Contains(labels[portoLogPathLabel], "container-id.log") {
		t.Fatalf("direct labels = %v", labels)
	}
}

func TestResolveContainerIDUsesDockerNameLabel(t *testing.T) {
	containers := &fakeContainersClient{container: &containersapi.Container{
		ID:     "porto-1234567890",
		Labels: map[string]string{nerdctlNameLabel: "demo"},
	}}
	runtimeClient := &grpcContainerRuntime{namespace: "default", containers: containers}
	resolved, err := runtimeClient.resolveContainerID(context.Background(), "demo")
	if err != nil {
		t.Fatalf("resolve container name: %v", err)
	}
	if resolved != "porto-1234567890" {
		t.Fatalf("resolved ID = %q", resolved)
	}
	if want := []string{"get demo", "list"}; !reflect.DeepEqual(containers.calls, want) {
		t.Fatalf("resolution calls = %q, want %q", containers.calls, want)
	}
}

func TestDirectCNIConnectAndDisconnectPersistStructuredState(t *testing.T) {
	connectKey := "/helper cni-connect --network backend --container demo --netns /proc/42/ns/net --aliases api --interface-prefix porto1"
	disconnectKey := "/helper cni-disconnect --network backend --container demo --netns /proc/42/ns/net --aliases api --interface-prefix porto1"
	runner := &fakeRunner{
		outputs: map[string][]byte{
			connectKey:    []byte(`{"interface":"eth1","mac":"02:00:00:00:00:01","addresses":["10.10.0.2"],"gateways":["10.10.0.1"]}`),
			disconnectKey: []byte(`{"removed":true}`),
		},
		errors: map[string]error{},
	}
	containers := &fakeContainersClient{container: &containersapi.Container{
		ID: "demo",
		Labels: map[string]string{
			nerdctlNetworksLabel: `["none"]`,
		},
	}}
	tasks := &fakeTasksClient{process: &tasktypes.Process{
		ContainerID: "demo",
		Pid:         42,
		Status:      tasktypes.Status_RUNNING,
	}}
	runtimeClient := &grpcContainerRuntime{
		namespace:  "default",
		runner:     runner,
		helperPath: "/helper",
		containers: containers,
		tasks:      tasks,
	}
	if err := runtimeClient.Connect(context.Background(), "backend", "demo", []string{"api"}); err != nil {
		t.Fatalf("connect CNI: %v", err)
	}
	var state map[string]ContainerNetworkState
	if err := json.Unmarshal(
		[]byte(containers.container.GetLabels()[portoNetworkStateLabel]),
		&state,
	); err != nil {
		t.Fatalf("decode network state: %v", err)
	}
	if got := state["backend"]; got.Interface != "eth1" ||
		got.IPAddress != "10.10.0.2" ||
		!reflect.DeepEqual(got.Aliases, []string{"api"}) {
		t.Fatalf("network state = %+v", got)
	}
	if err := runtimeClient.Disconnect(context.Background(), "backend", "demo", false); err != nil {
		t.Fatalf("disconnect CNI: %v", err)
	}
	if strings.Contains(containers.container.GetLabels()[nerdctlNetworksLabel], "backend") {
		t.Fatalf("network metadata was not removed: %v", containers.container.GetLabels())
	}
}

func TestForcedNetworkReconciliationRestoresNewTaskNamespace(t *testing.T) {
	connectKey := "/helper cni-connect --network backend --container demo --netns /proc/42/ns/net --aliases api --interface-prefix porto1"
	runner := &fakeRunner{
		outputs: map[string][]byte{
			connectKey: []byte(`{"interface":"porto10","addresses":["10.10.0.3"]}`),
		},
		errors: map[string]error{},
	}
	state, _ := json.Marshal(map[string]ContainerNetworkState{
		"backend": {Name: "backend", Interface: "porto10", Aliases: []string{"api"}},
	})
	aliases, _ := json.Marshal(map[string][]string{"backend": {"api"}})
	containers := &fakeContainersClient{container: &containersapi.Container{
		ID: "demo",
		Labels: map[string]string{
			portoNetworkStateLabel: string(state),
			portoNetworkAliasLabel: string(aliases),
			portoNetworkPIDLabel:   "41",
		},
	}}
	runtimeClient := &grpcContainerRuntime{
		namespace:  "default",
		runner:     runner,
		helperPath: "/helper",
		containers: containers,
		tasks: &fakeTasksClient{process: &tasktypes.Process{
			ContainerID: "demo",
			Pid:         42,
			Status:      tasktypes.Status_RUNNING,
		}},
	}
	if err := runtimeClient.ReconcileNetworks(context.Background(), "demo", true); err != nil {
		t.Fatalf("force reconcile networks: %v", err)
	}
	if got := containers.container.GetLabels()[portoNetworkPIDLabel]; got != "42" {
		t.Fatalf("network PID label = %q, want 42", got)
	}
	if len(runner.commands) != 1 || runner.commands[0].Args[0] != "cni-connect" {
		t.Fatalf("helper commands = %+v", runner.commands)
	}
}

func TestHealthSchedulingHelpersUseDockerTiming(t *testing.T) {
	check := &ContainerHealthcheck{
		Test:          []string{"CMD-SHELL", "test -f /tmp/ready"},
		Interval:      30 * time.Second,
		StartPeriod:   time.Minute,
		StartInterval: time.Second,
		Retries:       4,
	}
	if got := healthCommand(check); !reflect.DeepEqual(got, []string{"sh", "-c", "test -f /tmp/ready"}) {
		t.Fatalf("health command = %q", got)
	}
	if got := healthInterval(check, true); got != time.Second {
		t.Fatalf("start interval = %s", got)
	}
	if got := healthInterval(check, false); got != 30*time.Second {
		t.Fatalf("normal interval = %s", got)
	}
	if got := healthRetries(check); got != 4 {
		t.Fatalf("health retries = %d", got)
	}
}

func TestBackendLocalSpecPreservesImageCommandWithEntrypointOverride(t *testing.T) {
	options := directContainerSpecOptions(
		CreateContainerRequest{
			Entrypoint:  []string{"/entrypoint"},
			Environment: []string{"PORTO=1"},
			Networks:    []ContainerNetwork{{Name: directNetworkNone}},
		},
		nil,
		"",
		"linux/arm64",
		ocispec.ImageConfig{
			Cmd:        []string{"serve"},
			Env:        []string{"PATH=/bin"},
			WorkingDir: "/work",
			StopSignal: "SIGQUIT",
		},
		true,
	)
	spec := &oci.Spec{}
	container := &corecontainers.Container{ID: "demo"}
	for _, option := range options {
		if err := option(namespaces.WithNamespace(context.Background(), "default"), nil, container, spec); err != nil {
			t.Fatalf("apply spec option: %v", err)
		}
	}
	if !reflect.DeepEqual(spec.Process.Args, []string{"/entrypoint", "serve"}) {
		t.Fatalf("process args = %q", spec.Process.Args)
	}
	if spec.Process.Cwd != "/work" ||
		!reflect.DeepEqual(spec.Process.Env, []string{"PATH=/bin", "PORTO=1"}) {
		t.Fatalf("process config = %+v", spec.Process)
	}
	if spec.Annotations["org.opencontainers.image.stopSignal"] != "SIGQUIT" {
		t.Fatalf("stop signal annotations = %v", spec.Annotations)
	}
}

func TestLimaDefaultContainerPlatformIsLinux(t *testing.T) {
	platform, err := directContainerPlatform("", true)
	if err != nil {
		t.Fatalf("default platform: %v", err)
	}
	if !strings.HasPrefix(platform, "linux/") {
		t.Fatalf("default Lima platform = %q", platform)
	}
}

func TestCheckpointContainerLabelsDropRuntimeCounters(t *testing.T) {
	encoded, err := json.Marshal(map[string]string{
		"app":              "porto",
		restartCountLabel:  "3",
		restartStatusLabel: "running",
	})
	if err != nil {
		t.Fatal(err)
	}
	labels, err := checkpointContainerLabels(map[string]string{
		checkpointSourceLabelsLabel: string(encoded),
	})
	if err != nil {
		t.Fatalf("checkpoint labels: %v", err)
	}
	if !reflect.DeepEqual(labels, map[string]string{"app": "porto"}) {
		t.Fatalf("restored labels = %v", labels)
	}
}

func TestManagerRoutesRestartPolicyUpdateDirectly(t *testing.T) {
	operations := &fakeContainerOperations{errs: map[string]error{}}
	if err := managerWithContainerOperations(operations).UpdateContainer(
		context.Background(),
		"demo",
		ContainerUpdate{Restart: "on-failure:3"},
	); err != nil {
		t.Fatalf("update restart policy: %v", err)
	}
	if want := []string{"update-restart demo on-failure:3", "close"}; !reflect.DeepEqual(operations.calls, want) {
		t.Fatalf("restart calls = %q, want %q", operations.calls, want)
	}
}

func TestContainerdRestartPolicyDisableRemovesRuntimeLabels(t *testing.T) {
	containers := &fakeContainersClient{container: &containersapi.Container{
		ID: "demo",
		Labels: map[string]string{
			restartPolicyLabel: "always",
			restartCountLabel:  "4",
			restartStatusLabel: "running",
		},
	}}
	runtimeClient := &grpcContainerRuntime{namespace: "default", containers: containers}
	if err := runtimeClient.UpdateRestartPolicy(context.Background(), "demo", "no"); err != nil {
		t.Fatalf("disable restart policy: %v", err)
	}
	labels := containers.updateRequest.GetContainer().GetLabels()
	for _, key := range []string{restartPolicyLabel, restartCountLabel, restartStatusLabel} {
		if labels[key] != "" {
			t.Fatalf("restart label %s = %q in %v", key, labels[key], labels)
		}
	}
}

func TestDirectProcessClosesOutputBeforeWaitConsumerRuns(t *testing.T) {
	reader, writer := io.Pipe()
	wait := make(chan containerd.ExitStatus, 1)
	process := &fakeContainerdProcess{}
	direct := newDirectContainerProcess(
		process,
		wait,
		directProcessIO{
			stdin:  nopWriteCloser{Writer: io.Discard},
			stdout: reader,
			stderr: io.NopCloser(strings.NewReader("")),
			finishOutput: func() {
				_ = writer.Close()
			},
		},
		true,
		"default",
	)
	if err := direct.Kill(); err != nil {
		t.Fatalf("kill process: %v", err)
	}
	if err := direct.Resize(context.Background(), 80, 24); err != nil {
		t.Fatalf("resize process: %v", err)
	}
	status := containerd.NewExitStatus(0, time.Now(), nil)
	wait <- *status
	close(wait)
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(direct.Stdout())
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("read output: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("output did not close after process exit")
	}
	if err := direct.Wait(); err != nil {
		t.Fatalf("wait process: %v", err)
	}
	if process.deleteCalls != 1 {
		t.Fatalf("exec delete calls = %d, want 1", process.deleteCalls)
	}
	if process.deleteNamespace != "default" {
		t.Fatalf("delete namespace = %q", process.deleteNamespace)
	}
	if process.killNamespace != "default" || process.resizeNamespace != "default" {
		t.Fatalf("process namespaces = kill %q resize %q", process.killNamespace, process.resizeNamespace)
	}
}

func TestDirectAttachedTaskRetainsStoppedTask(t *testing.T) {
	wait := make(chan containerd.ExitStatus, 1)
	process := &fakeContainerdProcess{}
	direct := newDirectContainerProcess(
		process,
		wait,
		directProcessIO{
			stdin:  nopWriteCloser{Writer: io.Discard},
			stdout: io.NopCloser(strings.NewReader("")),
			stderr: io.NopCloser(strings.NewReader("")),
		},
		false,
		"default",
	)
	status := containerd.NewExitStatus(0, time.Now(), nil)
	wait <- *status
	close(wait)
	if err := direct.Wait(); err != nil {
		t.Fatalf("wait attached task: %v", err)
	}
	if process.deleteCalls != 0 {
		t.Fatalf("attached task delete calls = %d, want 0", process.deleteCalls)
	}
}

func TestHealthTimeoutKillsExecBeforeReturning(t *testing.T) {
	process := newBlockingHealthProcess()
	manager := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}})
	manager.execConnector = func(context.Context) (execOperations, error) {
		return &fakeExecOperations{process: process}, nil
	}
	result := manager.runHealthCheck(context.Background(), "demo", &ContainerHealthcheck{
		Test:    []string{"CMD", "sleep", "60"},
		Timeout: 10 * time.Millisecond,
	})
	if result == nil || result.ExitCode == 0 || !strings.Contains(result.Output, "timed out") {
		t.Fatalf("health result = %+v", result)
	}
	if !process.killed {
		t.Fatal("timed-out health process was not killed")
	}
}

func TestFirstHealthCheckWaitsForConfiguredInterval(t *testing.T) {
	inventory := newContainerInventory(nil, defaultInventoryOptions())
	inventory.snapshot.Containers = []Container{{
		ID:     "demo",
		State:  "running",
		Labels: map[string]string{portoManagedLabel: portoRuntimeVersion},
		Health: ContainerHealth{Status: "starting"},
		Healthcheck: &ContainerHealthcheck{
			Test:     []string{"CMD", "true"},
			Interval: 10 * time.Second,
		},
	}}
	manager := &Manager{inventory: inventory}
	state := &healthSchedulerState{
		schedules:      make(map[string]*healthSchedule),
		networkNext:    make(map[string]time.Time),
		networkRunning: make(map[string]bool),
	}
	now := time.Now()
	manager.scheduleHealthChecks(context.Background(), state, now)
	schedule := state.schedules["demo"]
	if schedule == nil || !schedule.nextRun.Equal(now.Add(10*time.Second)) {
		t.Fatalf("initial health schedule = %+v", schedule)
	}
}

func TestProcessWaitExitCodeUsesDirectContainerExit(t *testing.T) {
	if got := processWaitExitCode(&containerExitError{code: 23}); got != 23 {
		t.Fatalf("exit code = %d, want 23", got)
	}
}

func TestFailedStreamUpgradeCollectsExecResources(t *testing.T) {
	process := newBlockingHealthProcess()
	if code := stopAndCollectProcess(process); code != 137 {
		t.Fatalf("collected exit code = %d", code)
	}
	if !process.killed {
		t.Fatal("failed stream upgrade did not kill the exec")
	}
}

func TestDirectLogTailZeroStartsAtEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "container.log")
	if err := os.WriteFile(path, []byte("old\nlines\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	offset, err := logTailOffset(file, 0)
	if err != nil {
		t.Fatalf("tail offset: %v", err)
	}
	if offset != int64(len("old\nlines\n")) {
		t.Fatalf("tail=0 offset = %d", offset)
	}
	if all, err := parseLogTail("all"); err != nil || all != allLogLines {
		t.Fatalf("tail=all = %d, %v", all, err)
	}
}

func TestAttachedDisconnectDoesNotKillContainerTask(t *testing.T) {
	process := newDetachableProcess()
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan processStreamResult, 1)
	go func() {
		result <- serveProcessStreamResult(ctx, server, process, false, false, true, true)
	}()
	cancel()
	select {
	case streamResult := <-result:
		if streamResult.authoritative {
			t.Fatalf("disconnect result = %+v", streamResult)
		}
	case <-time.After(time.Second):
		t.Fatal("attached stream did not detach after cancellation")
	}
	if process.killed {
		t.Fatal("attached container task was killed on disconnect")
	}
	process.finish()
}

func TestDirectCNIRejectsHostAndStoppedNamespaces(t *testing.T) {
	tests := []struct {
		name     string
		networks string
		status   tasktypes.Status
		want     error
	}{
		{name: "host", networks: `["host"]`, status: tasktypes.Status_RUNNING, want: ErrUnsupported},
		{name: "stopped", networks: `["none"]`, status: tasktypes.Status_STOPPED, want: ErrConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtimeClient := &grpcContainerRuntime{
				namespace: "default",
				runner:    &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}},
				containers: &fakeContainersClient{container: &containersapi.Container{
					ID:     "demo",
					Labels: map[string]string{nerdctlNetworksLabel: test.networks},
				}},
				tasks: &fakeTasksClient{process: &tasktypes.Process{
					ContainerID: "demo",
					Pid:         42,
					Status:      test.status,
				}},
			}
			err := runtimeClient.Connect(context.Background(), "backend", "demo", nil)
			if !errors.Is(err, test.want) {
				t.Fatalf("connect error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestUnmanagedStoppedTaskUsesCompatibilityRecreation(t *testing.T) {
	tasks := &fakeTasksClient{getErr: status.Error(codes.NotFound, "missing")}
	runtimeClient := &grpcContainerRuntime{
		namespace: "default",
		containers: &fakeContainersClient{container: &containersapi.Container{
			ID:   "demo",
			Spec: &anypb.Any{Value: []byte(`{"process":{"args":["true"]}}`)},
		}},
		tasks: tasks,
	}
	err := runtimeClient.recreateAndStartTask(context.Background(), "demo")
	if !errors.Is(err, ErrUnsupported) || len(tasks.calls) != 0 {
		t.Fatalf("recreate unmanaged task = %v, calls %v", err, tasks.calls)
	}
}

func TestForceDeleteDisablesRestartBeforeKillingTask(t *testing.T) {
	containers := &fakeContainersClient{container: &containersapi.Container{
		ID: "demo",
		Labels: map[string]string{
			portoManagedLabel:  portoRuntimeVersion,
			restartPolicyLabel: "always",
			restartStatusLabel: "running",
		},
	}}
	tasks := &fakeTasksClient{process: &tasktypes.Process{
		ContainerID: "demo",
		Status:      tasktypes.Status_RUNNING,
	}}
	runtimeClient := &grpcContainerRuntime{
		namespace:  "default",
		containers: containers,
		tasks:      tasks,
	}
	if err := runtimeClient.Delete(context.Background(), "demo", true, false); err != nil {
		t.Fatalf("force delete: %v", err)
	}
	if containers.updateRequests[0].GetContainer().GetLabels()[restartStatusLabel] != "stopped" ||
		containers.updateRequests[0].GetContainer().GetLabels()[restartStoppedLabel] != "true" {
		t.Fatalf("restart disable update = %v", containers.updateRequests[0].GetContainer().GetLabels())
	}
}

func TestRenameRejectsDuplicateDockerName(t *testing.T) {
	current := &containersapi.Container{
		ID:     "one",
		Labels: map[string]string{nerdctlNameLabel: "current"},
	}
	containers := &fakeContainersClient{
		container: current,
		listContainers: []*containersapi.Container{
			current,
			{ID: "two", Labels: map[string]string{nerdctlNameLabel: "taken"}},
		},
	}
	runtimeClient := &grpcContainerRuntime{namespace: "default", containers: containers}
	err := runtimeClient.Rename(context.Background(), "one", "taken")
	if !errors.Is(err, ErrConflict) || containers.updateRequest != nil {
		t.Fatalf("rename duplicate = %v, update %v", err, containers.updateRequest)
	}
}

func TestNetworkReconciliationHonorsRetryDeadlineAfterPIDChange(t *testing.T) {
	inventory := newContainerInventory(nil, defaultInventoryOptions())
	inventory.snapshot.Containers = []Container{{
		ID:  "demo",
		PID: 42,
		Labels: map[string]string{
			portoManagedLabel:      portoRuntimeVersion,
			portoNetworkStateLabel: `{"backend":{"name":"backend"}}`,
			portoNetworkPIDLabel:   "41",
		},
	}}
	manager := &Manager{inventory: inventory}
	now := time.Now()
	state := &healthSchedulerState{
		schedules:          make(map[string]*healthSchedule),
		networkNext:        map[string]time.Time{"demo": now.Add(time.Minute)},
		networkRunning:     make(map[string]bool),
		networkObservedPID: map[string]uint32{"demo": 42},
	}
	manager.scheduleNetworkReconciliation(context.Background(), state, inventory.snapshotValue(), now)
	if state.networkRunning["demo"] {
		t.Fatal("network reconciliation ignored retry deadline")
	}
}

func TestHealthGenerationRejectsPreviousTaskResult(t *testing.T) {
	inventory := newContainerInventory(nil, defaultInventoryOptions())
	inventory.snapshot.Containers = []Container{{
		ID:     "demo",
		PID:    42,
		Labels: map[string]string{nerdctlHealthcheckLabel: "new"},
	}}
	manager := &Manager{inventory: inventory}
	if manager.healthGenerationCurrent("demo", 41, "old") {
		t.Fatal("previous task health generation was accepted")
	}
}

type fakeContainerdProcess struct {
	deleteCalls     int
	deleteNamespace string
	killNamespace   string
	resizeNamespace string
}

func (p *fakeContainerdProcess) ID() string                  { return "process" }
func (p *fakeContainerdProcess) Pid() uint32                 { return 42 }
func (p *fakeContainerdProcess) Start(context.Context) error { return nil }
func (p *fakeContainerdProcess) Delete(ctx context.Context, _ ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	p.deleteCalls++
	if outgoing, ok := metadata.FromOutgoingContext(ctx); ok {
		values := outgoing.Get(containerdNamespaceHeader)
		if len(values) > 0 {
			p.deleteNamespace = values[len(values)-1]
		}
	}
	return containerd.NewExitStatus(0, time.Now(), nil), nil
}
func (p *fakeContainerdProcess) Kill(ctx context.Context, _ syscall.Signal, _ ...containerd.KillOpts) error {
	if outgoing, ok := metadata.FromOutgoingContext(ctx); ok {
		values := outgoing.Get(containerdNamespaceHeader)
		if len(values) > 0 {
			p.killNamespace = values[len(values)-1]
		}
	}
	return nil
}
func (p *fakeContainerdProcess) Wait(context.Context) (<-chan containerd.ExitStatus, error) {
	return nil, errors.New("unused")
}
func (p *fakeContainerdProcess) CloseIO(context.Context, ...containerd.IOCloserOpts) error {
	return nil
}
func (p *fakeContainerdProcess) Resize(ctx context.Context, _ uint32, _ uint32) error {
	if outgoing, ok := metadata.FromOutgoingContext(ctx); ok {
		values := outgoing.Get(containerdNamespaceHeader)
		if len(values) > 0 {
			p.resizeNamespace = values[len(values)-1]
		}
	}
	return nil
}
func (p *fakeContainerdProcess) IO() cio.IO { return nil }
func (p *fakeContainerdProcess) Status(context.Context) (containerd.Status, error) {
	return containerd.Status{Status: containerd.Stopped}, nil
}

type blockingHealthProcess struct {
	once   sync.Once
	done   chan struct{}
	killed bool
}

func newBlockingHealthProcess() *blockingHealthProcess {
	return &blockingHealthProcess{done: make(chan struct{})}
}

func (p *blockingHealthProcess) Stdin() io.WriteCloser {
	return nopWriteCloser{Writer: io.Discard}
}
func (p *blockingHealthProcess) Stdout() io.ReadCloser {
	return io.NopCloser(strings.NewReader(""))
}
func (p *blockingHealthProcess) Stderr() io.ReadCloser {
	return io.NopCloser(strings.NewReader(""))
}
func (p *blockingHealthProcess) Wait() error {
	<-p.done
	return &containerExitError{code: 137}
}
func (p *blockingHealthProcess) Kill() error {
	p.once.Do(func() {
		p.killed = true
		close(p.done)
	})
	return nil
}
func (p *blockingHealthProcess) PID() int { return 42 }

type detachableProcess struct {
	stdoutReader *io.PipeReader
	stdoutWriter *io.PipeWriter
	stderrReader *io.PipeReader
	stderrWriter *io.PipeWriter
	done         chan struct{}
	once         sync.Once
	killed       bool
}

func newDetachableProcess() *detachableProcess {
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	return &detachableProcess{
		stdoutReader: stdoutReader,
		stdoutWriter: stdoutWriter,
		stderrReader: stderrReader,
		stderrWriter: stderrWriter,
		done:         make(chan struct{}),
	}
}

func (p *detachableProcess) Stdin() io.WriteCloser {
	return nopWriteCloser{Writer: io.Discard}
}
func (p *detachableProcess) Stdout() io.ReadCloser { return p.stdoutReader }
func (p *detachableProcess) Stderr() io.ReadCloser { return p.stderrReader }
func (p *detachableProcess) Wait() error {
	<-p.done
	return nil
}
func (p *detachableProcess) Kill() error {
	p.killed = true
	p.finish()
	return nil
}
func (p *detachableProcess) PID() int               { return 42 }
func (p *detachableProcess) KillOnDisconnect() bool { return false }
func (p *detachableProcess) finish() {
	p.once.Do(func() {
		_ = p.stdoutWriter.Close()
		_ = p.stderrWriter.Close()
		close(p.done)
	})
}

var _ runtimes.Runner = (*fakeRunner)(nil)
