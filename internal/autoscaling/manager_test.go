package autoscaling

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/kubernetes"
	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/karpenter/pkg/operator/options"
)

func testManager(t *testing.T, provider string) (*Manager, *mockProvisioner) {
	t.Helper()
	p := &mockProvisioner{request: kubernetes.ClusterRequest{Name: "dev", Provider: provider}}
	m, err := New(p, scratch(t), func(ctx context.Context, _, _ string) (func(), error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return func() {}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	m.connect = func(name string) (clusterConnection, error) {
		if name != "dev" {
			return clusterConnection{}, errors.New("wrong cluster context")
		}
		return clusterConnection{client: c}, nil
	}
	return m, p
}

func TestInvalidConfigAndKindCapabilities(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []NodeConfig{
		{Enabled: true},
		{Enabled: true, MaxNodes: 33, Machine: testConfig().Machine},
		{Enabled: true, MaxNodes: 2, Machine: kubernetes.MachineSpec{CPUs: 0, MemoryMiB: 2048, DiskGiB: 20}},
		{Enabled: true, MaxNodes: 32, Machine: kubernetes.MachineSpec{CPUs: 32, MemoryMiB: 131072, DiskGiB: 1024}},
	} {
		m, _ := testManager(t, "k3s")
		m.connect = func(string) (clusterConnection, error) {
			t.Fatal("invalid config reached cluster")
			return clusterConnection{}, nil
		}
		if _, err := m.Configure(ctx, "dev", Config{Nodes: cfg}); err == nil {
			t.Fatal("invalid machine/host budget accepted")
		}
	}
	m, _ := testManager(t, "kind")
	if _, err := m.Configure(ctx, "dev", Config{Nodes: testConfig()}); err == nil || !strings.Contains(err.Error(), "kind topology is immutable") {
		t.Fatalf("kind must reject real node scaling with actionable error: %v", err)
	}
	if err := validateConfig(kubernetes.ClusterRequest{Provider: "kind"}, Config{KEDA: true}); err != nil {
		t.Fatalf("KEDA must support kind: %v", err)
	}
	if _, err := m.Status(ctx, "../escape"); err == nil {
		t.Fatal("path escape cluster accepted")
	}
}

func TestRunRestoresPausesJoinsAndRestartsControllers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, _ := testManager(t, "k3s")
	if err := writeJSON(filepath.Join(m.directory("dev"), "settings.json"), Config{Nodes: testConfig()}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 4)
	stopped := make(chan struct{}, 4)
	var active atomic.Int32
	m.run = func(ctx context.Context, _ clusterConnection, _ *localProvider) error {
		active.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		// Pause must join even when a controller takes time to finish.
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		stopped <- struct{}{}
		return nil
	}
	finished := make(chan error, 1)
	go func() { finished <- m.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("persisted controller did not restart")
	}
	if err := m.Pause(ctx, "dev"); err != nil || active.Load() != 0 {
		t.Fatalf("Pause did not join in-flight work: %v", err)
	}
	<-stopped
	status, err := m.Status(ctx, "dev")
	if err != nil || !status.Paused || status.Running {
		t.Fatalf("unexpected paused status: %+v, %v", status, err)
	}
	if err := m.Resume(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("Resume did not start controller")
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil || active.Load() != 0 {
			t.Fatalf("Run did not join controllers: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon shutdown did not join controller")
	}
}

func TestInstallationFailureDoesNotEnableSettings(t *testing.T) {
	m, _ := testManager(t, "kind")
	m.download = func(context.Context, string) ([]byte, error) { return nil, errors.New("release download failed") }
	status, err := m.Configure(context.Background(), "dev", Config{KEDA: true})
	if err == nil || status.KEDA || status.KEDAReady || !status.Paused || !strings.Contains(status.Message, "release download failed") {
		t.Fatalf("installation failure was hidden or incorrectly enabled: %+v, %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(m.directory("dev"), "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed installation persisted enabled settings")
	}
}

func TestSettingsUseChildDirectoryAndRenameForget(t *testing.T) {
	ctx := context.Background()
	m, p := testManager(t, "k3s")
	cfg := Config{Nodes: testConfig(), KEDA: true}
	if err := writeJSON(filepath.Join(m.directory("dev"), "settings.json"), cfg); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(m.root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "autoscaling" || !entries[0].IsDir() {
		t.Fatalf("autoscaling polluted cluster metadata root: %v, %v", entries, err)
	}
	if err := m.Rename(ctx, "dev", "renamed"); err != nil {
		t.Fatal(err)
	}
	p.request.Name = "renamed"
	actual, err := m.settings("renamed")
	if err != nil || actual != cfg {
		t.Fatalf("rename did not retain settings: %+v, %v", actual, err)
	}
	if err := m.Forget(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.directory("renamed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Forget did not remove host-side state")
	}
}

func TestPrivateKubeconfigContextIgnoresAmbientConfig(t *testing.T) {
	m, _ := testManager(t, "k3s")
	data := `apiVersion: v1
kind: Config
current-context: foreign
clusters:
- name: local
  cluster:
    server: https://127.0.0.1:26443
- name: foreign
  cluster:
    server: https://127.0.0.1:16443
contexts:
- name: porto-k3s-dev
  context:
    cluster: local
    user: local
- name: foreign
  context:
    cluster: foreign
    user: foreign
users:
- name: local
  user:
    token: synthetic-local
- name: foreign
  user:
    token: synthetic-foreign
`
	if err := os.WriteFile(filepath.Join(m.root, config.KubernetesClusterFileToken("dev")+".yaml"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	// The ambient file is deliberately nonexistent. ExplicitPath must never
	// consult it, even if current-context in the private file is foreign.
	t.Setenv("KUBECONFIG", filepath.Join(m.root, "missing-ambient-config"))
	connection, err := m.connection("dev")
	if err != nil {
		t.Fatal(err)
	}
	if connection.config.Host != "https://127.0.0.1:26443" || connection.config.BearerToken != "synthetic-local" {
		t.Fatalf("connection escaped managed context: %+v", connection.config)
	}
}

func installationClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	s, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).WithStatusSubresource(&PortoNodeClass{}).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if patch.Type() != types.ApplyPatchType {
				return c.Patch(ctx, object, patch, opts...)
			}
			// The fake client has no SSA implementation. Simulate only its
			// create/replace behavior; ownership checks are production code.
			old := object.DeepCopyObject().(client.Object)
			err := c.Get(ctx, client.ObjectKeyFromObject(object), old)
			if client.IgnoreNotFound(err) != nil {
				return err
			}
			if err != nil {
				return c.Create(ctx, object)
			}
			object.SetResourceVersion(old.GetResourceVersion())
			return c.Update(ctx, object)
		},
	}).Build()
}

func TestManifestRefusesForeignOwnershipBeforeAnyMutation(t *testing.T) {
	ctx := context.Background()
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "keda-operator", Namespace: "keda"}}
	c := installationClient(t, foreign)
	data := []byte(`apiVersion: apps/v1
kind: Deployment
metadata: {name: first, namespace: keda}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: keda-operator, namespace: keda}
`)
	if err := installManifest(ctx, c, data); err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("foreign installation was adopted: %v", err)
	}
	var first appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Name: "first", Namespace: "keda"}, &first); err == nil {
		t.Fatal("partial installation occurred before foreign ownership preflight")
	}
}

func TestManifestApplyIsIdempotentAndReportsErrors(t *testing.T) {
	ctx := context.Background()
	c := installationClient(t)
	data := []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: keda-operator, namespace: keda}\n")
	for range 2 {
		if err := installManifest(ctx, c, data); err != nil {
			t.Fatal(err)
		}
	}
	var deployment appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: "keda", Name: "keda-operator"}, &deployment); err != nil || deployment.Labels[ownerLabel] != "porto" {
		t.Fatalf("installation ownership missing: %+v, %v", deployment.Labels, err)
	}
	for _, invalid := range []string{"", "not: [valid: yaml", "metadata: {name: no-kind}"} {
		if err := installManifest(ctx, c, []byte(invalid)); err == nil {
			t.Fatalf("invalid installation manifest accepted: %q", invalid)
		}
	}
	broken := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("admission denied")
		},
	})
	if err := installManifest(ctx, broken, data); err == nil || !strings.Contains(err.Error(), "admission denied") {
		t.Fatalf("API installation error hidden: %v", err)
	}
}

func TestKEDADisablePreservesScaledObjectsAndForeignInstallations(t *testing.T) {
	ctx := context.Background()
	scaled := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1", "kind": "ScaledObject",
		"metadata": map[string]any{"name": "queue", "namespace": "default"},
	}}
	c := installationClient(t, scaled)
	if err := disableKEDA(ctx, c); err == nil || !strings.Contains(err.Error(), "ScaledObjects") {
		t.Fatalf("KEDA disabled despite existing workload scaler: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(scaled), scaled); err != nil {
		t.Fatal("disable destroyed workload scaler")
	}
}

func TestKEDAReadinessReportsFailedRollout(t *testing.T) {
	ctx := context.Background()
	var objects []client.Object
	for _, name := range []string{"scaledobjects.keda.sh", "scaledjobs.keda.sh", "triggerauthentications.keda.sh", "clustertriggerauthentications.keda.sh"} {
		objects = append(objects, &apiextensionsv1.CustomResourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{{
				Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue,
			}}},
		})
	}
	objects = append(objects, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "keda-operator", Namespace: "keda", Labels: map[string]string{ownerLabel: "porto"}},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Reason: "ProgressDeadlineExceeded", Message: "image pull failed",
		}}},
	})
	if err := kedaReady(ctx, installationClient(t, objects...)); err == nil || !strings.Contains(err.Error(), "image pull failed") {
		t.Fatalf("KEDA failed rollout was hidden: %v", err)
	}
}

func TestControllerOptionsAreScopedAndOptionalFeaturesDisabled(t *testing.T) {
	t.Setenv("FEATURE_GATES", "NodeRepair=true")
	ctx := controllerContext(context.Background())
	opts := options.FromContext(ctx)
	if !opts.IgnoreDRARequests || !opts.DisableClusterStateObservability || opts.FeatureGates.NodeRepair ||
		opts.FeatureGates.NodeOverlay || opts.FeatureGates.StaticCapacity || opts.FeatureGates.CapacityBuffer {
		t.Fatalf("unsupported controller features enabled: %+v", opts)
	}
	if os.Getenv("FEATURE_GATES") != "NodeRepair=true" {
		t.Fatal("host environment mutated")
	}
}
