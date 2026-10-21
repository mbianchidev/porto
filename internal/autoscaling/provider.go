package autoscaling

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/mbianchidev/porto/internal/kubernetes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

type localProvider struct {
	mu          sync.Mutex
	provisioner Provisioner
	operation   Operation
	cluster     string
	config      NodeConfig
	statePath   string
	client      client.Client // Uncached: ownership and storage checks cannot lag.
	records     map[string]claimRecord
}

var _ cloudprovider.CloudProvider = (*localProvider)(nil)

func newProvider(p Provisioner, operation Operation, name, dir string, cfg NodeConfig, c client.Client) (*localProvider, error) {
	provider := &localProvider{
		provisioner: p, operation: operation, cluster: name, config: cfg,
		statePath: filepath.Join(dir, "claims.json"), client: c, records: map[string]claimRecord{},
	}
	if err := readJSON(provider.statePath, &provider.records); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if provider.records == nil {
		return nil, errors.New("invalid autoscaling claim ledger")
	}
	groups := map[string]bool{}
	for uid, record := range provider.records {
		baseNodeName := "porto-" + record.Runtime + "-" + record.Group + "-1"
		if uid == "" || strings.ContainsAny(uid, "/\x00\r\n") || record.UID != uid ||
			record.Group != claimGroup(uid) || (record.ProviderID != providerID(record.Runtime, uid) && record.ProviderID != "k3s://"+record.NodeName) ||
			validateName(record.Runtime) != nil || groups[record.Group] ||
			(record.NodeName != baseNodeName && record.NodeName != "lima-"+baseNodeName) ||
			!record.Machine.Enabled || validateConfig(kubernetes.ClusterRequest{Provider: "k3s"}, Config{Nodes: record.Machine}) != nil {
			return nil, errors.New("invalid autoscaling claim ownership ledger; refusing to adopt VMs")
		}
		groups[record.Group] = true
	}
	return provider, nil
}

func claimGroup(uid string) string {
	hash := sha256.Sum256([]byte(uid))
	// Lima names have a 63-byte limit, including a 40-byte runtime name.
	// Collisions are checked against the full UID before provisioning.
	return fmt.Sprintf("as-%x", hash[:5])
}

func providerID(runtimeName, uid string) string { return "porto:///" + runtimeName + "/" + uid }

func runtimeName(request kubernetes.ClusterRequest) string {
	if request.RuntimeName != "" {
		return request.RuntimeName
	}
	return request.Name
}

func (p *localProvider) Name() string                                 { return "porto" }
func (p *localProvider) RepairPolicies() []cloudprovider.RepairPolicy { return nil }
func (p *localProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&PortoNodeClass{}}
}
func (p *localProvider) IsDrifted(context.Context, *v1.NodeClaim) (cloudprovider.DriftReason, error) {
	// Machine changes apply only to newly created workers. Existing VMs are never
	// force-replaced, which could destroy local storage.
	return "", nil
}

func (p *localProvider) Create(ctx context.Context, claim *v1.NodeClaim) (*v1.NodeClaim, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.validateClaim(ctx, claim); err != nil {
		return nil, err
	}
	release, err := p.operation(ctx, p.cluster, "autoscaling-create")
	if err != nil {
		return nil, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request, err := p.provisioner.Configuration(p.cluster)
	if err != nil {
		return nil, err
	}
	if err := validateConfig(request, Config{Nodes: p.config}); err != nil {
		return nil, err
	}
	uid := string(claim.UID)
	record, exists := p.records[uid]
	if exists && (record.Runtime != runtimeName(request) || record.Name != claim.Name) {
		return nil, errors.New("NodeClaim runtime ownership changed; refusing to adopt an old worker")
	}
	if !exists {
		if err := p.checkBudget(request); err != nil {
			return nil, cloudprovider.NewInsufficientCapacityError(err)
		}
		for _, existing := range p.records {
			if existing.Group == claimGroup(uid) {
				return nil, errors.New("NodeClaim identity hash collision; refusing to share a VM")
			}
		}
		record = claimRecord{
			UID: uid, Name: claim.Name, Group: claimGroup(uid), Runtime: runtimeName(request),
			ProviderID: providerID(runtimeName(request), uid), Machine: p.config,
		}
		record.NodeName, err = p.provisioner.AutoscalingNodeName(ctx, p.cluster, record.Group)
		if err != nil {
			return nil, err
		}
		// Reserve durably before creating any machine. Uncertain failures consume
		// the budget until the same claim is retried or explicitly deleted.
		p.records[uid] = record
		if err := writeJSON(p.statePath, p.records); err != nil {
			delete(p.records, uid)
			return nil, err
		}
	}
	nodeName, err := p.provisioner.AutoscalingNodeName(ctx, p.cluster, record.Group)
	if err != nil || nodeName != record.NodeName {
		return nil, errors.Join(err, mismatch(nodeName, record.NodeName))
	}
	labels := map[string]string{
		ownerLabel: "porto", runtimeLabel: record.Runtime, claimLabel: uid, corev1.LabelInstanceTypeStable: instanceName(record.Machine.Machine),
	}
	group := kubernetes.NodeGroupSpec{
		Name: record.Group, Count: 1, Machine: record.Machine.Machine, Labels: labels,
		Taints: []string{v1.UnregisteredTaintKey + ":NoExecute"},
	}
	for _, taint := range append(slices.Clone(claim.Spec.Taints), claim.Spec.StartupTaints...) {
		group.Taints = append(group.Taints, taint.ToString())
	}
	if err := p.provisioner.EnsureAutoscalingNode(ctx, p.cluster, group, request.Version); err != nil {
		return nil, fmt.Errorf("provision real Lima worker for %s: %w", claim.Name, err)
	}
	// Do not manufacture a Node. Wait for the actual kubelet, verify its exact
	// reserved identity, then supply the provider ID used by upstream registration.
	var node corev1.Node
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 4*time.Minute, true, func(ctx context.Context) (bool, error) {
		err := p.client.Get(ctx, types.NamespacedName{Name: record.NodeName}, &node)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}); err != nil {
		return nil, fmt.Errorf("wait for worker kubelet %s: %w", record.NodeName, err)
	}
	if err := verifyNode(&node, record, false); err != nil {
		return nil, err
	}
	if request.Provider == "k3s" && node.Spec.ProviderID == "k3s://"+record.NodeName && record.ProviderID != node.Spec.ProviderID {
		previous := record
		record.ProviderID = node.Spec.ProviderID
		p.records[uid] = record
		if err := writeJSON(p.statePath, p.records); err != nil {
			p.records[uid] = previous
			return nil, fmt.Errorf("persist verified native kubelet provider identity: %w", err)
		}
	}
	stored := node.DeepCopy()
	node.Spec.ProviderID = record.ProviderID
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	// A storage controller only removes this protection when the worker is empty
	// of local data; NodePool consolidation is restricted to WhenEmpty as well.
	node.Annotations[v1.DoNotDisruptAnnotationKey] = "true"
	if err := p.client.Patch(ctx, &node, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, fmt.Errorf("set verified kubelet provider identity: %w", err)
	}
	return p.hydrate(record), nil
}

func mismatch(actual, expected string) error {
	if actual == expected {
		return nil
	}
	return fmt.Errorf("node identity mismatch: got %q, expected %q", actual, expected)
}

func (p *localProvider) validateClaim(ctx context.Context, claim *v1.NodeClaim) error {
	if claim.UID == "" || strings.ContainsAny(string(claim.UID), "/\x00\r\n") ||
		claim.Spec.NodeClassRef == nil || claim.Spec.NodeClassRef.Group != NodeClassGroupVersion.Group ||
		claim.Spec.NodeClassRef.Kind != "PortoNodeClass" || claim.Spec.NodeClassRef.Name != managedName ||
		claim.Labels[v1.NodePoolLabelKey] != managedName || claim.Labels[ownerLabel] != "porto" {
		return errors.New("refusing foreign or invalid NodeClaim")
	}
	var nodeClass PortoNodeClass
	if err := p.client.Get(ctx, types.NamespacedName{Name: managedName}, &nodeClass); err != nil {
		return cloudprovider.NewNodeClassNotReadyError(err)
	}
	if nodeClass.Labels[ownerLabel] != "porto" || nodeClass.Spec.Machine != p.config.Machine {
		return errors.New("PortoNodeClass ownership or machine budget does not match persisted configuration")
	}
	typesOffered := p.instanceType()
	requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(claim.Spec.Requirements...)
	if err := typesOffered.Requirements.Compatible(requirements, scheduling.AllowUndefinedWellKnownLabels); err != nil {
		return cloudprovider.NewInsufficientCapacityError(err)
	}
	for name, requested := range claim.Spec.Resources.Requests {
		available := typesOffered.Allocatable()[name]
		if requested.Cmp(available) > 0 {
			return cloudprovider.NewInsufficientCapacityError(fmt.Errorf("worker cannot satisfy %s", name))
		}
	}
	return nil
}

func (p *localProvider) checkBudget(request kubernetes.ClusterRequest) error {
	if len(p.records) >= p.config.MaxNodes {
		return errors.New("Porto autoscaling reached its hard maxNodes worker budget")
	}
	cpu, memory, disk := p.config.Machine.CPUs, p.config.Machine.MemoryMiB, p.config.Machine.DiskGiB
	for _, record := range p.records {
		cpu += record.Machine.Machine.CPUs
		memory += record.Machine.Machine.MemoryMiB
		disk += record.Machine.Machine.DiskGiB
	}
	if cpu > 128 || memory > 262144 || disk > 4096 {
		return errors.New("Porto autoscaling reached its hard host resource budget")
	}
	for _, group := range request.NodeGroups {
		if !strings.HasPrefix(group.Name, "as-") || group.Count == 0 {
			continue
		}
		found := false
		for _, record := range p.records {
			if record.Group == group.Name && group.Count == 1 {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("untracked reserved node group %s; refusing to exceed the host budget", group.Name)
		}
	}
	return nil
}

func verifyNode(node *corev1.Node, record claimRecord, exact bool) error {
	if node.Name != record.NodeName || node.Labels[ownerLabel] != "porto" || node.Labels[runtimeLabel] != record.Runtime || node.Labels[claimLabel] != record.UID ||
		node.Labels["node-role.kubernetes.io/control-plane"] != "" || node.Labels["node-role.kubernetes.io/master"] != "" {
		return fmt.Errorf("refusing foreign or control-plane kubelet %s", node.Name)
	}
	_, controlPlane := node.Labels["node-role.kubernetes.io/control-plane"]
	_, master := node.Labels["node-role.kubernetes.io/master"]
	if controlPlane || master {
		return fmt.Errorf("refusing control-plane kubelet %s", node.Name)
	}
	if node.Spec.ProviderID == record.ProviderID {
		return nil
	}
	if !exact && (node.Spec.ProviderID == "" || node.Spec.ProviderID == "k3s://"+record.NodeName) {
		return nil
	}
	return fmt.Errorf("worker %s has unexpected providerID %q", node.Name, node.Spec.ProviderID)
}

func (p *localProvider) Delete(ctx context.Context, claim *v1.NodeClaim) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	record, ok := p.find(claim.Status.ProviderID)
	if !ok {
		return cloudprovider.NewNodeClaimNotFoundError(errors.New("Porto worker is not in the ownership ledger"))
	}
	if string(claim.UID) != record.UID || claim.Name != record.Name {
		return errors.New("refusing NodeClaim deletion with mismatched UID or name")
	}
	if claim.DeletionTimestamp.IsZero() {
		return errors.New("refusing to delete a NodeClaim that is not terminating")
	}
	release, err := p.operation(ctx, p.cluster, "autoscaling-delete")
	if err != nil {
		return err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	request, err := p.provisioner.Configuration(p.cluster)
	if err != nil {
		return err
	}
	if runtimeName(request) != record.Runtime {
		return errors.New("cluster runtime changed; refusing worker deletion")
	}
	nodeName, err := p.provisioner.AutoscalingNodeName(ctx, p.cluster, record.Group)
	if err != nil || nodeName != record.NodeName {
		return errors.Join(err, mismatch(nodeName, record.NodeName))
	}
	var node corev1.Node
	if err := p.client.Get(ctx, types.NamespacedName{Name: record.NodeName}, &node); err == nil {
		if err := verifyNode(&node, record, true); err != nil {
			return err
		}
		if node.DeletionTimestamp.IsZero() {
			return errors.New("waiting for upstream Karpenter node termination and drain")
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if protected, reason, err := storageProtected(ctx, p.client, record.NodeName); err != nil {
		return err
	} else if protected {
		return fmt.Errorf("preserving worker disk: %s; migrate local data before deleting this NodeClaim", reason)
	}
	if err := p.provisioner.DeleteAutoscalingNode(ctx, p.cluster, record.Group); err != nil {
		return err
	}
	delete(p.records, record.UID)
	if err := writeJSON(p.statePath, p.records); err != nil {
		p.records[record.UID] = record
		return err
	}
	return nil
}

func (p *localProvider) find(id string) (claimRecord, bool) {
	for _, record := range p.records {
		if record.ProviderID == id {
			return record, true
		}
	}
	return claimRecord{}, false
}

func (p *localProvider) Get(ctx context.Context, id string) (*v1.NodeClaim, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if record, ok := p.find(id); ok {
		exists, err := p.provisioner.AutoscalingNodeExists(ctx, p.cluster, record.Group)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("worker VM %s no longer exists", record.NodeName))
		}
		return p.hydrate(record), nil
	}
	return nil, cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("Porto worker %q not found", id))
}

func (p *localProvider) List(ctx context.Context) ([]*v1.NodeClaim, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	claims := make([]*v1.NodeClaim, 0, len(p.records))
	for _, record := range p.records {
		exists, err := p.provisioner.AutoscalingNodeExists(ctx, p.cluster, record.Group)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		claims = append(claims, p.hydrate(record))
	}
	return claims, nil
}

func (p *localProvider) hydrate(record claimRecord) *v1.NodeClaim {
	instance := instanceType(record.Machine.Machine)
	return &v1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: record.Name, UID: types.UID(record.UID), Labels: map[string]string{
			ownerLabel: "porto", runtimeLabel: record.Runtime, claimLabel: record.UID,
			v1.NodePoolLabelKey: managedName, corev1.LabelInstanceTypeStable: instance.Name,
			corev1.LabelArchStable: runtime.GOARCH, corev1.LabelOSStable: "linux",
			corev1.LabelTopologyZone: "local", corev1.LabelTopologyRegion: "local", v1.CapacityTypeLabelKey: v1.CapacityTypeOnDemand,
		}},
		Status: v1.NodeClaimStatus{ProviderID: record.ProviderID, Capacity: instance.Capacity, Allocatable: instance.Allocatable()},
	}
}

func instanceName(m kubernetes.MachineSpec) string {
	return fmt.Sprintf("porto-%dcpu-%dmi-%dgi", m.CPUs, m.MemoryMiB, m.DiskGiB)
}

func (p *localProvider) instanceType() *cloudprovider.InstanceType {
	return instanceType(p.config.Machine)
}
func instanceType(m kubernetes.MachineSpec) *cloudprovider.InstanceType {
	offering := scheduling.NewRequirements(
		scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "local"),
	)
	return &cloudprovider.InstanceType{
		Name: instanceName(m),
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(ownerLabel, corev1.NodeSelectorOpIn, "porto"),
			scheduling.NewRequirement(v1.NodeClassLabelKey(NodeClassGroupVersion.WithKind("PortoNodeClass").GroupKind()), corev1.NodeSelectorOpIn, managedName),
			scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, instanceName(m)),
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, runtime.GOARCH),
			scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, "linux"),
			scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, "local"),
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "local"),
			scheduling.NewRequirement(v1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, v1.CapacityTypeOnDemand),
		),
		Offerings: cloudprovider.Offerings{&cloudprovider.Offering{Requirements: offering, Available: true, Price: float64(m.CPUs)}},
		Capacity: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse(fmt.Sprint(m.CPUs)),
			corev1.ResourceMemory:           resource.MustParse(fmt.Sprintf("%dMi", m.MemoryMiB)),
			corev1.ResourceEphemeralStorage: resource.MustParse(fmt.Sprintf("%dGi", m.DiskGiB)),
			corev1.ResourcePods:             resource.MustParse("110"),
		},
		Overhead: &cloudprovider.InstanceTypeOverhead{KubeReserved: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("512Mi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("4Gi"),
		}},
	}
}

func (p *localProvider) GetInstanceTypes(_ context.Context, pool *v1.NodePool) ([]*cloudprovider.InstanceType, error) {
	if pool != nil && (pool.Name != managedName || pool.Labels[ownerLabel] != "porto") {
		return nil, errors.New("refusing foreign NodePool")
	}
	return []*cloudprovider.InstanceType{p.instanceType()}, nil
}
