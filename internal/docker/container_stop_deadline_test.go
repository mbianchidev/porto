package docker

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type canceledGraceTasks struct {
	*fakeTasksClient
	waits int
}

func (tasks *canceledGraceTasks) Wait(ctx context.Context, request *tasksapi.WaitRequest, _ ...grpc.CallOption) (*tasksapi.WaitResponse, error) {
	tasks.calls = append(tasks.calls, "wait "+request.ContainerID)
	tasks.waits++
	if tasks.waits == 1 {
		<-ctx.Done()
		return nil, status.Error(codes.Canceled, "shim wait canceled at grace deadline")
	}
	return &tasksapi.WaitResponse{ExitStatus: 137}, nil
}

func TestStopUsesOwnGraceDeadlineWithoutOverridingCallerCancellation(t *testing.T) {
	for _, callerDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "expired grace", true: "caller deadline"}[callerDeadline], func(t *testing.T) {
			tasks := &canceledGraceTasks{fakeTasksClient: &fakeTasksClient{process: &tasktypes.Process{ContainerID: "demo", Status: tasktypes.Status_RUNNING}}}
			backend := &grpcContainerRuntime{namespace: "default", tasks: tasks,
				containers: &fakeContainersClient{container: &containersapi.Container{ID: "demo"}}}
			ctx := context.Background()
			if callerDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
			}
			err := backend.Stop(ctx, "demo", 1)
			expected := []uint32{15, 9}
			if callerDeadline {
				expected = []uint32{15}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("caller cancellation was lost: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(tasks.killSignals, expected) {
				t.Fatalf("stop signals = %v, want %v", tasks.killSignals, expected)
			}
		})
	}
}
