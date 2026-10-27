package docker

import (
	"context"
	"testing"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
)

func TestContainerdSnapshotRetainsRealCompatibilityPublishedRoutes(t *testing.T) {
	calls := 0
	runtimeClient := &grpcContainerRuntime{
		namespace: "synthetic-namespace",
		containers: &fakeContainersClient{container: &containersapi.Container{
			ID: "synthetic-routed-container", Labels: map[string]string{
				nerdctlNameLabel: "synthetic-api", "dev.porto.http-port": "8080",
			},
		}},
		tasks: &fakeTasksClient{listProcesses: []*tasktypes.Process{
			{ID: "synthetic-routed-container", Pid: 42, Status: tasktypes.Status_RUNNING},
		}},
		enrich: func(context.Context) ([]Container, error) {
			calls++
			return []Container{{ID: "synthetic-routed-container", Ports: "127.0.0.1:41001->8080/tcp"}}, nil
		},
	}
	for range 2 {
		containers, err := runtimeClient.Snapshot(context.Background())
		if err != nil || len(containers) != 1 {
			t.Fatalf("snapshot=%+v, error=%v", containers, err)
		}
		found := false
		for _, network := range containers[0].NetworkDetails {
			found = found || network.HostIP == "127.0.0.1" && network.HostPort == 41001 && network.ContainerPort == 8080 && network.Protocol == "tcp"
		}
		if !found {
			t.Fatalf("actual compatibility-published route was lost from the direct snapshot: %+v", containers[0])
		}
	}
	if calls != 1 {
		t.Fatalf("stable inventory launched %d compatibility reads, want one cached read", calls)
	}
}
