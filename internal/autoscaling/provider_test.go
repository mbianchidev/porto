package autoscaling

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mbianchidev/porto/internal/kubernetes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

type mockProvisioner struct {
	mu      sync.Mutex
	request kubernetes.ClusterRequest
	scale   func(context.Context, string, kubernetes.NodeGroupSpec, string) error
	deleted int
	scaled  int
	foreign bool
}

func (p *mockProvisioner) Configuration(name string) (kubernetes.ClusterRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	request := p.request
	if name != request.Name {
		return request, errors.New("cluster does not exist")
	}
	return request, nil
}
func (p *mockProvisioner) ContextName(name string) (string, error) {
	return "porto-k3s-" + name, nil
}
func (p *mockProvisioner) AutoscalingNodeName(_ context.Context, name, group string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.foreign {
		return "", errors.New("foreign VM ownership")
	}
	nodeName := "porto-" + runtimeName(p.request) + "-" + group + "-1"
	if p.request.Provider == "k0s" {
		nodeName = "lima-" + nodeName
	}
	return nodeName, nil
}
func (p *mockProvisioner) ScaleNodeGroup(ctx context.Context, name string, group kubernetes.NodeGroupSpec, version string) error {
	p.mu.Lock()
	p.scaled++
	scale := p.scale
	p.mu.Unlock()
	if scale != nil {
		return scale(ctx, name, group, version)
	}
	return nil
}
func (p *mockProvisioner) DeleteAutoscalingNode(_ context.Context, _, _ string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted++
	return nil
}
func (p *mockProvisioner) AutoscalingNodeExists(ctx context.Context, name, group string) (bool, error) {
	_, err := p.AutoscalingNodeName(ctx, name, group)
	return err == nil, err
}
func (p *mockProvisioner) EnsureAutoscalingNode(ctx context.Context, name string, group kubernetes.NodeGroupSpec, version string) error {
	return p.ScaleNodeGroup(ctx, name, group, version)
}

func scratch(t *testing.T) string {
	t.Helper()
	root := ".autoscaling-test-state"
	path := filepath.Join(root, strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()))
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path); _ = os.Remove(root) })
	return path
}

func testConfig() NodeConfig {
	return NodeConfig{Enabled: true, MaxNodes: 2, Machine: kubernetes.MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}}
}

func testClaim(uid string) *v1.NodeClaim {
	return &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-" + uid, UID: types.UID(uid),
			Labels: map[string]string{v1.NodePoolLabelKey: managedName, ownerLabel: "porto"}},
		Spec: v1.NodeClaimSpec{
			NodeClassRef: &v1.NodeClassReference{Group: NodeClassGroupVersion.Group, Kind: "PortoNodeClass", Name: managedName},
		},
	}
}

func testProvider(t *testing.T) (*localProvider, *mockProvisioner, client.Client, *atomic.Int32) {
	t.Helper()
	s, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(&PortoNodeClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: NodeClassGroupVersion.String(), Kind: "PortoNodeClass"},
		ObjectMeta: metav1.ObjectMeta{Name: managedName, Labels: map[string]string{ownerLabel: "porto"}},
		Spec:       PortoNodeClassSpec{Machine: testConfig().Machine},
	}).Build()
	p := &mockProvisioner{request: kubernetes.ClusterRequest{Name: "dev", RuntimeName: "stable", Provider: "k3s", Version: "v1.35.0+k3s1"}}
	active := &atomic.Int32{}
	provider, err := newProvider(p, func(ctx context.Context, name, action string) (func(), error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if name != "dev" || !strings.HasPrefix(action, "autoscaling-") {
			return nil, errors.New("incorrect operation scope")
		}
		active.Add(1)
		return func() { active.Add(-1) }, nil
	}, "dev", scratch(t), testConfig(), c)
	if err != nil {
		t.Fatal(err)
	}
	p.scale = func(ctx context.Context, name string, group kubernetes.NodeGroupSpec, _ string) error {
		if active.Load() != 1 {
			return errors.New("mutation outside operation gate")
		}
		nodeName, _ := p.AutoscalingNodeName(ctx, name, group.Name)
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName, Labels: group.Labels},
			Spec: corev1.NodeSpec{ProviderID: "k3s://" + nodeName}}
		if err := c.Create(ctx, node); client.IgnoreAlreadyExists(err) != nil {
			return err
		}
		return nil
	}
	return provider, p, c, active
}

func TestProviderRealWorkerRetryRestartAndOwnership(t *testing.T) {
	ctx := context.Background()
	provider, p, c, active := testProvider(t)
	claim := testClaim("uid-one")
	first, err := provider.Create(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status.ProviderID != "porto:///stable/uid-one" || p.scaled != 1 || active.Load() != 0 {
		t.Fatalf("unexpected worker: %#v, calls=%d, gate=%d", first.Status, p.scaled, active.Load())
	}
	var actual corev1.Node
	record := provider.records[string(claim.UID)]
	if err := c.Get(ctx, types.NamespacedName{Name: record.NodeName}, &actual); err != nil {
		t.Fatal(err)
	}
	if err := verifyNode(&actual, record, true); err != nil {
		t.Fatal(err)
	}
	restarted, err := newProvider(p, provider.operation, "dev", filepath.Dir(provider.statePath), testConfig(), c)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := restarted.Create(ctx, claim)
	if err != nil || retry.Status.ProviderID != first.Status.ProviderID || len(restarted.records) != 1 {
		t.Fatalf("retry must reuse durable worker identity: %v, %#v", err, retry)
	}
	if _, err := restarted.Get(ctx, "porto:///foreign/uid-one"); !cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("foreign provider ID was accepted: %v", err)
	}
	p.mu.Lock()
	p.request.RuntimeName = "replacement"
	p.mu.Unlock()
	if _, err := restarted.Create(ctx, claim); err == nil {
		t.Fatal("retry adopted worker after runtime changed")
	}
}

func TestProviderConcurrentHardLimits(t *testing.T) {
	provider, p, _, active := testProvider(t)
	var succeeded atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			uid := fmt.Sprintf("uid-%d", i)
			if _, err := provider.Create(context.Background(), testClaim(uid)); err == nil {
				succeeded.Add(1)
			} else if !cloudprovider.IsInsufficientCapacityError(err) {
				t.Errorf("unexpected create error: %v", err)
			}
		})
	}
	wg.Wait()
	if succeeded.Load() != 2 || len(provider.records) != 2 || p.scaled != 2 || active.Load() != 0 {
		t.Fatalf("hard node limit failed: succeeded=%d records=%d calls=%d", succeeded.Load(), len(provider.records), p.scaled)
	}
}

func TestFailedCreationRetainsReservationAndRetries(t *testing.T) {
	provider, p, _, _ := testProvider(t)
	scale := p.scale
	p.scale = func(context.Context, string, kubernetes.NodeGroupSpec, string) error {
		return errors.New("VM boot failed")
	}
	claim := testClaim("retry")
	if _, err := provider.Create(context.Background(), claim); err == nil || len(provider.records) != 1 {
		t.Fatal("failed create must reserve durable capacity")
	}
	p.scale = scale
	if _, err := provider.Create(context.Background(), claim); err != nil || len(provider.records) != 1 {
		t.Fatalf("retry failed: %v", err)
	}
}

func TestProviderRejectsForeignClaimAndVM(t *testing.T) {
	for _, mutation := range []func(*v1.NodeClaim){
		func(c *v1.NodeClaim) { c.UID = "" },
		func(c *v1.NodeClaim) { c.Spec.NodeClassRef.Group = "foreign.example" },
		func(c *v1.NodeClaim) { c.Labels[v1.NodePoolLabelKey] = "foreign" },
		func(c *v1.NodeClaim) { c.Labels[ownerLabel] = "foreign" },
	} {
		provider, p, _, _ := testProvider(t)
		claim := testClaim("foreign")
		mutation(claim)
		if _, err := provider.Create(context.Background(), claim); err == nil || p.scaled != 0 {
			t.Fatal("foreign claim must not provision machines")
		}
	}
	provider, p, _, _ := testProvider(t)
	p.foreign = true
	if _, err := provider.Create(context.Background(), testClaim("foreign")); err == nil || p.scaled != 0 {
		t.Fatal("foreign VM must not be adopted")
	}
}

func TestProviderRejectsForeignKubelet(t *testing.T) {
	provider, p, c, _ := testProvider(t)
	uid := "foreign-node"
	nodeName, _ := p.AutoscalingNodeName(context.Background(), "dev", claimGroup(uid))
	if err := c.Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Spec: corev1.NodeSpec{ProviderID: "another://provider"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Create(context.Background(), testClaim(uid)); err == nil {
		t.Fatal("foreign kubelet must not be adopted")
	}
}

func TestProviderDeletionProtectsStorageAndRequiresUpstreamDrain(t *testing.T) {
	ctx := context.Background()
	provider, p, c, _ := testProvider(t)
	claim := testClaim("delete")
	created, err := provider.Create(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	claim.Status = created.Status
	if err := provider.Delete(ctx, claim); err == nil {
		t.Fatal("non-terminating claim was deleted")
	}
	now := metav1.Now()
	claim.DeletionTimestamp = &now
	if err := provider.Delete(ctx, claim); err == nil {
		t.Fatal("undrained node was deleted")
	}
	record := provider.records[string(claim.UID)]
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: record.NodeName}, &node); err != nil {
		t.Fatal(err)
	}
	node.Finalizers = []string{v1.TerminationFinalizer}
	if err := c.Update(ctx, &node); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, &node); err != nil {
		t.Fatal(err)
	}
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "precious"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/data"}}}}
	if err := c.Create(ctx, pv); err != nil {
		t.Fatal(err)
	}
	if err := provider.Delete(ctx, claim); err == nil || p.deleted != 0 {
		t.Fatal("local PV data was not protected")
	}
	if err := c.Delete(ctx, pv); err != nil {
		t.Fatal(err)
	}
	if err := provider.Delete(ctx, claim); err != nil || p.deleted != 1 || len(provider.records) != 0 {
		t.Fatalf("safe drained worker deletion failed: %v", err)
	}
	if err := provider.Delete(ctx, claim); !cloudprovider.IsNodeClaimNotFoundError(err) {
		t.Fatalf("repeated deletion must report not found: %v", err)
	}
}

func TestStorageProtectionAndEmptyDir(t *testing.T) {
	ctx := context.Background()
	provider, _, c, _ := testProvider(t)
	claim := testClaim("storage")
	if _, err := provider.Create(ctx, claim); err != nil {
		t.Fatal(err)
	}
	record := provider.records[string(claim.UID)]
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "default"},
		Spec: corev1.PodSpec{NodeName: record.NodeName, Volumes: []corev1.Volume{{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := provider.protectStorage(ctx); err != nil {
		t.Fatal(err)
	}
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: record.NodeName}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Annotations[v1.DoNotDisruptAnnotationKey] != "true" {
		t.Fatal("emptyDir worker was not protected")
	}
	if err := c.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := provider.protectStorage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: record.NodeName}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Annotations[v1.DoNotDisruptAnnotationKey] != "false" {
		t.Fatal("empty worker did not become eligible for WhenEmpty consolidation")
	}
}

func TestCorruptLedgerFailsClosed(t *testing.T) {
	p := &mockProvisioner{}
	path := scratch(t)
	if err := writeJSON(filepath.Join(path, "claims.json"), map[string]claimRecord{"fake": {UID: "fake", Group: "foreign"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := newProvider(p, nil, "dev", path, testConfig(), nil); err == nil {
		t.Fatal("corrupt ownership ledger was accepted")
	}
}
