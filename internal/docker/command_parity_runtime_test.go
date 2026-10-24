package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	corecontainers "github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/moby/moby/api/pkg/stdcopy"
	dockerclient "github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestDockerSDKAttachReplaysHistoricalOutput(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl container inspect demo": []byte(`[{"Id":"demo","Config":{"Tty":false},"State":{"Status":"exited"}}]`),
		},
		ordered: map[string][]runtimes.OutputChunk{
			"nerdctl logs --tail all demo": {
				{Stream: "stdout", Data: []byte("synthetic stdout\n")},
				{Stream: "stderr", Data: []byte("synthetic stderr\n")},
			},
		},
	}
	client := dockerSDKClient(t, startDockerTestAPI(t, New(runner)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attached, err := client.ContainerAttach(ctx, "demo", dockerclient.ContainerAttachOptions{
		Logs: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	var stdout, stderr strings.Builder
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attached.Reader); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "synthetic stdout\n" || stderr.String() != "synthetic stderr\n" {
		t.Fatalf("attach output = %q, %q", stdout.String(), stderr.String())
	}
}

func TestDirectContainerSpecKeepsReadOnlyRootAndInitialTTYSize(t *testing.T) {
	request := CreateContainerRequest{
		ReadOnly: true, TTY: true, ConsoleSize: [2]uint{30, 100},
		Networks: []ContainerNetwork{{Name: directNetworkNone}},
	}
	spec := &oci.Spec{}
	for _, option := range directContainerSpecOptions(request, nil, "", "linux/arm64", ocispec.ImageConfig{}, true) {
		if err := option(namespaces.WithNamespace(context.Background(), "default"), nil, &corecontainers.Container{ID: "synthetic"}, spec); err != nil {
			t.Fatal(err)
		}
	}
	if !spec.Root.Readonly || !spec.Process.Terminal || spec.Process.ConsoleSize == nil ||
		spec.Process.ConsoleSize.Height != 30 || spec.Process.ConsoleSize.Width != 100 {
		t.Fatalf("root and terminal = %+v %+v", spec.Root, spec.Process)
	}
}

func TestCombinedResourcePolicyUpdateRollsBackBothOnTaskFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rollback"}[fail], func(t *testing.T) {
			originalSpec, err := json.Marshal(specs.Spec{Linux: &specs.Linux{Resources: &specs.LinuxResources{}}})
			if err != nil {
				t.Fatal(err)
			}
			originalLabels := map[string]string{portoLogPathLabel: "/synthetic/log", restartPolicyLabel: "no", "unrelated": "keep"}
			containers := &fakeContainersClient{container: &containersapi.Container{
				ID: "demo", Labels: cloneStringMap(originalLabels),
				Spec: &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Spec", Value: originalSpec},
			}}
			tasks := &fakeTasksClient{process: &tasktypes.Process{ContainerID: "demo", Status: tasktypes.Status_RUNNING}}
			if fail {
				tasks.updateErr = status.Error(codes.Unimplemented, "synthetic cgroup failure")
			}
			runtime := &grpcContainerRuntime{namespace: "default", containers: containers, tasks: tasks}
			pids := int64(64)
			err = runtime.UpdateResources(context.Background(), "demo", ContainerUpdate{
				Memory: 268435456, MemorySwap: -1, CPUShares: 512, CPUSetCPUs: "0-1",
				MemoryReservation: 134217728, PidsLimit: &pids, Restart: "always",
			})
			if fail {
				if !errors.Is(err, ErrUnsupported) || !reflect.DeepEqual(containers.container.Labels, originalLabels) ||
					!reflect.DeepEqual(containers.container.Spec.Value, originalSpec) {
					t.Fatalf("update did not roll back spec and policy: %v; %+v", err, containers.container)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var persisted specs.Spec
			if err := json.Unmarshal(containers.container.Spec.Value, &persisted); err != nil {
				t.Fatal(err)
			}
			resources := persisted.Linux.Resources
			if *resources.Memory.Limit != 268435456 || *resources.Memory.Swap != -1 ||
				*resources.CPU.Shares != 512 || resources.CPU.Cpus != "0-1" ||
				*resources.Pids.Limit != 64 || containers.container.Labels[restartPolicyLabel] != "always" ||
				containers.container.Labels["unrelated"] != "keep" {
				t.Fatalf("combined update lost resources or labels: %+v, %v", resources, containers.container.Labels)
			}
		})
	}
}

func TestDockerRecursiveMountAndGPUOptionsReachCompatibilityRuntime(t *testing.T) {
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		got := strings.Join(command.Args, " ")
		for _, option := range []string{
			"--mount type=bind,source=/synthetic,target=/data,readonly,bind-propagation=rprivate,bind-recursive=readonly",
			`--gpus "device=GPU-synthetic-1,GPU-synthetic-2"`,
			"--security-opt apparmor=synthetic-profile",
		} {
			if !strings.Contains(got, option) {
				t.Errorf("missing %q in %s", option, got)
			}
		}
		return []byte("synthetic-container\n"), nil
	}}
	body := `{"Image":"synthetic","HostConfig":{
		"Mounts":[{"Type":"bind","Source":"/synthetic","Target":"/data","ReadOnly":true,"BindOptions":{"ReadOnlyForceRecursive":true}}],
		"DeviceRequests":[{"DeviceIDs":["GPU-synthetic-1","GPU-synthetic-2"],"Capabilities":[["gpu"]]}],
		"SecurityOpt":["apparmor=synthetic-profile"]}}`
	response := httptest.NewRecorder()
	NewAPI(New(runner), "").ServeHTTP(response, httptest.NewRequest("POST", "/v1.47/containers/create", strings.NewReader(body)))
	if response.Code != 201 {
		t.Fatalf("create = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerCLIExtendedCommandsUseThePortoContext(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		switch {
		case strings.HasPrefix(args, "top "):
			return []byte("PID COMMAND\n42 sleep 60\n"), nil
		case strings.HasPrefix(args, "diff "):
			return []byte("A /synthetic\n"), nil
		case strings.HasPrefix(args, "history "):
			return []byte("{\"Snapshot\":\"sha256:layer\",\"CreatedAt\":\"2026-01-01T00:00:00Z\",\"CreatedBy\":\"synthetic\",\"Size\":\"42\"}\n"), nil
		case strings.HasPrefix(args, "image inspect "):
			return []byte(`[{"Id":"sha256:config","RepoTags":["synthetic:latest"]}]`), nil
		case strings.HasPrefix(args, "ps "):
			return []byte("{\"ID\":\"demo\",\"Names\":\"demo\",\"State\":\"running\",\"CreatedAt\":\"2026-01-01T00:00:00Z\"}\n"), nil
		case strings.HasPrefix(args, "create "):
			return []byte("synthetic-container\n"), nil
		case strings.HasPrefix(args, "images "), strings.HasPrefix(args, "image rm "):
			return nil, nil
		default:
			return nil, errors.New("unexpected command: " + args)
		}
	}}
	operations := &parityOperations{fakeContainerOperations: &fakeContainerOperations{}}
	manager := New(runner)
	manager.operationsConnector = func(context.Context) (containerOperations, error) { return operations, nil }
	endpoint := startDockerTestAPI(t, manager)
	environment := append(os.Environ(), "DOCKER_CONFIG="+t.TempDir(), "DOCKER_CONTEXT=", "DOCKER_HOST=", "DOCKER_TLS_VERIFY=", "DOCKER_CERT_PATH=")
	run := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "docker", args...)
		command.Env = environment
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), errors.Join(err, ctx.Err()), output)
		}
	}
	run("context", "create", "porto", "--docker", "host="+endpoint)
	for _, args := range [][]string{
		{"top", "demo"}, {"diff", "demo"}, {"history", "synthetic:latest"},
		{"ps", "--all", "--size", "--last", "1"}, {"images", "--all"}, {"rmi", "--no-prune", "synthetic:latest"},
		{"checkpoint", "create", "demo", "saved"}, {"checkpoint", "ls", "demo"}, {"checkpoint", "rm", "demo", "saved"},
		{"update", "--cpu-shares", "512", "--pids-limit", "64", "--memory", "256m", "--restart", "always", "demo"},
		{"create", "--cpus", "1.5", "--memory", "256m", "--pids-limit", "64", "--read-only", "--cap-add", "NET_ADMIN", "synthetic:latest", "true"},
	} {
		run(append([]string{"--context", "porto"}, args...)...)
	}
}
