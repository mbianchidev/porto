package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type fakeContainerOperations struct {
	calls    []string
	waitCode int
	errs     map[string]error
}

type fakeNetworkOperations struct {
	calls []string
	errs  map[string]error
}

func (f *fakeNetworkOperations) record(call string) error {
	f.calls = append(f.calls, call)
	return f.errs[call]
}

func (f *fakeNetworkOperations) Connect(
	_ context.Context,
	network,
	container string,
	aliases []string,
) error {
	return f.record(fmt.Sprintf("connect %s %s %v", network, container, aliases))
}

func (f *fakeNetworkOperations) Disconnect(
	_ context.Context,
	network,
	container string,
	force bool,
) error {
	return f.record(fmt.Sprintf("disconnect %s %s %t", network, container, force))
}

func (f *fakeNetworkOperations) Close() error {
	return f.record("close")
}

func (f *fakeContainerOperations) record(call string) error {
	f.calls = append(f.calls, call)
	return f.errs[call]
}

func (f *fakeContainerOperations) Start(_ context.Context, id string) error {
	return f.record("start " + id)
}

func (f *fakeContainerOperations) Stop(_ context.Context, id string, timeout int) error {
	return f.record(fmt.Sprintf("stop %s %d", id, timeout))
}

func (f *fakeContainerOperations) Kill(_ context.Context, id string, signal uint32) error {
	return f.record(fmt.Sprintf("kill %s %d", id, signal))
}

func (f *fakeContainerOperations) Pause(_ context.Context, id string) error {
	return f.record("pause " + id)
}

func (f *fakeContainerOperations) Resume(_ context.Context, id string) error {
	return f.record("resume " + id)
}

func (f *fakeContainerOperations) Restart(_ context.Context, id string, timeout int) error {
	return f.record(fmt.Sprintf("restart %s %d", id, timeout))
}

func (f *fakeContainerOperations) Wait(_ context.Context, id string) (int, error) {
	return f.waitCode, f.record("wait " + id)
}

func (f *fakeContainerOperations) Rename(_ context.Context, id, name string) error {
	return f.record("rename " + id + " " + name)
}

func (f *fakeContainerOperations) UpdateLabels(_ context.Context, id string, labels map[string]string) error {
	return f.record(fmt.Sprintf("update-labels %s %v", id, labels))
}

func (f *fakeContainerOperations) UpdateResources(_ context.Context, id string, update ContainerUpdate) error {
	return f.record(fmt.Sprintf(
		"update %s cpu=%d memory=%d swap=%d",
		id,
		update.NanoCPUs,
		update.Memory,
		update.MemorySwap,
	))
}

func (f *fakeContainerOperations) UpdateHealth(
	_ context.Context,
	id string,
	healthcheck *ContainerHealthcheck,
) error {
	return f.record(fmt.Sprintf(
		"update-health %s test=%v interval=%s timeout=%s retries=%d",
		id,
		healthcheck.Test,
		healthcheck.Interval,
		healthcheck.Timeout,
		healthcheck.Retries,
	))
}

func (f *fakeContainerOperations) Delete(_ context.Context, id string, force, volumes bool) error {
	return f.record(fmt.Sprintf("delete %s %t %t", id, force, volumes))
}

func (f *fakeContainerOperations) Close() error {
	return f.record("close")
}

func managerWithContainerOperations(operations *fakeContainerOperations) *Manager {
	manager := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}})
	manager.operationsConnector = func(context.Context) (containerOperations, error) {
		return operations, nil
	}
	return manager
}

func managerWithNetworkOperations(operations *fakeNetworkOperations) *Manager {
	manager := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}})
	manager.networkConnector = func(context.Context) (networkOperations, error) {
		return operations, nil
	}
	return manager
}

func TestManagerRoutesNetworkActionsThroughMockableOperations(t *testing.T) {
	operations := &fakeNetworkOperations{errs: map[string]error{}}
	manager := managerWithNetworkOperations(operations)
	if err := manager.ConnectNetwork(
		context.Background(),
		"backend",
		"demo",
		[]string{"api", "api.internal"},
	); err != nil {
		t.Fatalf("connect network: %v", err)
	}
	if err := manager.DisconnectNetwork(context.Background(), "backend", "demo", true); err != nil {
		t.Fatalf("disconnect network: %v", err)
	}
	want := []string{
		"connect backend demo [api api.internal]", "close",
		"disconnect backend demo true", "close",
	}
	if !reflect.DeepEqual(operations.calls, want) {
		t.Fatalf("operation calls = %q, want %q", operations.calls, want)
	}
}

func TestManagerNetworkActionsFallbackWhenDirectRuntimeIsUnavailable(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl network connect --alias api backend demo": nil,
			"nerdctl network disconnect --force backend demo":  nil,
		},
		errors: map[string]error{},
	}
	manager := New(runner)
	manager.networkConnector = func(context.Context) (networkOperations, error) {
		return nil, ErrUnavailable
	}
	if err := manager.ConnectNetwork(
		context.Background(),
		"backend",
		"demo",
		[]string{"api"},
	); err != nil {
		t.Fatalf("connect network fallback: %v", err)
	}
	if err := manager.DisconnectNetwork(context.Background(), "backend", "demo", true); err != nil {
		t.Fatalf("disconnect network fallback: %v", err)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("fallback commands = %+v, want two network commands", runner.commands)
	}
}

func TestManagerNetworkActionsExposeUnsupportedWithoutPretendingDirectSupport(t *testing.T) {
	call := "connect backend demo []"
	operations := &fakeNetworkOperations{
		errs: map[string]error{call: fmt.Errorf("%w: CNI endpoint lifecycle", ErrUnsupported)},
	}
	runner := &fakeRunner{
		outputs: map[string][]byte{"nerdctl network connect backend demo": nil},
		errors:  map[string]error{},
	}
	manager := New(runner)
	manager.networkConnector = func(context.Context) (networkOperations, error) {
		return operations, nil
	}
	err := manager.ConnectNetwork(context.Background(), "backend", "demo", nil)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("connect network error = %v, want ErrUnsupported", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("unsupported direct operation fell back to commands: %+v", runner.commands)
	}
	if want := []string{call, "close"}; !reflect.DeepEqual(operations.calls, want) {
		t.Fatalf("operation calls = %q, want %q", operations.calls, want)
	}
}

func TestDockerAPIReportsUnsupportedDirectNetworkOperation(t *testing.T) {
	operations := &fakeNetworkOperations{
		errs: map[string]error{
			"disconnect backend demo false": fmt.Errorf(
				"%w: CNI endpoint lifecycle",
				ErrUnsupported,
			),
		},
	}
	response := httptest.NewRecorder()
	NewAPI(managerWithNetworkOperations(operations), "").ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodPost,
			"/v1.47/networks/backend/disconnect",
			strings.NewReader(`{"Container":"demo"}`),
		),
	)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("network disconnect response = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), ErrUnsupported.Error()) {
		t.Fatalf("network response did not expose typed error: %s", response.Body.String())
	}
}

func TestContainerdNetworkOperationsReturnTypedUnsupportedErrors(t *testing.T) {
	runtimeClient := &grpcContainerRuntime{}
	connectErr := runtimeClient.Connect(context.Background(), "backend", "demo", nil)
	if !errors.Is(connectErr, ErrUnsupported) ||
		!strings.Contains(connectErr.Error(), "CNI endpoints") {
		t.Fatalf("containerd connect error = %v", connectErr)
	}
	disconnectErr := runtimeClient.Disconnect(context.Background(), "backend", "demo", false)
	if !errors.Is(disconnectErr, ErrUnsupported) ||
		!strings.Contains(disconnectErr.Error(), "metadata alone") {
		t.Fatalf("containerd disconnect error = %v", disconnectErr)
	}
}

func TestContainerCapabilitiesReportUnsupportedDirectNetworkUpdates(t *testing.T) {
	capability := containerCapabilities().NetworkUpdates
	if capability.Supported || !strings.Contains(capability.Reason, "CNI endpoint") {
		t.Fatalf("network update capability = %+v", capability)
	}
}

func TestManagerRoutesLifecycleActionsThroughContainerOperations(t *testing.T) {
	operations := &fakeContainerOperations{errs: map[string]error{}}
	manager := managerWithContainerOperations(operations)
	actions := []struct {
		action  string
		timeout int
	}{
		{action: "start"},
		{action: "stop", timeout: 12},
		{action: "pause"},
		{action: "unpause"},
		{action: "restart", timeout: 3},
	}
	for _, action := range actions {
		if err := manager.ContainerActionWithTimeout(
			context.Background(),
			"demo",
			action.action,
			action.timeout,
		); err != nil {
			t.Fatalf("%s container: %v", action.action, err)
		}
	}
	want := []string{
		"start demo", "close",
		"stop demo 12", "close",
		"pause demo", "close",
		"resume demo", "close",
		"restart demo 3", "close",
	}
	if !reflect.DeepEqual(operations.calls, want) {
		t.Fatalf("operation calls = %q, want %q", operations.calls, want)
	}
}

func TestManagerRoutesKillSignalAndWaitThroughContainerOperations(t *testing.T) {
	operations := &fakeContainerOperations{waitCode: 137, errs: map[string]error{}}
	manager := managerWithContainerOperations(operations)
	if err := manager.KillContainer(context.Background(), "demo", "SIGUSR1"); err != nil {
		t.Fatalf("kill container: %v", err)
	}
	code, err := manager.WaitContainer(context.Background(), "demo", "not-running")
	if err != nil {
		t.Fatalf("wait container: %v", err)
	}
	if code != 137 {
		t.Fatalf("exit code = %d, want 137", code)
	}
	want := []string{"kill demo 10", "close", "wait demo", "close"}
	if !reflect.DeepEqual(operations.calls, want) {
		t.Fatalf("operation calls = %q, want %q", operations.calls, want)
	}
}

func TestDockerAPIKillRoutesNamedSignalThroughContainerOperations(t *testing.T) {
	operations := &fakeContainerOperations{errs: map[string]error{}}
	response := httptest.NewRecorder()
	NewAPI(managerWithContainerOperations(operations), "").ServeHTTP(
		response,
		httptest.NewRequest(http.MethodPost, "/v1.47/containers/demo/kill?signal=SIGTERM", nil),
	)
	if response.Code != http.StatusNoContent {
		t.Fatalf("kill response = %d: %s", response.Code, response.Body.String())
	}
	if want := []string{"kill demo 15", "close"}; !reflect.DeepEqual(operations.calls, want) {
		t.Fatalf("operation calls = %q, want %q", operations.calls, want)
	}
}

func TestManagerFallsBackWhenDirectOperationIsUnsupported(t *testing.T) {
	operations := &fakeContainerOperations{
		errs: map[string]error{"restart demo 0": ErrUnsupported},
	}
	runner := &fakeRunner{
		outputs: map[string][]byte{"nerdctl restart demo": nil},
		errors:  map[string]error{},
	}
	manager := New(runner)
	manager.operationsConnector = func(context.Context) (containerOperations, error) {
		return operations, nil
	}
	if err := manager.ContainerAction(context.Background(), "demo", "restart"); err != nil {
		t.Fatalf("restart container: %v", err)
	}
	if len(runner.commands) != 1 || !reflect.DeepEqual(runner.commands[0].Args, []string{"restart", "demo"}) {
		t.Fatalf("fallback commands = %+v", runner.commands)
	}
}

func TestManagerRoutesResourceUpdatesThroughContainerOperations(t *testing.T) {
	operations := &fakeContainerOperations{errs: map[string]error{}}
	manager := managerWithContainerOperations(operations)
	update := ContainerUpdate{
		NanoCPUs:   2_000_000_000,
		Memory:     1_024,
		MemorySwap: 2_048,
	}
	if err := manager.UpdateContainer(context.Background(), "demo", update); err != nil {
		t.Fatalf("update container: %v", err)
	}
	want := []string{"update demo cpu=2000000000 memory=1024 swap=2048", "close"}
	if !reflect.DeepEqual(operations.calls, want) {
		t.Fatalf("operation calls = %q, want %q", operations.calls, want)
	}
}

func TestManagerRoutesHealthUpdatesThroughMockableContainerOperations(t *testing.T) {
	operations := &fakeContainerOperations{errs: map[string]error{}}
	manager := managerWithContainerOperations(operations)
	err := manager.UpdateContainer(context.Background(), "demo", ContainerUpdate{
		Healthcheck: &ContainerHealthcheck{
			Test:     []string{"CMD-SHELL", "true"},
			Interval: 30 * time.Second,
			Timeout:  5 * time.Second,
			Retries:  3,
		},
	})
	if err != nil {
		t.Fatalf("update container health: %v", err)
	}
	want := []string{
		"update-health demo test=[CMD-SHELL true] interval=30s timeout=5s retries=3",
		"close",
	}
	if !reflect.DeepEqual(operations.calls, want) {
		t.Fatalf("operation calls = %q, want %q", operations.calls, want)
	}
}

func TestManagerDoesNotFallBackWhenDirectHealthUpdateIsUnsupported(t *testing.T) {
	call := "update-health demo test=[CMD-SHELL true] interval=30s timeout=5s retries=3"
	operations := &fakeContainerOperations{
		errs: map[string]error{call: ErrUnsupported},
	}
	runner := &fakeRunner{
		outputs: map[string][]byte{"nerdctl healthcheck demo": nil},
		errors:  map[string]error{},
	}
	manager := New(runner)
	manager.operationsConnector = func(context.Context) (containerOperations, error) {
		return operations, nil
	}
	err := manager.UpdateContainer(context.Background(), "demo", ContainerUpdate{
		Healthcheck: &ContainerHealthcheck{
			Test:     []string{"CMD-SHELL", "true"},
			Interval: 30 * time.Second,
			Timeout:  5 * time.Second,
			Retries:  3,
		},
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("health update error = %v, want ErrUnsupported", err)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("unsupported health update fell back to commands: %+v", runner.commands)
	}
}

func TestDockerAPIReportsUnsupportedDirectHealthUpdate(t *testing.T) {
	call := "update-health demo test=[CMD-SHELL true] interval=30s timeout=5s retries=3"
	operations := &fakeContainerOperations{
		errs: map[string]error{call: ErrUnsupported},
	}
	response := httptest.NewRecorder()
	NewAPI(managerWithContainerOperations(operations), "").ServeHTTP(
		response,
		httptest.NewRequest(
			http.MethodPost,
			"/v1.47/containers/demo/update",
			strings.NewReader(`{
				"Healthcheck":{
					"Test":["CMD-SHELL","true"],
					"Interval":30000000000,
					"Timeout":5000000000,
					"Retries":3
				}
			}`),
		),
	)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("health update response = %d: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), ErrUnsupported.Error()) {
		t.Fatalf("health update response did not expose typed error: %s", response.Body.String())
	}
}

func TestContainerdHealthUpdateReturnsTypedUnsupportedError(t *testing.T) {
	err := (&grpcContainerRuntime{}).UpdateHealth(
		context.Background(),
		"demo",
		&ContainerHealthcheck{Test: []string{"CMD-SHELL", "true"}},
	)
	if !errors.Is(err, ErrUnsupported) ||
		!strings.Contains(err.Error(), "scheduling and result logs") {
		t.Fatalf("containerd health update error = %v", err)
	}
}

func TestManagerFallsBackWhenDirectResourceUpdateIsUnsupported(t *testing.T) {
	operations := &fakeContainerOperations{
		errs: map[string]error{
			"update demo cpu=2000000000 memory=1024 swap=2048": ErrUnsupported,
		},
	}
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl update --cpus 2 --memory 1024 --memory-swap 2048 demo": nil,
		},
		errors: map[string]error{},
	}
	manager := New(runner)
	manager.operationsConnector = func(context.Context) (containerOperations, error) {
		return operations, nil
	}
	err := manager.UpdateContainer(context.Background(), "demo", ContainerUpdate{
		NanoCPUs:   2_000_000_000,
		Memory:     1_024,
		MemorySwap: 2_048,
	})
	if err != nil {
		t.Fatalf("update container: %v", err)
	}
	if len(runner.commands) != 1 || !reflect.DeepEqual(
		runner.commands[0].Args,
		[]string{"update", "--cpus", "2", "--memory", "1024", "--memory-swap", "2048", "demo"},
	) {
		t.Fatalf("fallback commands = %+v", runner.commands)
	}
}

func TestManagerRoutesMetadataActionsThroughContainerOperations(t *testing.T) {
	operations := &fakeContainerOperations{errs: map[string]error{}}
	manager := managerWithContainerOperations(operations)
	if err := manager.RenameContainer(context.Background(), "demo", "renamed"); err != nil {
		t.Fatalf("rename container: %v", err)
	}
	for _, action := range []string{"remove", "remove-force", "remove-volumes", "remove-force-volumes"} {
		if err := manager.ContainerAction(context.Background(), "demo", action); err != nil {
			t.Fatalf("%s container: %v", action, err)
		}
	}
	want := []string{
		"rename demo renamed", "close",
		"delete demo false false", "close",
		"delete demo true false", "close",
		"delete demo false true", "close",
		"delete demo true true", "close",
	}
	if !reflect.DeepEqual(operations.calls, want) {
		t.Fatalf("operation calls = %q, want %q", operations.calls, want)
	}
}

func TestManagerMetadataActionsFallbackOnlyForUnavailableOrUnsupported(t *testing.T) {
	tests := []struct {
		name         string
		directErr    error
		wantFallback bool
	}{
		{name: "unsupported", directErr: ErrUnsupported, wantFallback: true},
		{name: "unavailable", directErr: ErrUnavailable, wantFallback: true},
		{name: "other error", directErr: errors.New("metadata update failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operations := &fakeContainerOperations{
				errs: map[string]error{"rename demo renamed": test.directErr},
			}
			runner := &fakeRunner{
				outputs: map[string][]byte{"nerdctl rename demo renamed": nil},
				errors:  map[string]error{},
			}
			manager := New(runner)
			manager.operationsConnector = func(context.Context) (containerOperations, error) {
				return operations, nil
			}
			err := manager.RenameContainer(context.Background(), "demo", "renamed")
			if test.wantFallback {
				if err != nil {
					t.Fatalf("rename container: %v", err)
				}
				if len(runner.commands) != 1 {
					t.Fatalf("fallback commands = %+v, want one rename", runner.commands)
				}
			} else {
				if !errors.Is(err, test.directErr) {
					t.Fatalf("rename error = %v, want %v", err, test.directErr)
				}
				if len(runner.commands) != 0 {
					t.Fatalf("unexpected fallback commands = %+v", runner.commands)
				}
			}
		})
	}
}

func TestManagerRemoveFallsBackWhenDirectMetadataIsUnavailable(t *testing.T) {
	operations := &fakeContainerOperations{
		errs: map[string]error{"delete demo false false": ErrUnavailable},
	}
	runner := &fakeRunner{
		outputs: map[string][]byte{"nerdctl rm demo": nil},
		errors:  map[string]error{},
	}
	manager := New(runner)
	manager.operationsConnector = func(context.Context) (containerOperations, error) {
		return operations, nil
	}
	if err := manager.ContainerAction(context.Background(), "demo", "remove"); err != nil {
		t.Fatalf("remove container: %v", err)
	}
	if len(runner.commands) != 1 ||
		!reflect.DeepEqual(runner.commands[0].Args, []string{"rm", "demo"}) {
		t.Fatalf("fallback commands = %+v, want nerdctl rm demo", runner.commands)
	}
}

type fakeTasksClient struct {
	tasksapi.TasksClient
	process       *tasktypes.Process
	calls         []string
	killSignals   []uint32
	updateRequest *tasksapi.UpdateTaskRequest
	waitCode      uint32
	getErr        error
	updateErr     error
	deleteErr     error
}

func (f *fakeTasksClient) Get(
	_ context.Context,
	request *tasksapi.GetRequest,
	_ ...grpc.CallOption,
) (*tasksapi.GetResponse, error) {
	f.calls = append(f.calls, "get "+request.GetContainerID())
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &tasksapi.GetResponse{Process: f.process}, nil
}

func (f *fakeTasksClient) Resume(
	_ context.Context,
	request *tasksapi.ResumeTaskRequest,
	_ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	f.calls = append(f.calls, "resume "+request.GetContainerID())
	return &emptypb.Empty{}, nil
}

func (f *fakeTasksClient) Kill(
	_ context.Context,
	request *tasksapi.KillRequest,
	_ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	f.calls = append(f.calls, "kill "+request.GetContainerID())
	f.killSignals = append(f.killSignals, request.GetSignal())
	return &emptypb.Empty{}, nil
}

func (f *fakeTasksClient) Wait(
	_ context.Context,
	request *tasksapi.WaitRequest,
	_ ...grpc.CallOption,
) (*tasksapi.WaitResponse, error) {
	f.calls = append(f.calls, "wait "+request.GetContainerID())
	return &tasksapi.WaitResponse{ExitStatus: f.waitCode}, nil
}

func (f *fakeTasksClient) Update(
	_ context.Context,
	request *tasksapi.UpdateTaskRequest,
	_ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	f.calls = append(f.calls, "update "+request.GetContainerID())
	f.updateRequest = request
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return &emptypb.Empty{}, nil
}

func (f *fakeTasksClient) Delete(
	_ context.Context,
	request *tasksapi.DeleteTaskRequest,
	_ ...grpc.CallOption,
) (*tasksapi.DeleteResponse, error) {
	f.calls = append(f.calls, "delete "+request.GetContainerID())
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &tasksapi.DeleteResponse{}, nil
}

type fakeContainersClient struct {
	containersapi.ContainersClient
	container      *containersapi.Container
	calls          []string
	updateRequest  *containersapi.UpdateContainerRequest
	updateRequests []*containersapi.UpdateContainerRequest
	getErr         error
	updateErr      error
	deleteErr      error
}

func (f *fakeContainersClient) Get(
	_ context.Context,
	request *containersapi.GetContainerRequest,
	_ ...grpc.CallOption,
) (*containersapi.GetContainerResponse, error) {
	f.calls = append(f.calls, "get "+request.GetID())
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &containersapi.GetContainerResponse{Container: f.container}, nil
}

func (f *fakeContainersClient) Update(
	_ context.Context,
	request *containersapi.UpdateContainerRequest,
	_ ...grpc.CallOption,
) (*containersapi.UpdateContainerResponse, error) {
	f.calls = append(f.calls, "update "+request.GetContainer().GetID())
	f.updateRequest = request
	f.updateRequests = append(f.updateRequests, request)
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return &containersapi.UpdateContainerResponse{Container: request.GetContainer()}, nil
}

func (f *fakeContainersClient) Delete(
	_ context.Context,
	request *containersapi.DeleteContainerRequest,
	_ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	f.calls = append(f.calls, "delete "+request.GetID())
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return &emptypb.Empty{}, nil
}

func TestGRPCContainerOperationsResumePausedStart(t *testing.T) {
	tasks := &fakeTasksClient{process: &tasktypes.Process{
		ContainerID: "demo",
		Status:      tasktypes.Status_PAUSED,
	}}
	runtimeClient := &grpcContainerRuntime{namespace: "default", tasks: tasks}
	if err := runtimeClient.Start(context.Background(), "demo"); err != nil {
		t.Fatalf("start paused container: %v", err)
	}
	if want := []string{"get demo", "resume demo"}; !reflect.DeepEqual(tasks.calls, want) {
		t.Fatalf("task calls = %q, want %q", tasks.calls, want)
	}
}

func TestGRPCContainerOperationsStopUsesContainerSettings(t *testing.T) {
	tasks := &fakeTasksClient{
		process: &tasktypes.Process{ContainerID: "demo", Status: tasktypes.Status_RUNNING},
	}
	runtimeClient := &grpcContainerRuntime{
		namespace:  "default",
		tasks:      tasks,
		containers: &fakeContainersClient{container: &containersapi.Container{ID: "demo"}},
	}
	if err := runtimeClient.Stop(context.Background(), "demo", 2); err != nil {
		t.Fatalf("stop container: %v", err)
	}
	if want := []uint32{15}; !reflect.DeepEqual(tasks.killSignals, want) {
		t.Fatalf("kill signals = %v, want %v", tasks.killSignals, want)
	}
	if want := []string{"get demo", "kill demo", "wait demo"}; !reflect.DeepEqual(tasks.calls, want) {
		t.Fatalf("task calls = %q, want %q", tasks.calls, want)
	}
}

func TestGRPCContainerOperationsWaitReturnsCreatedTaskImmediately(t *testing.T) {
	tasks := &fakeTasksClient{process: &tasktypes.Process{
		ContainerID: "demo",
		Status:      tasktypes.Status_CREATED,
		ExitStatus:  7,
	}}
	runtimeClient := &grpcContainerRuntime{namespace: "default", tasks: tasks}
	code, err := runtimeClient.Wait(context.Background(), "demo")
	if err != nil {
		t.Fatalf("wait created container: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if want := []string{"get demo"}; !reflect.DeepEqual(tasks.calls, want) {
		t.Fatalf("task calls = %q, want %q", tasks.calls, want)
	}
}

func TestGRPCContainerOperationsRenamePreservesLabels(t *testing.T) {
	containers := &fakeContainersClient{container: &containersapi.Container{
		ID: "demo",
		Labels: map[string]string{
			"app":            "porto",
			nerdctlNameLabel: "old-name",
		},
	}}
	runtimeClient := &grpcContainerRuntime{namespace: "default", containers: containers}
	if err := runtimeClient.Rename(context.Background(), "demo", "new-name"); err != nil {
		t.Fatalf("rename container: %v", err)
	}
	if want := []string{"get demo", "update demo"}; !reflect.DeepEqual(containers.calls, want) {
		t.Fatalf("container calls = %q, want %q", containers.calls, want)
	}
	if got := containers.updateRequest.GetContainer().GetLabels(); !reflect.DeepEqual(got, map[string]string{
		"app":            "porto",
		nerdctlNameLabel: "new-name",
	}) {
		t.Fatalf("updated labels = %v", got)
	}
	if !reflect.DeepEqual(
		containers.updateRequest.GetUpdateMask(),
		&fieldmaskpb.FieldMask{Paths: []string{"labels"}},
	) {
		t.Fatalf("update mask = %v, want labels", containers.updateRequest.GetUpdateMask())
	}
}

func TestGRPCContainerOperationsUpdateLabelsMergesExistingMetadata(t *testing.T) {
	containers := &fakeContainersClient{container: &containersapi.Container{
		ID:     "demo",
		Labels: map[string]string{"existing": "value"},
	}}
	runtimeClient := &grpcContainerRuntime{namespace: "default", containers: containers}
	if err := runtimeClient.UpdateLabels(
		context.Background(),
		"demo",
		map[string]string{"added": "label"},
	); err != nil {
		t.Fatalf("update labels: %v", err)
	}
	want := map[string]string{"existing": "value", "added": "label"}
	if got := containers.updateRequest.GetContainer().GetLabels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("updated labels = %v, want %v", got, want)
	}
}

func TestGRPCContainerOperationsUpdatesTaskAndPersistedResources(t *testing.T) {
	shares := uint64(512)
	reservation := int64(256)
	specDocument, err := json.Marshal(specs.Spec{
		Linux: &specs.Linux{Resources: &specs.LinuxResources{
			CPU:    &specs.LinuxCPU{Shares: &shares},
			Memory: &specs.LinuxMemory{Reservation: &reservation},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks := &fakeTasksClient{process: &tasktypes.Process{
		ContainerID: "demo",
		Status:      tasktypes.Status_RUNNING,
	}}
	containers := &fakeContainersClient{container: &containersapi.Container{
		ID:   "demo",
		Spec: &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Spec", Value: specDocument},
	}}
	runtimeClient := &grpcContainerRuntime{
		namespace:  "default",
		tasks:      tasks,
		containers: containers,
	}
	err = runtimeClient.UpdateResources(context.Background(), "demo", ContainerUpdate{
		NanoCPUs:   1_500_000_000,
		Memory:     1_024,
		MemorySwap: 2_048,
	})
	if err != nil {
		t.Fatalf("update resources: %v", err)
	}
	if want := []string{"get demo", "update demo"}; !reflect.DeepEqual(tasks.calls, want) {
		t.Fatalf("task calls = %q, want %q", tasks.calls, want)
	}
	if want := []string{"get demo", "update demo"}; !reflect.DeepEqual(containers.calls, want) {
		t.Fatalf("container calls = %q, want %q", containers.calls, want)
	}
	if tasks.updateRequest.GetResources().GetTypeUrl() != containerdLinuxResourcesTypeURL {
		t.Fatalf("resource type URL = %q", tasks.updateRequest.GetResources().GetTypeUrl())
	}
	var taskResources specs.LinuxResources
	if err := json.Unmarshal(tasks.updateRequest.GetResources().GetValue(), &taskResources); err != nil {
		t.Fatalf("decode task resources: %v", err)
	}
	if taskResources.CPU == nil || taskResources.CPU.Quota == nil ||
		*taskResources.CPU.Quota != 150_000 || taskResources.CPU.Period == nil ||
		*taskResources.CPU.Period != 100_000 || taskResources.CPU.Shares == nil ||
		*taskResources.CPU.Shares != shares {
		t.Fatalf("updated CPU resources = %+v", taskResources.CPU)
	}
	if taskResources.Memory == nil || taskResources.Memory.Limit == nil ||
		*taskResources.Memory.Limit != 1_024 || taskResources.Memory.Swap == nil ||
		*taskResources.Memory.Swap != 2_048 || taskResources.Memory.Reservation == nil ||
		*taskResources.Memory.Reservation != reservation {
		t.Fatalf("updated memory resources = %+v", taskResources.Memory)
	}
	var persistedSpec specs.Spec
	if err := json.Unmarshal(
		containers.updateRequest.GetContainer().GetSpec().GetValue(),
		&persistedSpec,
	); err != nil {
		t.Fatalf("decode persisted spec: %v", err)
	}
	if persistedSpec.Linux == nil || !reflect.DeepEqual(persistedSpec.Linux.Resources, &taskResources) {
		t.Fatalf("persisted resources = %+v, task resources = %+v", persistedSpec.Linux, taskResources)
	}
	wantMask := &fieldmaskpb.FieldMask{Paths: []string{"spec"}}
	if !reflect.DeepEqual(
		containers.updateRequest.GetUpdateMask(),
		wantMask,
	) {
		t.Fatalf("update mask = %v, want %v", containers.updateRequest.GetUpdateMask(), wantMask)
	}
}

func TestGRPCContainerOperationsRollsBackMetadataForUnsupportedTaskUpdate(t *testing.T) {
	specDocument, err := json.Marshal(specs.Spec{
		Linux: &specs.Linux{Resources: &specs.LinuxResources{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks := &fakeTasksClient{
		process:   &tasktypes.Process{ContainerID: "demo", Status: tasktypes.Status_RUNNING},
		updateErr: status.Error(codes.Unimplemented, "task update unavailable"),
	}
	containers := &fakeContainersClient{container: &containersapi.Container{
		ID:   "demo",
		Spec: &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Spec", Value: specDocument},
	}}
	runtimeClient := &grpcContainerRuntime{
		namespace:  "default",
		tasks:      tasks,
		containers: containers,
	}
	err = runtimeClient.UpdateResources(context.Background(), "demo", ContainerUpdate{Memory: 1_024})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("update error = %v, want unsupported", err)
	}
	if want := []string{"get demo", "update demo", "update demo"}; !reflect.DeepEqual(
		containers.calls,
		want,
	) {
		t.Fatalf("container calls = %q, want %q", containers.calls, want)
	}
	if len(containers.updateRequests) != 2 ||
		!reflect.DeepEqual(
			containers.updateRequests[1].GetContainer().GetSpec().GetValue(),
			specDocument,
		) {
		t.Fatalf("container metadata was not rolled back: %+v", containers.updateRequests)
	}
}

func TestGRPCContainerOperationsKeepsMetadataWhenTaskExitsDuringUpdate(t *testing.T) {
	specDocument, err := json.Marshal(specs.Spec{
		Linux: &specs.Linux{Resources: &specs.LinuxResources{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks := &fakeTasksClient{
		process:   &tasktypes.Process{ContainerID: "demo", Status: tasktypes.Status_RUNNING},
		updateErr: status.Error(codes.NotFound, "task exited"),
	}
	containers := &fakeContainersClient{container: &containersapi.Container{
		ID:   "demo",
		Spec: &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Spec", Value: specDocument},
	}}
	runtimeClient := &grpcContainerRuntime{
		namespace:  "default",
		tasks:      tasks,
		containers: containers,
	}
	if err := runtimeClient.UpdateResources(
		context.Background(),
		"demo",
		ContainerUpdate{Memory: 1_024},
	); err != nil {
		t.Fatalf("update resources after task exit: %v", err)
	}
	if want := []string{"get demo", "update demo"}; !reflect.DeepEqual(containers.calls, want) {
		t.Fatalf("container calls = %q, want %q", containers.calls, want)
	}
}

func TestApplyContainerUpdateDefaultsSwapToTwiceMemory(t *testing.T) {
	resources := &specs.LinuxResources{}
	applyContainerUpdate(resources, ContainerUpdate{Memory: 1_024})
	if resources.Memory == nil || resources.Memory.Limit == nil ||
		*resources.Memory.Limit != 1_024 || resources.Memory.Swap == nil ||
		*resources.Memory.Swap != 2_048 {
		t.Fatalf("memory resources = %+v", resources.Memory)
	}
}

func TestGRPCContainerOperationsDeleteStoppedTaskBeforeMetadata(t *testing.T) {
	tasks := &fakeTasksClient{process: &tasktypes.Process{
		ContainerID: "demo",
		Status:      tasktypes.Status_STOPPED,
	}}
	containers := &fakeContainersClient{container: &containersapi.Container{ID: "demo"}}
	runtimeClient := &grpcContainerRuntime{
		namespace:  "default",
		tasks:      tasks,
		containers: containers,
	}
	if err := runtimeClient.Delete(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("delete container: %v", err)
	}
	if want := []string{"get demo", "delete demo"}; !reflect.DeepEqual(tasks.calls, want) {
		t.Fatalf("task calls = %q, want %q", tasks.calls, want)
	}
	if want := []string{"get demo", "delete demo"}; !reflect.DeepEqual(containers.calls, want) {
		t.Fatalf("container calls = %q, want %q", containers.calls, want)
	}
}

func TestGRPCContainerOperationsDeleteMetadataWithoutTask(t *testing.T) {
	tasks := &fakeTasksClient{getErr: status.Error(codes.NotFound, "task missing")}
	containers := &fakeContainersClient{container: &containersapi.Container{ID: "demo"}}
	runtimeClient := &grpcContainerRuntime{
		namespace:  "default",
		tasks:      tasks,
		containers: containers,
	}
	if err := runtimeClient.Delete(context.Background(), "demo", false, false); err != nil {
		t.Fatalf("delete container metadata: %v", err)
	}
	if want := []string{"get demo"}; !reflect.DeepEqual(tasks.calls, want) {
		t.Fatalf("task calls = %q, want %q", tasks.calls, want)
	}
	if want := []string{"get demo", "delete demo"}; !reflect.DeepEqual(containers.calls, want) {
		t.Fatalf("container calls = %q, want %q", containers.calls, want)
	}
}

func TestGRPCContainerOperationsDeleteFallsBackBeforeManagedCleanup(t *testing.T) {
	tasks := &fakeTasksClient{process: &tasktypes.Process{
		ContainerID: "demo",
		Status:      tasktypes.Status_STOPPED,
	}}
	containers := &fakeContainersClient{container: &containersapi.Container{
		ID:          "demo",
		Snapshotter: "overlayfs",
		SnapshotKey: "demo",
	}}
	runtimeClient := &grpcContainerRuntime{
		namespace:  "default",
		tasks:      tasks,
		containers: containers,
	}
	err := runtimeClient.Delete(context.Background(), "demo", false, false)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("delete error = %v, want unsupported", err)
	}
	if len(tasks.calls) != 0 {
		t.Fatalf("task was changed before fallback: %q", tasks.calls)
	}
	if want := []string{"get demo"}; !reflect.DeepEqual(containers.calls, want) {
		t.Fatalf("container calls = %q, want %q", containers.calls, want)
	}
}

func TestContainerdOperationErrorPreservesTypedErrors(t *testing.T) {
	tests := []struct {
		code codes.Code
		want error
	}{
		{code: codes.Canceled, want: context.Canceled},
		{code: codes.DeadlineExceeded, want: context.DeadlineExceeded},
		{code: codes.NotFound, want: ErrNotFound},
		{code: codes.FailedPrecondition, want: ErrConflict},
		{code: codes.Unavailable, want: ErrUnavailable},
		{code: codes.Unimplemented, want: ErrUnsupported},
	}
	for _, test := range tests {
		err := containerdOperationError("pause", "demo", status.Error(test.code, "failure"))
		if !errors.Is(err, test.want) {
			t.Fatalf("%s error = %v, want %v", test.code, err, test.want)
		}
	}
}

func TestParseContainerSignal(t *testing.T) {
	for value, want := range map[string]uint32{
		"":         9,
		"9":        9,
		"TERM":     15,
		"sigusr1":  10,
		"SIGWINCH": 28,
	} {
		got, err := parseContainerSignal(value)
		if err != nil || got != want {
			t.Fatalf("signal %q = %d, %v; want %d", value, got, err, want)
		}
	}
	if _, err := parseContainerSignal("SIGUNKNOWN"); err == nil {
		t.Fatal("invalid signal was accepted")
	}
}
