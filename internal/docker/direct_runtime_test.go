package docker

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	coreimages "github.com/containerd/containerd/v2/core/images"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestDirectCreateValidationSupportsHostAndNoneNetworks(t *testing.T) {
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

func TestDirectCNIConnectAndDisconnectPersistStructuredState(t *testing.T) {
	connectKey := "/helper cni-connect --network backend --container demo --netns /proc/42/ns/net --aliases api"
	disconnectKey := "/helper cni-disconnect --network backend --container demo --netns /proc/42/ns/net --aliases api"
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
		if _, ok := labels[key]; ok {
			t.Fatalf("restart label %s remains in %v", key, labels)
		}
	}
}

var _ runtimes.Runner = (*fakeRunner)(nil)
