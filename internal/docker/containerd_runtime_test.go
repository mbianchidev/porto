package docker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	eventsapi "github.com/containerd/containerd/api/services/events/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestContainerFromContainerdMapsTypedLifecycleState(t *testing.T) {
	memoryLimit := int64(512 * 1024 * 1024)
	cpuQuota := int64(50000)
	cpuPeriod := uint64(100000)
	specDocument, err := json.Marshal(specs.Spec{
		Process: &specs.Process{Args: []string{"sleep", "30"}},
		Linux: &specs.Linux{Resources: &specs.LinuxResources{
			CPU:    &specs.LinuxCPU{Quota: &cpuQuota, Period: &cpuPeriod, Cpus: "0-1"},
			Memory: &specs.LinuxMemory{Limit: &memoryLimit},
		}},
		Mounts: []specs.Mount{{Type: "bind", Source: "/tmp/data", Destination: "/data", Options: []string{"rw"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	exited := created.Add(time.Minute)
	container := containerFromContainerd(
		&containersapi.Container{
			ID:        "abc",
			Image:     "docker.io/library/alpine:latest",
			Spec:      &anypb.Any{Value: specDocument},
			CreatedAt: timestamppb.New(created),
			UpdatedAt: timestamppb.New(exited),
			Labels: map[string]string{
				nerdctlNameLabel:        "demo",
				nerdctlNetworksLabel:    `["bridge"]`,
				nerdctlPortsLabel:       `[{"hostIP":"127.0.0.1","hostPort":8080,"containerPort":80,"protocol":"tcp"}]`,
				nerdctlHealthcheckLabel: `{"Test":["CMD","true"]}`,
				nerdctlHealthStateLabel: `{"Status":"unhealthy","FailingStreak":2}`,
				restartPolicyLabel:      "always",
				restartCountLabel:       "3",
			},
		},
		&tasktypes.Process{
			ContainerID: "abc",
			Pid:         42,
			Status:      tasktypes.Status_STOPPED,
			ExitStatus:  137,
			ExitedAt:    timestamppb.New(exited),
		},
	)
	if container.Name != "demo" || container.State != "exited" || !container.TaskPresent {
		t.Fatalf("unexpected container state: %+v", container)
	}
	if container.ExitSignal == nil || *container.ExitSignal != 9 || container.ExitReason != "signal" {
		t.Fatalf("unexpected exit metadata: %+v", container)
	}
	if container.RestartPolicy != "always" || container.RestartCount != 3 {
		t.Fatalf("unexpected restart metadata: %+v", container)
	}
	if container.Health.Status != "unhealthy" || container.Health.FailingStreak != 2 {
		t.Fatalf("unexpected health metadata: %+v", container.Health)
	}
	if container.Resources.MemoryLimit != memoryLimit || container.Resources.CPUQuota != cpuQuota {
		t.Fatalf("unexpected resource metadata: %+v", container.Resources)
	}
	if container.Ports != "127.0.0.1:8080->80/tcp" || container.Networks != "bridge" {
		t.Fatalf("unexpected network metadata: ports=%q networks=%q", container.Ports, container.Networks)
	}
	if len(container.MountDetails) != 1 || container.MountDetails[0].Destination != "/data" {
		t.Fatalf("unexpected mounts: %+v", container.MountDetails)
	}
}

func TestContainerLifecycleEventDecodesExecExitAndOOM(t *testing.T) {
	exited := time.Date(2026, 9, 1, 12, 1, 0, 0, time.UTC)
	exitPayload, err := anypb.New(&eventtypes.TaskExit{
		ContainerID: "abc",
		ID:          "exec-1",
		ExitStatus:  143,
		ExitedAt:    timestamppb.New(exited),
	})
	if err != nil {
		t.Fatal(err)
	}
	exitEvent, relevant := containerLifecycleEvent(&eventsapi.Envelope{
		Topic:     "/tasks/exit",
		Timestamp: timestamppb.New(exited.Add(-time.Second)),
		Event:     exitPayload,
	})
	if !relevant || exitEvent.Type != "exec-exit" || exitEvent.ExecID != "exec-1" {
		t.Fatalf("unexpected exec event: %+v", exitEvent)
	}
	if exitEvent.ExitSignal == nil || *exitEvent.ExitSignal != 15 || !exitEvent.Timestamp.Equal(exited) {
		t.Fatalf("unexpected exec exit metadata: %+v", exitEvent)
	}

	oomPayload, err := anypb.New(&eventtypes.TaskOOM{ContainerID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	oomEvent, relevant := containerLifecycleEvent(&eventsapi.Envelope{
		Topic:     "/tasks/oom",
		Timestamp: timestamppb.New(exited),
		Event:     oomPayload,
	})
	if !relevant || !oomEvent.OOM || oomEvent.ContainerID != "abc" || oomEvent.Reason != "oom" {
		t.Fatalf("unexpected OOM event: %+v", oomEvent)
	}
}

func TestContainerLifecycleEventTreatsInitProcessAsTaskExit(t *testing.T) {
	payload, err := anypb.New(&eventtypes.TaskExit{
		ContainerID: "abc",
		ID:          "abc",
		ExitStatus:  0,
		ExitedAt:    timestamppb.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	event, relevant := containerLifecycleEvent(&eventsapi.Envelope{
		Topic: "/tasks/exit",
		Event: payload,
	})
	if !relevant || event.Type != "task-exit" {
		t.Fatalf("init process exit was misclassified: %+v", event)
	}
}

func TestContainerLifecycleEventKeepsPartialRelevantPayload(t *testing.T) {
	event, relevant := containerLifecycleEvent(&eventsapi.Envelope{
		Topic:     "/containers/update",
		Timestamp: timestamppb.Now(),
		Event:     &anypb.Any{Value: []byte("invalid protobuf")},
	})
	if !relevant || event.Reason == "" {
		t.Fatalf("partial event was not retained: %+v", event)
	}
}

func TestCompatibilityMetadataRefreshesOnlyAfterMetadataChanges(t *testing.T) {
	enrichCalls := 0
	runtimeClient := &grpcContainerRuntime{
		enrich: func(context.Context) ([]Container, error) {
			enrichCalls++
			return []Container{{ID: "abc", Ports: "8080->80/tcp"}}, nil
		},
		enrichmentVersion: map[string]string{},
		enrichment:        map[string]Container{},
	}
	created := timestamppb.Now()
	records := []*containersapi.Container{{ID: "abc", UpdatedAt: created}}
	first, err := runtimeClient.compatibilityMetadata(context.Background(), records)
	if err != nil {
		t.Fatalf("initial enrichment: %v", err)
	}
	second, err := runtimeClient.compatibilityMetadata(context.Background(), records)
	if err != nil {
		t.Fatalf("cached enrichment: %v", err)
	}
	if enrichCalls != 1 || first["abc"].Ports == "" || second["abc"].Ports == "" {
		t.Fatalf("unexpected cached enrichment: calls=%d first=%+v second=%+v", enrichCalls, first, second)
	}
	runtimeClient.invalidateCompatibilityMetadata()
	if _, err := runtimeClient.compatibilityMetadata(context.Background(), records); err != nil {
		t.Fatalf("lifecycle enrichment refresh: %v", err)
	}
	if enrichCalls != 2 {
		t.Fatalf("enrichment calls = %d, want 2 after lifecycle invalidation", enrichCalls)
	}
	records[0].UpdatedAt = timestamppb.New(created.AsTime().Add(time.Second))
	if _, err := runtimeClient.compatibilityMetadata(context.Background(), records); err != nil {
		t.Fatalf("refreshed enrichment: %v", err)
	}
	if enrichCalls != 3 {
		t.Fatalf("enrichment calls = %d, want 3 after metadata update", enrichCalls)
	}
}

func TestMergeCompatibilityMetadataRestoresStoppedTaskAndPorts(t *testing.T) {
	container := Container{ID: "abc", State: "created"}
	mergeContainerCompatibilityMetadata(&container, Container{
		ID:       "abc",
		State:    "exited",
		Status:   "Exited (137) 2 seconds ago",
		Ports:    "127.0.0.1:8080->80/tcp",
		Networks: "bridge",
	})
	if container.State != "exited" || container.ExitCode == nil || *container.ExitCode != 137 {
		t.Fatalf("stopped state was not restored: %+v", container)
	}
	if container.ExitSignal == nil || *container.ExitSignal != 9 {
		t.Fatalf("exit signal was not restored: %+v", container)
	}
	if len(container.NetworkDetails) != 1 ||
		container.NetworkDetails[0].HostPort != 8080 ||
		container.NetworkDetails[0].ContainerPort != 80 {
		t.Fatalf("published ports were not structured: %+v", container.NetworkDetails)
	}
}

func TestNerdctlCompatibilityMarksRunningTask(t *testing.T) {
	compatibility, err := decodeNerdctlContainers([]byte(
		`{"ID":"abc","Names":"demo","State":"Up","Status":"Up","PID":"42"}` + "\n",
	))
	if err != nil {
		t.Fatalf("decode nerdctl container: %v", err)
	}
	if len(compatibility) != 1 || compatibility[0].State != "running" ||
		!compatibility[0].TaskPresent || compatibility[0].PID != 42 {
		t.Fatalf("running compatibility state = %+v", compatibility)
	}
	container := Container{ID: "abc", State: "created"}
	mergeContainerCompatibilityMetadata(&container, compatibility[0])
	if container.State != "running" || !container.TaskPresent || container.PID != 42 {
		t.Fatalf("running task metadata was not merged: %+v", container)
	}
}
