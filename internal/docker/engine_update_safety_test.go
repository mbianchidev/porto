package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	namespacesapi "github.com/containerd/containerd/api/services/namespaces/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type updateNamespaces struct{ namespacesapi.NamespacesClient }

func (*updateNamespaces) List(context.Context, *namespacesapi.ListNamespacesRequest, ...grpc.CallOption) (*namespacesapi.ListNamespacesResponse, error) {
	return &namespacesapi.ListNamespacesResponse{Namespaces: []*namespacesapi.Namespace{
		{Name: "default"}, {Name: "unmanaged"},
	}}, nil
}

type updateTasks struct {
	tasksapi.TasksClient
	state tasktypes.Status
	err   error
}

func (tasks *updateTasks) List(ctx context.Context, _ *tasksapi.ListTasksRequest, _ ...grpc.CallOption) (*tasksapi.ListTasksResponse, error) {
	if tasks.err != nil {
		return nil, tasks.err
	}
	headers, _ := metadata.FromOutgoingContext(ctx)
	namespace := headers.Get(containerdNamespaceHeader)
	response := &tasksapi.ListTasksResponse{}
	if len(namespace) > 0 && namespace[len(namespace)-1] == "unmanaged" && tasks.state != tasktypes.Status_UNKNOWN {
		response.Tasks = []*tasktypes.Process{{ID: "synthetic-unmanaged-task", Status: tasks.state}}
	}
	return response, nil
}

func updateSafetyRuntime(t *testing.T, tasks *updateTasks) *grpcContainerRuntime {
	t.Helper()
	client, err := containerd.New("", containerd.WithServices(
		containerd.WithNamespaceClient(&updateNamespaces{}),
		containerd.WithTaskClient(tasks),
	))
	if err != nil {
		t.Fatal(err)
	}
	connection, err := grpc.NewClient("passthrough:///synthetic-update-guard", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if connection.GetState() != connectivity.Shutdown {
			if err := connection.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	return &grpcContainerRuntime{client: client, namespace: "default", connection: connection}
}

func TestPrepareEngineUpdatePreservesActiveAndUnobservableWorkloads(t *testing.T) {
	for _, test := range []struct {
		name         string
		state        tasktypes.Status
		inventoryErr error
		activeBuild  bool
		dataBusy     bool
	}{
		{name: "unmanaged running task", state: tasktypes.Status_RUNNING},
		{name: "manually paused task", state: tasktypes.Status_PAUSED},
		{name: "incomplete task inventory", inventoryErr: errors.New("synthetic namespace unavailable")},
		{name: "active BuildKit operation", activeBuild: true},
		{name: "active storage operation", dataBusy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{outputs: map[string][]byte{
				"limactl list porto-engine --json":                                                []byte(`{"name":"porto-engine","status":"Running"}`),
				`limactl shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`: []byte("synthetic-owner\n"),
			}}
			manager := NewWithStateDir(runner, t.TempDir())
			manager.lookPath = func(name string) (string, error) { return name, nil }
			manager.runtimeConnector = func(context.Context) (containerRuntime, error) {
				return updateSafetyRuntime(t, &updateTasks{state: test.state, err: test.inventoryErr}), nil
			}
			manager.dialBuildKit = buildKitControlTestDialer(t, &buildKitPlatformServer{active: test.activeBuild})
			detached := false
			manager.nativeClose = func(context.Context) error { detached = true; return nil }
			if err := manager.writeEngineState(engineState{
				Mode: "lima", Instance: engineInstanceName, OwnerID: "synthetic-owner", ResourceSaverToken: "synthetic-sleep-owner",
			}); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(manager.stateDir, engineStateFile)
			before, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if test.dataBusy {
				manager.dataMu.Lock()
				defer manager.dataMu.Unlock()
			}
			stopped, err := manager.PrepareEngineUpdate(context.Background())
			if err == nil || stopped {
				t.Fatalf("unsafe update was accepted: stopped=%t error=%v", stopped, err)
			}
			if test.inventoryErr == nil && !errors.Is(err, ErrConflict) {
				t.Fatalf("observable active workload was not reported as a lifecycle conflict: %v", err)
			}
			if detached {
				t.Fatal("refused update detached existing native files")
			}
			after, readErr := os.ReadFile(statePath)
			if readErr != nil || string(after) != string(before) {
				t.Fatalf("refused update changed saved ownership: %s, %v", after, readErr)
			}
			for _, command := range runner.commands {
				if strings.HasPrefix(strings.Join(command.Args, " "), "stop ") {
					t.Fatal("refused update stopped the engine")
				}
			}

		})
	}
}

func TestPrepareEngineUpdateHonorsCancellationWhileWaitingForInstallation(t *testing.T) {
	for _, fileLock := range []bool{false, true} {
		t.Run(map[bool]string{false: "mutex", true: "cross-process lock"}[fileLock], func(t *testing.T) {
			manager := NewWithStateDir(&fakeRunner{}, t.TempDir())
			var release func()
			if fileLock {
				lock, err := acquireEngineInstallLock(filepath.Join(manager.stateDir, engineLockFile))
				if err != nil {
					t.Fatal(err)
				}
				release = func() {
					if err := lock.Close(); err != nil {
						t.Error(err)
					}
				}
			} else {
				manager.installMu.Lock()
				release = manager.installMu.Unlock
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			received := false
			defer func() {
				release()
				if !received {
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("update preparation did not finish after releasing the fixture lock")
					}
				}
			}()
			go func() {
				_, err := manager.PrepareEngineUpdate(ctx)
				done <- err
			}()
			select {
			case err := <-done:
				received = true
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("update lock cancellation = %v", err)
				}
				if !manager.dataMu.TryLock() {
					t.Fatal("cancelled update retained the runtime data transaction")
				}
				manager.dataMu.Unlock()
			case <-time.After(300 * time.Millisecond):
				t.Error("cancelled update remained blocked on an installation lock")
			}
		})
	}
}
