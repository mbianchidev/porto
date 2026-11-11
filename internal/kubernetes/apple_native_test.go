package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type nativeLifecycleFixture struct {
	p                  *ClusterProvisioner
	request            ClusterRequest
	runner             *fakeRunner
	states             map[string]string
	shutdowns          []string
	failure            string
	onStop             func(string)
	snapshots          int
	deleteFailure      string
	keepDeletedStorage string
	deleted            []string
}

func newNativeLifecycleFixture(t *testing.T) *nativeLifecycleFixture {
	t.Helper()
	f := &nativeLifecycleFixture{runner: newFakeRunner(), states: make(map[string]string)}
	f.p = newAppleProvisioner(t, f.runner)
	f.request = ClusterRequest{Name: "owned", Provider: "apple", Apple: &AppleClusterState{
		Format: 1, ID: "cluster-token", AppRoot: t.TempDir(),
	}}
	type fileIdentity struct {
		info os.FileInfo
		id   string
	}
	files := make(map[string]fileIdentity)
	f.p.appleFileIdentity = func(path string) (string, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if existing, ok := files[path]; ok {
			if os.SameFile(existing.info, info) {
				return existing.id, nil
			}
			return "replaced-" + path, nil
		}
		id := "original-" + path
		files[path] = fileIdentity{info: info, id: id}
		return id, nil
	}
	for index, id := range []string{"owned-control", "owned-worker-1", "owned-worker-2"} {
		role := "worker"
		if index == 0 {
			role = "control-plane"
		}
		node := AppleNode{ID: id, Identity: id + "-token", Role: role, CreatedAt: "2026-11-11T12:00:00Z"}
		configuration, err := json.Marshal(map[string]any{
			"id": id, "creationDate": node.CreatedAt,
			"labels": map[string]string{appleClusterLabel: f.request.Apple.ID, appleNodeLabel: node.Identity},
		})
		if err != nil {
			t.Fatal(err)
		}
		node.Configuration = configuration
		directory := filepath.Join(f.request.Apple.AppRoot, "containers", id)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		descriptor, err := json.Marshal(map[string]any{
			"source": filepath.Join(directory, "rootfs.ext4"), "destination": "/",
			"type": map[string]any{"block": map[string]string{"format": "ext4"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{"rootfs.json": descriptor, "rootfs.ext4": []byte("synthetic persistent node data")} {
			if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		node.Storage, err = f.p.captureAppleStorage(f.request.Apple.AppRoot, id)
		if err != nil {
			t.Fatal(err)
		}
		f.request.Apple.Nodes = append(f.request.Apple.Nodes, node)
		f.states[id] = "running"
	}
	client := fake.NewClientset()
	f.p.appleClient = func(ClusterRequest) (kubeclient.Interface, error) {
		f.snapshots++
		return client, nil
	}
	f.runner.handler = func(command runtimes.Command) ([]byte, error) {
		if command.Name != "container" || len(command.Args) == 0 {
			return nil, errors.New("unexpected runtime")
		}
		if command.Args[0] == "inspect" {
			var result []map[string]any
			for _, id := range command.Args[1:] {
				if _, exists := f.states[id]; !exists {
					return nil, errors.New("native node is absent")
				}
				found := false
				for _, node := range f.request.Apple.Nodes {
					if node.ID == id {
						result = append(result, map[string]any{"id": id, "configuration": node.Configuration,
							"status": map[string]string{"state": f.states[id]}})
						found = true
					}
				}
				if !found {
					return nil, fmt.Errorf("unowned resource %s", id)
				}
			}
			return json.Marshal(result)
		}
		if command.Args[0] == "delete" && len(command.Args) == 2 {
			id := command.Args[1]
			if id == f.deleteFailure {
				return nil, errors.New("synthetic native deletion failure")
			}
			if f.states[id] != "stopped" {
				return nil, errors.New("attempted to delete a running or unowned VM")
			}
			f.deleted = append(f.deleted, id)
			delete(f.states, id)
			if id != f.keepDeletedStorage {
				if err := os.RemoveAll(filepath.Join(f.request.Apple.AppRoot, "containers", id)); err != nil {
					return nil, err
				}
			}
			return nil, nil
		}
		if command.Args[0] == "list" {
			result := make([]map[string]any, 0, len(f.states))
			for id := range f.states {
				result = append(result, map[string]any{"id": id})
			}
			return json.Marshal(result)
		}
		if command.Args[0] != "exec" || len(command.Args) < 3 {
			return nil, fmt.Errorf("forbidden native lifecycle primitive: %v", command.Args)
		}
		id := command.Args[1]
		if _, ok := f.states[id]; !ok {
			return nil, errors.New("operation targeted an unrelated resource")
		}
		args := strings.Join(command.Args[2:], " ")
		if strings.Contains(args, "/var/lib/kubelet/config.yaml") {
			return []byte("shutdownGracePeriod: 300s\nshutdownGracePeriodCriticalPods: 60s\n"), nil
		}
		if strings.Contains(args, "--property=SystemState") {
			return []byte("running\n"), nil
		}
		if strings.Contains(args, "systemd-inhibit") && !strings.Contains(args, "systemctl poweroff") {
			return nil, nil
		}
		if !strings.Contains(args, "systemctl poweroff --no-block") {
			return nil, fmt.Errorf("unexpected guest command %s", args)
		}
		if id == f.failure {
			return nil, errors.New("synthetic graceful shutdown failure")
		}
		f.shutdowns = append(f.shutdowns, id)
		f.states[id] = "stopped"
		if f.onStop != nil {
			f.onStop(id)
		}
		return nil, nil
	}
	if err := f.p.writeClusterMetadata(f.request); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestNativeAppleStopOrdersWorkersBeforeControlPlaneAndIsIdempotent(t *testing.T) {
	f := newNativeLifecycleFixture(t)
	if err := f.p.SetRunning(context.Background(), f.request.Name, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.shutdowns, []string{"owned-worker-2", "owned-worker-1", "owned-control"}) {
		t.Fatalf("unsafe node shutdown order: %v", f.shutdowns)
	}
	if err := f.p.SetRunning(context.Background(), f.request.Name, false); err != nil {
		t.Fatal(err)
	}
	if len(f.shutdowns) != 3 {
		t.Fatalf("already-stopped VMs were stopped again: %v", f.shutdowns)
	}
	current, err := f.p.readClusterMetadata(f.request.Name)
	if err != nil || current.Phase != "" || current.Apple.Operation.CompletedAt == "" {
		t.Fatalf("verified stop was not persisted: %+v, error = %v", current, err)
	}
	for _, node := range current.Apple.Nodes {
		data, err := os.ReadFile(node.Storage.Rootfs)
		if err != nil || string(data) != "synthetic persistent node data" {
			t.Fatalf("node backing data changed: %q, error = %v", data, err)
		}
	}
}

func TestNativeApplePartialStopRetainsControlPlaneAndRetriesFromPersistedProgress(t *testing.T) {
	f := newNativeLifecycleFixture(t)
	f.failure = "owned-worker-1"
	err := f.p.SetRunning(context.Background(), f.request.Name, false)
	if err == nil || f.states["owned-control"] != "running" || f.states["owned-worker-2"] != "stopped" {
		t.Fatalf("partial stop lost quorum or failure: %v, states = %v", err, f.states)
	}
	current, err := f.p.readClusterMetadata(f.request.Name)
	if err != nil || current.Phase != "stop-failed" || current.Apple.Operation.Nodes["owned-worker-2"] != "stopped" {
		t.Fatalf("partial stop progress was not retained: %+v, error = %v", current, err)
	}
	failureSnapshots := f.snapshots
	reopened := NewClusterProvisioner(nil, f.runner, f.p.kubeconfigRoot)
	reopened.goos, reopened.goarch = "darwin", "arm64"
	reopened.appleLookPath, reopened.appleFileIdentity, reopened.appleClient = f.p.appleLookPath, f.p.appleFileIdentity, f.p.appleClient
	f.failure = ""
	if err := reopened.SetRunning(context.Background(), f.request.Name, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.shutdowns, []string{"owned-worker-2", "owned-worker-1", "owned-control"}) || f.snapshots != failureSnapshots {
		t.Fatalf("retry repeated completed steps or replaced the pre-stop health baseline: %v, snapshots = %d", f.shutdowns, f.snapshots)
	}
}

func TestNativeAppleCancellationDoesNotForceOrDeleteAnyVM(t *testing.T) {
	f := newNativeLifecycleFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.onStop = func(string) { cancel() }
	err := f.p.SetRunning(ctx, f.request.Name, false)
	if !errors.Is(err, context.Canceled) || f.states["owned-control"] != "running" {
		t.Fatalf("canceled shutdown did not retain the control plane: %v, states = %v", err, f.states)
	}
	current, err := f.p.readClusterMetadata(f.request.Name)
	if err != nil || current.Phase != "stop-failed" {
		t.Fatalf("cancellation lost recoverable progress: %+v, error = %v", current, err)
	}
}

func TestNativeAppleStartAndStopResumeAnAlreadyAcknowledgedShutdown(t *testing.T) {
	for _, start := range []bool{false, true} {
		t.Run(fmt.Sprintf("start-%t", start), func(t *testing.T) {
			f := newNativeLifecycleFixture(t)
			f.request.Phase = "stop-failed"
			f.request.Apple.Operation = &AppleLifecycle{
				ID: "interrupted-shutdown", Goal: "stopped",
				Nodes: map[string]string{"owned-control": "shutdown-requested", "owned-worker-1": "stopped", "owned-worker-2": "stopped"},
			}
			f.states["owned-worker-1"], f.states["owned-worker-2"] = "stopped", "stopped"
			if err := f.p.writeClusterMetadata(f.request); err != nil {
				t.Fatal(err)
			}
			run := f.runner.handler
			shutdownObserved, booted := false, false
			f.runner.handler = func(command runtimes.Command) ([]byte, error) {
				if shutdownObserved && len(command.Args) > 0 && command.Args[0] == "inspect" {
					f.states["owned-control"] = "stopped"
				}
				if slices.Equal(command.Args, []string{"system", "status", "--format", "json"}) {
					return json.Marshal(map[string]any{
						"status": "running", "client": map[string]string{"version": "1.7.1"},
						"server": map[string]string{"version": "1.7.1"}, "paths": map[string]string{"appRoot": f.request.Apple.AppRoot},
					})
				}
				if slices.Equal(command.Args, []string{"start", "owned-control"}) {
					booted = true
					return nil, errors.New("synthetic stop after resumed native boot")
				}
				if len(command.Args) > 2 && command.Args[0] == "exec" && command.Args[1] == "owned-control" {
					if slices.Contains(command.Args, "--property=SystemState") {
						shutdownObserved = true
					}
					return nil, errors.New("guest is already shutting down")
				}
				return run(command)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := f.p.SetRunning(ctx, f.request.Name, start)
			if start {
				if !booted || err == nil || !strings.Contains(err.Error(), "resumed native boot") {
					t.Fatalf("Start did not wait for the pending shutdown before booting the same VM: %v, booted = %t", err, booted)
				}
			} else if err != nil {
				t.Fatalf("Stop could not resume a shutdown after guest services exited: %v", err)
			}
			if len(f.shutdowns) != 0 || f.states["owned-control"] != "stopped" {
				t.Fatalf("recovery repeated a shutdown or lost its outcome: %v, states = %v", f.shutdowns, f.states)
			}
		})
	}
}

func TestNativeAppleOperationsSerializeAcrossApplicationInstances(t *testing.T) {
	f := newNativeLifecycleFixture(t)
	request := f.request
	release, err := f.p.lockAppleOperation(context.Background(), &request, "stop")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	reopened := NewClusterProvisioner(nil, f.runner, f.p.kubeconfigRoot)
	reopened.goos, reopened.goarch = "darwin", "arm64"
	reopened.appleLookPath, reopened.appleFileIdentity, reopened.appleClient = f.p.appleLookPath, f.p.appleFileIdentity, f.p.appleClient
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	err = reopened.SetRunning(ctx, f.request.Name, false)
	if !errors.Is(err, context.DeadlineExceeded) || len(f.shutdowns) != 0 {
		t.Fatalf("conflicting process changed native resources: %v, shutdowns = %v", err, f.shutdowns)
	}
}

func TestNativeAppleInventoryDistinguishesActiveAndInterruptedOperationsAcrossInstances(t *testing.T) {
	f := newNativeLifecycleFixture(t)
	f.request.Phase = "stopping"
	f.request.Apple.Operation = &AppleLifecycle{ID: "pending-stop", Goal: "stopped", Nodes: make(map[string]string)}
	if err := f.p.writeClusterMetadata(f.request); err != nil {
		t.Fatal(err)
	}
	release, err := f.p.lockAppleOperation(context.Background(), &f.request, "stop")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	reopened := NewClusterProvisioner(nil, f.runner, f.p.kubeconfigRoot)
	reopened.goos, reopened.goarch = "darwin", "arm64"
	reopened.appleLookPath, reopened.appleFileIdentity = f.p.appleLookPath, f.p.appleFileIdentity
	cluster := Cluster{State: f.request.Phase}
	reopened.observeAppleNative(context.Background(), f.request, &cluster)
	if cluster.State != "stopping" {
		t.Fatalf("another active application's shutdown was reported as interrupted: %+v", cluster)
	}
	release()
	reopened.observeAppleNative(context.Background(), f.request, &cluster)
	if cluster.State != "degraded" || !strings.Contains(cluster.Message, "interrupted") {
		t.Fatalf("released operation ownership did not expose recoverable interruption: %+v", cluster)
	}
}

func TestNativeAppleDeleteRetriesOnlyUndeletedOwnedResources(t *testing.T) {
	f := newNativeLifecycleFixture(t)
	f.deleteFailure = "owned-worker-1"
	err := f.p.Delete(context.Background(), f.request.Name)
	if err == nil || !slices.Equal(f.deleted, []string{"owned-worker-2"}) {
		t.Fatalf("partial deletion did not expose its outcome: %v, deleted = %v", err, f.deleted)
	}
	current, err := f.p.readClusterMetadata(f.request.Name)
	if err != nil || !current.Apple.Nodes[2].Deleted || current.Phase != "deleting" {
		t.Fatalf("deletion progress was not persisted: %+v, error = %v", current, err)
	}
	f.deleteFailure = ""
	if err := f.p.Delete(context.Background(), f.request.Name); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.deleted, []string{"owned-worker-2", "owned-worker-1", "owned-control"}) {
		t.Fatalf("retry deleted a resource twice or lost ownership: %v", f.deleted)
	}
	if _, err := f.p.readClusterMetadata(f.request.Name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed deletion left an inconsistent ownership record: %v", err)
	}
}

func TestNativeAppleDeleteAfterLifecycleFailureStillRequiresGracefulShutdown(t *testing.T) {
	for _, phase := range []string{"start-failed", "stop-failed"} {
		t.Run(phase, func(t *testing.T) {
			f := newNativeLifecycleFixture(t)
			f.request.Phase = phase
			if err := f.p.writeClusterMetadata(f.request); err != nil {
				t.Fatal(err)
			}
			run := f.runner.handler
			f.runner.handler = func(command runtimes.Command) ([]byte, error) {
				if len(command.Args) > 2 && command.Args[0] == "exec" && command.Args[1] == "owned-worker-1" &&
					slices.Contains(command.Args, "/var/lib/kubelet/config.yaml") {
					return nil, errors.New("synthetic unavailable shutdown budget")
				}
				return run(command)
			}
			err := f.p.Delete(context.Background(), f.request.Name)
			if err == nil || !strings.Contains(err.Error(), "shutdown budget") || len(f.deleted) != 0 ||
				f.states["owned-control"] != "running" || f.states["owned-worker-1"] != "running" {
				t.Fatalf("deletion bypassed workload-safe shutdown after %s: %v, deleted = %v, states = %v",
					phase, err, f.deleted, f.states)
			}
		})
	}
}

func TestNativeAppleDeleteRecoversLostAcknowledgementWithoutDiscardingResidualStorage(t *testing.T) {
	for _, residual := range []bool{false, true} {
		t.Run(fmt.Sprintf("residual-storage-%t", residual), func(t *testing.T) {
			f := newNativeLifecycleFixture(t)
			node := f.request.Apple.Nodes[2]
			if residual {
				f.keepDeletedStorage = node.ID
			}
			run := f.runner.handler
			f.runner.handler = func(command runtimes.Command) ([]byte, error) {
				output, err := run(command)
				if err == nil && slices.Equal(command.Args, []string{"delete", node.ID}) {
					return nil, errors.New("synthetic lost native deletion acknowledgement")
				}
				return output, err
			}
			if err := f.p.Delete(context.Background(), f.request.Name); err == nil {
				t.Fatal("uncertain native deletion was reported as complete")
			}
			f.runner.handler = run
			err := f.p.Delete(context.Background(), f.request.Name)
			if residual {
				data, readErr := os.ReadFile(node.Storage.Rootfs)
				if err == nil || !strings.Contains(err.Error(), "backing") || readErr != nil ||
					string(data) != "synthetic persistent node data" || !slices.Equal(f.deleted, []string{node.ID}) {
					t.Fatalf("ambiguous deletion discarded backing data: %v, data = %q, read = %v, deleted = %v",
						err, data, readErr, f.deleted)
				}
				return
			}
			if err != nil || !slices.Equal(f.deleted, []string{node.ID, "owned-worker-1", "owned-control"}) {
				t.Fatalf("retry could not reconcile the exact completed deletion: %v, deleted = %v", err, f.deleted)
			}
			if _, err := f.p.readClusterMetadata(f.request.Name); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovered deletion left its cluster record: %v", err)
			}
		})
	}
}

func TestNativeAppleRecoveryWaitsForHealthyStatefulWorkloadsButNotCompletedOrPausedWork(t *testing.T) {
	p := newAppleProvisioner(t, newFakeRunner())
	one := int32(1)
	set := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "fixture", UID: types.UID("stateful-uid"), Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: &one},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 1},
	}
	paused := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "paused", Namespace: "fixture", UID: types.UID("paused-uid"), Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: &one, Paused: true},
	}
	complete := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "completed", Namespace: "fixture", UID: types.UID("job-uid")},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}
	client := fake.NewClientset(set, paused, complete)
	p.appleClient = func(ClusterRequest) (kubeclient.Interface, error) { return client, nil }
	request := ClusterRequest{Name: "fixture", Provider: "apple", Apple: &AppleClusterState{Format: 1, ID: "fixture"}}
	snapshot, err := p.captureAppleHealth(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	set.Status.ReadyReplicas = 0
	if _, err := client.AppsV1().StatefulSets("fixture").UpdateStatus(context.Background(), set, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.checkAppleWorkloads(context.Background(), request, snapshot); err == nil || !strings.Contains(err.Error(), "database") {
		t.Fatalf("recovery ignored a previously healthy stateful workload: %v", err)
	}
	set.Status.ReadyReplicas = 1
	if _, err := client.AppsV1().StatefulSets("fixture").UpdateStatus(context.Background(), set, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.checkAppleWorkloads(context.Background(), request, snapshot); err != nil {
		t.Fatalf("completed or paused work incorrectly blocked recovery: %v", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" || action.GetVerb() == "delete" || action.GetVerb() == "create" {
			t.Fatalf("readiness changed application desired state: %+v", action)
		}
	}
}

func TestNativeAppleAddressRecoveryPreservesTLSAuthorityAndRejectsCredentialCommands(t *testing.T) {
	configuration := clientcmdapi.NewConfig()
	configuration.Clusters["native"] = &clientcmdapi.Cluster{Server: "https://192.0.2.10:6443", CertificateAuthorityData: []byte("synthetic-authority")}
	configuration.AuthInfos["native"] = &clientcmdapi.AuthInfo{ClientCertificateData: []byte("synthetic-client"), ClientKeyData: []byte("synthetic-key")}
	configuration.Contexts["native"] = &clientcmdapi.Context{Cluster: "native", AuthInfo: "native"}
	configuration.CurrentContext = "native"
	data, err := clientcmd.Write(*configuration)
	if err != nil {
		t.Fatal(err)
	}
	normalized, authority, err := appleClientConfiguration(data, "https://192.0.2.99:6443", "stable-native-node", true)
	if err != nil || string(authority) != "synthetic-authority" {
		t.Fatalf("address repair lost the TLS authority: %v", err)
	}
	recovered, err := clientcmd.Load(normalized)
	if err != nil {
		t.Fatal(err)
	}
	cluster := recovered.Clusters["native"]
	if cluster.Server != "https://192.0.2.99:6443" || cluster.TLSServerName != "stable-native-node" ||
		cluster.InsecureSkipTLSVerify || string(recovered.AuthInfos["native"].ClientKeyData) != "synthetic-key" {
		t.Fatalf("address repair weakened identity: %+v", recovered)
	}
	configuration.AuthInfos["native"].Exec = &clientcmdapi.ExecConfig{Command: "synthetic-untrusted-command"}
	data, err = clientcmd.Write(*configuration)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := appleClientConfiguration(data, "https://127.0.0.1:6443", "stable-native-node", true); err == nil {
		t.Fatal("native host credentials accepted an executable credential provider")
	}
}

func TestNativeAppleStopRejectsReplacedNodeBeforeChangingAnyResource(t *testing.T) {
	runner := newFakeRunner()
	var mutations []runtimes.Command
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if len(command.Args) > 0 && command.Args[0] == "inspect" {
			return []byte(`[{"id":"owned-control","configuration":{"id":"owned-control","creationDate":"2026-11-11T12:00:00Z","labels":{"dev.porto.cluster":"someone-else","dev.porto.node":"node-token"}},"status":{"state":"running"}}]`), nil
		}
		mutations = append(mutations, command)
		return nil, errors.New("unexpected native mutation")
	}
	p := newAppleProvisioner(t, runner)
	spec := ClusterRequest{
		Name: "owned", Provider: "apple",
		Apple: &AppleClusterState{
			Format: 1, ID: "cluster-token",
			Nodes: []AppleNode{{
				ID: "owned-control", Identity: "node-token", Role: "control-plane",
				CreatedAt:     "2026-11-11T12:00:00Z",
				Configuration: json.RawMessage(`{"id":"owned-control","creationDate":"2026-11-11T12:00:00Z","labels":{"dev.porto.cluster":"cluster-token","dev.porto.node":"node-token"}}`),
			}},
		},
	}
	if err := p.writeClusterMetadata(spec); err != nil {
		t.Fatal(err)
	}
	err := p.SetRunning(context.Background(), spec.Name, false)
	if err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("replaced native node was not rejected by ownership: %v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("ownership failure changed runtime resources: %+v", mutations)
	}
}
