package kubernetes_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type appleAcceptanceRunner struct {
	runtimes.ExecRunner
	mu            sync.Mutex
	failStartNode string
}

func (r *appleAcceptanceRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	if filepath.Base(command.Name) == "container" {
		if len(command.Args) > 1 && (command.Args[0] == "stop" && slices.Contains(command.Args, "--all") ||
			command.Args[0] == "system" && command.Args[1] == "stop") {
			return nil, errors.New("acceptance forbids global native shutdown")
		}
		r.mu.Lock()
		if len(command.Args) == 2 && command.Args[0] == "start" && command.Args[1] == r.failStartNode {
			r.failStartNode = ""
			r.mu.Unlock()
			return nil, errors.New("synthetic native worker start failure")
		}
		r.mu.Unlock()
	}
	return r.ExecRunner.Run(ctx, command)
}

func (r *appleAcceptanceRunner) failNextStart(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failStartNode = node
}

type appleIsolationControl struct {
	cluster    kubernetes.Cluster
	manager    *kubernetes.Manager
	unrelated  string
	startDates map[string]string
	owner      string
}

func newAppleIsolationControl(t *testing.T) *appleIsolationControl {
	t.Helper()
	root, err := os.MkdirTemp("", "porto-native-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTO_HOME", root)
	runner := runtimes.ExecRunner{}
	p := kubernetes.NewClusterProvisioner(nil, runner, filepath.Join(root, "kubernetes"))
	name := fmt.Sprintf("apple-control-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cluster, err := p.Create(ctx, kubernetes.ClusterRequest{
		Name: name, Provider: "apple", ControlPlane: kubernetes.MachineSpec{CPUs: 2, MemoryMiB: 3072},
	})
	if err != nil {
		t.Fatalf("create untouched control cluster; retained state %s: %v", root, err)
	}
	control := &appleIsolationControl{cluster: cluster, manager: kubernetes.NewWithKubeconfigRoot(runner, filepath.Join(root, "kubernetes")),
		unrelated: fmt.Sprintf("porto-unrelated-%d", time.Now().UnixNano()), owner: rand.Text()}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Minute)
		defer stop()
		if err := p.Delete(cleanup, name); err != nil {
			t.Errorf("control cluster cleanup failed; ownership retained at %s: %v", root, err)
			return
		}
		if control.startDates != nil {
			output, err := runner.Run(cleanup, runtimes.Command{Name: "container", Args: []string{"inspect", control.unrelated}, StdoutOnly: true})
			var actual []struct {
				Configuration struct {
					Labels map[string]string `json:"labels"`
				} `json:"configuration"`
			}
			decodeErr := json.Unmarshal(output, &actual)
			if err != nil || decodeErr != nil || len(actual) != 1 || actual[0].Configuration.Labels["dev.porto.fixture-id"] != control.owner {
				t.Errorf("unrelated fixture ownership cannot be checked: %v", err)
				return
			}
			if output, err := runner.Run(cleanup, runtimes.Command{Name: "container", Args: []string{"kill", "--signal", "TERM", control.unrelated}}); err != nil {
				t.Errorf("gracefully stop exact unrelated fixture: %v: %s", err, output)
				return
			}
			deadline := time.Now().Add(time.Minute)
			for {
				states := appleContainerCheckpoints(t, cleanup, runner, []string{control.unrelated})
				if states[control.unrelated].State == "stopped" {
					break
				}
				if time.Now().After(deadline) {
					t.Error("unrelated fixture did not stop gracefully; no force was used")
					return
				}
				time.Sleep(time.Second)
			}
			if output, err := runner.Run(cleanup, runtimes.Command{Name: "container", Args: []string{"delete", control.unrelated}}); err != nil {
				t.Errorf("delete exact stopped unrelated fixture: %v: %s", err, output)
				return
			}
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	const image = "docker.io/library/busybox:1.37.0"
	if output, err := runner.Run(ctx, runtimes.Command{Name: "container", Args: []string{"image", "pull", "--platform", "linux/arm64", image}, Network: true}); err != nil {
		t.Fatalf("pull control fixture image: %v: %s", err, output)
	}
	if err := p.ImportImage(ctx, name, image); err != nil {
		t.Fatal(err)
	}
	const workload = `apiVersion: v1
kind: Pod
metadata:
  name: unchanged-control
spec:
  containers:
    - name: app
      image: docker.io/library/busybox:1.37.0
      imagePullPolicy: Never
      command: [sh, -c, "mkdir -p /www; printf unchanged-control-data > /www/marker; exec httpd -f -p 8080 -h /www"]
`
	base := []string{"--kubeconfig", cluster.KubeconfigPath, "--context", cluster.Context}
	for _, args := range [][]string{{"apply", "-f", "-"}, {"wait", "--for=condition=Ready", "pod/unchanged-control", "--timeout=5m"}} {
		var input []byte
		if args[0] == "apply" {
			input = []byte(workload)
		}
		if output, err := runner.Run(ctx, runtimes.Command{Name: "kubectl", Args: append(slices.Clone(base), args...), Stdin: input}); err != nil {
			t.Fatalf("control workload setup: %v: %s", err, output)
		}
	}
	script := `printf unchanged-unrelated-data > /tmp/marker; trap 'sync; exit 0' TERM INT; while :; do sleep 1; done`
	if output, err := runner.Run(ctx, runtimes.Command{Name: "container", Args: []string{
		"run", "--detach", "--name", control.unrelated, "--label", "dev.porto.fixture=unrelated",
		"--label", "dev.porto.fixture-id=" + control.owner,
		"--cpus", "1", "--memory", "256m", "--entrypoint", "/bin/sh", image, "-c", script,
	}}); err != nil {
		t.Fatalf("create exact unrelated fixture: %v: %s", err, output)
	}
	ids := append(slices.Clone(cluster.Nodes), control.unrelated)
	control.startDates = make(map[string]string)
	for id, checkpoint := range appleContainerCheckpoints(t, ctx, runner, ids) {
		control.startDates[id] = checkpoint.Started
	}
	control.assertUnaffected(t, ctx)
	return control
}

type appleContainerCheckpoint struct {
	State, Started string
}

func appleContainerCheckpoints(t *testing.T, ctx context.Context, runner runtimes.Runner, ids []string) map[string]appleContainerCheckpoint {
	t.Helper()
	output, err := runner.Run(ctx, runtimes.Command{Name: "container", Args: append([]string{"inspect"}, ids...), StdoutOnly: true})
	if err != nil {
		t.Fatal(runtimes.CommandError("inspect exact acceptance resources", output, err))
	}
	var containers []struct {
		ID     string `json:"id"`
		Status struct {
			State   string `json:"state"`
			Started string `json:"startedDate"`
		} `json:"status"`
	}
	if err := json.Unmarshal(output, &containers); err != nil {
		t.Fatal(err)
	}
	result := make(map[string]appleContainerCheckpoint)
	for _, item := range containers {
		result[item.ID] = appleContainerCheckpoint{State: item.Status.State, Started: item.Status.Started}
	}
	if len(result) != len(ids) {
		t.Fatal("acceptance resource inventory is incomplete")
	}
	return result
}

func (control *appleIsolationControl) assertUnaffected(t *testing.T, ctx context.Context) {
	t.Helper()
	runner := runtimes.ExecRunner{}
	ids := append(slices.Clone(control.cluster.Nodes), control.unrelated)
	for id, checkpoint := range appleContainerCheckpoints(t, ctx, runner, ids) {
		if checkpoint.State != "running" || checkpoint.Started != control.startDates[id] {
			t.Fatalf("another cluster or unrelated container was interrupted: %s %+v", id, checkpoint)
		}
	}
	output, err := control.manager.Exec(ctx, control.cluster.Context, "default", "unchanged-control", "app",
		[]string{"wget", "-qO-", "http://127.0.0.1:8080/marker"}, nil)
	if err != nil || string(output) != "unchanged-control-data" {
		t.Fatalf("control cluster workload changed: %q, error = %v", output, err)
	}
	output, err = runner.Run(ctx, runtimes.Command{Name: "container", Args: []string{"exec", control.unrelated, "/bin/cat", "/tmp/marker"}})
	if err != nil || string(output) != "unchanged-unrelated-data" {
		t.Fatalf("unrelated native container data changed: %q, error = %v", output, err)
	}
}
