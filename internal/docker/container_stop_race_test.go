package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type exitingStopTasks struct {
	*fakeTasksClient
	signal  uint32
	exit    func()
	waits   int
	force   bool
	reaped  bool
	waitErr error
}

func (tasks *exitingStopTasks) Kill(ctx context.Context, request *tasksapi.KillRequest, options ...grpc.CallOption) (*emptypb.Empty, error) {
	response, err := tasks.fakeTasksClient.Kill(ctx, request, options...)
	if request.GetSignal() == tasks.signal {
		tasks.exit()
		return nil, status.Error(codes.NotFound, "process already finished")
	}
	return response, err
}

func (tasks *exitingStopTasks) Wait(ctx context.Context, request *tasksapi.WaitRequest, options ...grpc.CallOption) (*tasksapi.WaitResponse, error) {
	tasks.waits++
	if (tasks.signal == 9 || tasks.force) && tasks.waits == 1 {
		return nil, status.Error(codes.DeadlineExceeded, "synthetic grace interval expired")
	}
	if tasks.waitErr != nil {
		tasks.exit()
		return nil, tasks.waitErr
	}
	if tasks.reaped {
		tasks.exit()
		return nil, status.Error(codes.NotFound, "task was reaped after exiting")
	}
	return tasks.fakeTasksClient.Wait(ctx, request, options...)
}

func TestStopAcceptsAnExitBetweenGraceTimeoutAndForcedSignal(t *testing.T) {
	tasks := &exitingStopTasks{
		fakeTasksClient: &fakeTasksClient{process: &tasktypes.Process{ContainerID: "demo", Status: tasktypes.Status_RUNNING}},
		signal:          9,
	}
	tasks.exit = func() { tasks.process.Status = tasktypes.Status_STOPPED }
	backend := &grpcContainerRuntime{
		namespace: "default", tasks: tasks,
		containers: &fakeContainersClient{container: &containersapi.Container{ID: "demo"}},
	}
	if err := backend.Stop(context.Background(), "demo", 1); err != nil {
		t.Fatalf("the existing container stopped before SIGKILL but stop failed: %v", err)
	}
	if len(tasks.killSignals) != 2 || tasks.killSignals[0] != 15 || tasks.killSignals[1] != 9 {
		t.Fatalf("graceful/forced stop semantics changed: %v", tasks.killSignals)
	}
}

func TestStopDoesNotHideMissingSignalTargetsThatAreStillRunning(t *testing.T) {
	tasks := &exitingStopTasks{
		fakeTasksClient: &fakeTasksClient{process: &tasktypes.Process{ContainerID: "demo", Status: tasktypes.Status_RUNNING}},
		signal:          9, exit: func() {},
	}
	backend := &grpcContainerRuntime{
		namespace: "default", tasks: tasks,
		containers: &fakeContainersClient{container: &containersapi.Container{ID: "demo"}},
	}
	if err := backend.Stop(context.Background(), "demo", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unverified running task was treated as stopped: %v", err)
	}
}

func TestStopReconcilesExitedOrReapedTasksWithoutHidingOtherFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		signal     uint32
		force      bool
		reaped     bool
		taskState  tasktypes.Status
		taskErr    error
		recordErr  error
		wantErr    error
		cleanupErr bool
		waitErr    error
	}{
		{name: "graceful exit before SIGTERM", signal: 15, taskState: tasktypes.Status_STOPPED},
		{name: "reaped graceful wait", reaped: true, taskErr: status.Error(codes.NotFound, "task missing")},
		{name: "reaped forced wait", force: true, reaped: true, taskErr: status.Error(codes.NotFound, "task missing")},
		{name: "task missing but metadata retained", signal: 9, taskErr: status.Error(codes.NotFound, "task missing")},
		{name: "metadata also deleted", signal: 9, taskErr: status.Error(codes.NotFound, "task missing"), recordErr: status.Error(codes.NotFound, "container missing"), wantErr: ErrNotFound},
		{name: "cannot verify stopped state", signal: 9, taskErr: status.Error(codes.Unavailable, "shim unavailable"), wantErr: ErrUnavailable},
		{name: "replacement task is being created", signal: 9, taskState: tasktypes.Status_CREATED, wantErr: ErrNotFound},
		{name: "network cleanup fails", signal: 9, taskState: tasktypes.Status_STOPPED, cleanupErr: true},
		{name: "shim cancels wait after exit", taskState: tasktypes.Status_STOPPED, waitErr: status.Error(codes.Canceled, "shim wait stream closed after exit")},
		{name: "shim cancels forced wait after exit", force: true, taskState: tasktypes.Status_STOPPED, waitErr: status.Error(codes.Canceled, "shim wait stream closed after exit")},
		{name: "cancelled wait does not prove a running task stopped", taskState: tasktypes.Status_RUNNING, waitErr: status.Error(codes.Canceled, "shim wait stream closed"), wantErr: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			containers := &fakeContainersClient{container: &containersapi.Container{ID: "demo", Labels: map[string]string{}}}
			tasks := &exitingStopTasks{
				fakeTasksClient: &fakeTasksClient{process: &tasktypes.Process{ContainerID: "demo", Status: tasktypes.Status_RUNNING}},
				signal:          test.signal, force: test.force, reaped: test.reaped, waitErr: test.waitErr,
			}
			tasks.exit = func() {
				tasks.process.Status = test.taskState
				tasks.getErr = test.taskErr
				containers.getErr = test.recordErr
				if test.cleanupErr {
					containers.container.Labels[portoNetworkAliasLabel] = "invalid synthetic network metadata"
				}
			}
			backend := &grpcContainerRuntime{namespace: "default", tasks: tasks, containers: containers}
			err := backend.Stop(context.Background(), "demo", 1)
			if test.cleanupErr {
				if err == nil || !strings.Contains(err.Error(), "decode network aliases") {
					t.Fatalf("stop ignored failed network cleanup: %v", err)
				}
			} else if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("stop error = %v, want %v", err, test.wantErr)
				}
				if test.wantErr == ErrUnavailable && errors.Is(err, ErrNotFound) {
					t.Fatalf("unknown backend state was reported as a missing container: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStopExitRacePreservesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tasks := &exitingStopTasks{
		fakeTasksClient: &fakeTasksClient{process: &tasktypes.Process{ContainerID: "demo", Status: tasktypes.Status_RUNNING}},
		signal:          9,
	}
	tasks.exit = func() {
		tasks.process.Status = tasktypes.Status_STOPPED
		cancel()
	}
	backend := &grpcContainerRuntime{
		namespace: "default", tasks: tasks,
		containers: &fakeContainersClient{container: &containersapi.Container{ID: "demo"}},
	}
	if err := backend.Stop(ctx, "demo", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop replaced caller cancellation with successful cleanup: %v", err)
	}
}
