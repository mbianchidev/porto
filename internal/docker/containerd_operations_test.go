package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type fakeContainerOperations struct {
	calls    []string
	waitCode int
	errs     map[string]error
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

type fakeTasksClient struct {
	tasksapi.TasksClient
	process     *tasktypes.Process
	calls       []string
	killSignals []uint32
	waitCode    uint32
}

func (f *fakeTasksClient) Get(
	_ context.Context,
	request *tasksapi.GetRequest,
	_ ...grpc.CallOption,
) (*tasksapi.GetResponse, error) {
	f.calls = append(f.calls, "get "+request.GetContainerID())
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

type fakeContainersClient struct {
	containersapi.ContainersClient
	container *containersapi.Container
}

func (f *fakeContainersClient) Get(
	context.Context,
	*containersapi.GetContainerRequest,
	...grpc.CallOption,
) (*containersapi.GetContainerResponse, error) {
	return &containersapi.GetContainerResponse{Container: f.container}, nil
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
