package autoscaling

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/mbianchidev/porto/internal/config"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

type clusterConnection struct {
	config *rest.Config
	client client.Client
}

type clusterRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Manager owns per-cluster controllers. Configuration and claim ledgers live
// under kubeconfigRoot/autoscaling, never among the cluster metadata JSON files.
type Manager struct {
	provisioner Provisioner
	root        string
	operation   Operation
	lifecycle   sync.Mutex
	mu          sync.Mutex
	ctx         context.Context
	runs        map[string]*clusterRun
	paused      map[string]bool
	failures    map[string]string
	connect     func(string) (clusterConnection, error)
	download    func(context.Context, string) ([]byte, error)
	run         func(context.Context, clusterConnection, *localProvider) error
}

func New(provisioner Provisioner, kubeconfigRoot string, operation Operation) (*Manager, error) {
	if provisioner == nil || operation == nil || kubeconfigRoot == "" {
		return nil, errors.New("autoscaling requires a provisioner, private kubeconfig root and shared cluster operation gate")
	}
	m := &Manager{
		provisioner: provisioner, root: kubeconfigRoot, operation: operation,
		runs: map[string]*clusterRun{}, paused: map[string]bool{}, failures: map[string]string{},
		download: downloadManifest, run: runController,
	}
	m.connect = m.connection
	return m, nil
}

func (m *Manager) directory(name string) string { return filepath.Join(m.root, "autoscaling", name) }
func (m *Manager) settings(name string) (Config, error) {
	var cfg Config
	err := readJSON(filepath.Join(m.directory(name), "settings.json"), &cfg)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	return cfg, err
}

func newScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		return nil, err
	}
	gv := schema.GroupVersion{Group: "karpenter.sh", Version: "v1"}
	s.AddKnownTypes(gv, &v1.NodePool{}, &v1.NodePoolList{}, &v1.NodeClaim{}, &v1.NodeClaimList{})
	metav1.AddToGroupVersion(s, gv)
	addNodeClass(s)
	return s, nil
}

func (m *Manager) connection(name string) (clusterConnection, error) {
	if err := validateName(name); err != nil {
		return clusterConnection{}, err
	}
	contextName, err := m.provisioner.ContextName(name)
	if err != nil {
		return clusterConnection{}, err
	}
	// ExplicitPath and CurrentContext prevent falling back to ambient KUBECONFIG
	// or the user's current context.
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: filepath.Join(m.root, config.KubernetesClusterFileToken(name)+".yaml")},
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		return clusterConnection{}, fmt.Errorf("load private kubeconfig for %s: %w", name, err)
	}
	cfg.QPS, cfg.Burst, cfg.Timeout, cfg.UserAgent = 50, 100, 0, "porto-autoscaling/"+KarpenterVersion
	s, err := newScheme()
	if err != nil {
		return clusterConnection{}, err
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	return clusterConnection{config: cfg, client: c}, err
}

// Run blocks for the daemon lifetime and restores all enabled controllers. A
// broken cluster is reported in Status without stopping unrelated controllers.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	if m.ctx != nil {
		m.mu.Unlock()
		return errors.New("autoscaling manager is already running")
	}
	m.ctx = ctx
	m.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(m.root, "autoscaling"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		m.mu.Lock()
		m.ctx = nil
		m.mu.Unlock()
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || validateName(entry.Name()) != nil {
			continue
		}
		m.lifecycle.Lock()
		m.mu.Lock()
		paused := m.paused[entry.Name()]
		m.mu.Unlock()
		var err error
		if !paused {
			err = m.resume(ctx, entry.Name())
		}
		m.lifecycle.Unlock()
		if err != nil {
			m.failure(entry.Name(), err)
		}
	}
	<-ctx.Done()
	m.lifecycle.Lock()
	m.mu.Lock()
	names := make([]string, 0, len(m.runs))
	for name := range m.runs {
		names = append(names, name)
	}
	m.mu.Unlock()
	for _, name := range names {
		_ = m.pause(ctx, name)
	}
	m.mu.Lock()
	m.ctx = nil
	m.mu.Unlock()
	m.lifecycle.Unlock()
	return nil
}

func (m *Manager) failure(name string, err error) {
	m.mu.Lock()
	m.failures[name] = err.Error()
	m.mu.Unlock()
	slog.Error("Porto autoscaling", "cluster", name, "error", err)
}

// Pause cancels and joins the upstream manager and all in-flight mutations.
// Call it before taking the daemon operation gate, not while holding the gate.
func (m *Manager) Pause(ctx context.Context, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	return m.pause(ctx, name)
}

func (m *Manager) pause(ctx context.Context, name string) error {
	m.mu.Lock()
	m.paused[name] = true
	run := m.runs[name]
	m.mu.Unlock()
	if run == nil {
		return nil
	}
	run.cancel()
	// Joining is deliberately unconditional: returning success while a mutation
	// still runs would permit stop/upgrade/delete to race it.
	<-run.done
	return nil
}

func (m *Manager) Resume(ctx context.Context, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	return m.resume(ctx, name)
}

func (m *Manager) resume(ctx context.Context, name string) error {
	cfg, err := m.settings(name)
	if err != nil {
		return err
	}
	request, err := m.provisioner.Configuration(name)
	if err != nil {
		return err
	}
	if err := validateConfig(request, cfg); err != nil {
		return err
	}
	m.mu.Lock()
	m.paused[name] = false
	parent := m.ctx
	existing := m.runs[name]
	m.mu.Unlock()
	if !cfg.Nodes.Enabled || parent == nil || existing != nil {
		return nil
	}
	connection, err := m.connect(name)
	if err != nil {
		return err
	}
	provider, err := newProvider(m.provisioner, m.operation, name, m.directory(name), cfg.Nodes, connection.client)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(parent)
	run := &clusterRun{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.runs[name] = run
	delete(m.failures, name)
	m.mu.Unlock()
	go func() {
		defer close(run.done)
		err := m.run(runCtx, connection, provider)
		cancel()
		m.mu.Lock()
		delete(m.runs, name)
		m.mu.Unlock()
		if err != nil && runCtx.Err() == nil {
			m.failure(name, err)
		} else if err != nil && !errors.Is(err, context.Canceled) && parent.Err() == nil {
			m.failure(name, err)
		}
	}()
	return nil
}

func (m *Manager) Configure(ctx context.Context, name string, cfg Config) (Status, error) {
	if err := validateName(name); err != nil {
		return Status{}, err
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	request, err := m.provisioner.Configuration(name)
	if err == nil {
		err = validateConfig(request, cfg)
	}
	if err != nil {
		return Status{}, err
	}
	old, err := m.settings(name)
	if err != nil {
		return Status{}, err
	}
	if err := m.pause(ctx, name); err != nil {
		return Status{}, err
	}
	err = m.configure(ctx, name, cfg, old)
	if err != nil {
		m.failure(name, err)
		// Persisted settings remain unchanged. Leave controllers paused after
		// partial installation; Resume is explicit and safe.
		status, _ := m.status(ctx, name, false)
		status.Message = err.Error()
		return status, err
	}
	if err := m.resume(ctx, name); err != nil {
		m.failure(name, err)
		status, _ := m.status(ctx, name, false)
		return status, err
	}
	return m.status(ctx, name, true)
}

func (m *Manager) configure(ctx context.Context, name string, cfg, old Config) error {
	release, err := m.operation(ctx, name, "autoscaling-configure")
	if err != nil {
		return err
	}
	defer release()
	request, err := m.provisioner.Configuration(name)
	if err != nil {
		return err
	}
	if err := validateConfig(request, cfg); err != nil {
		return err
	}
	var records map[string]claimRecord
	if err := readJSON(filepath.Join(m.directory(name), "claims.json"), &records); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if cfg.Nodes.Enabled && cfg.Nodes.MaxNodes < len(records) {
		return fmt.Errorf("maxNodes cannot be below the %d reserved workers; remove or consolidate workers first", len(records))
	}
	connection, err := m.connect(name)
	if err != nil {
		return err
	}
	if cfg.KEDA {
		data, err := m.download(ctx, kedaManifestURL)
		if err != nil {
			return err
		}
		if err := installManifest(ctx, connection.client, data); err != nil {
			return err
		}
		if err := kedaReady(ctx, connection.client); err != nil {
			return err
		}
	} else if old.KEDA {
		if err := disableKEDA(ctx, connection.client); err != nil {
			return err
		}
	}
	if cfg.Nodes.Enabled {
		if err := installKarpenter(ctx, connection.client); err != nil {
			return err
		}
		if err := configureNodePool(ctx, connection.client, cfg.Nodes); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(m.directory(name), "settings.json"), cfg)
}

func (m *Manager) Status(ctx context.Context, name string) (Status, error) {
	if err := validateName(name); err != nil {
		return Status{}, err
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	return m.status(ctx, name, true)
}

func (m *Manager) status(ctx context.Context, name string, live bool) (Status, error) {
	cfg, err := m.settings(name)
	if err != nil {
		return Status{}, err
	}
	request, err := m.provisioner.Configuration(name)
	if err != nil {
		return Status{}, err
	}
	s := Status{Config: cfg, Cluster: name, NodeSupported: request.Provider == "k3s" || request.Provider == "k0s",
		KarpenterVersion: KarpenterVersion, KEDAVersion: KEDAVersion}
	var records map[string]claimRecord
	if err := readJSON(filepath.Join(m.directory(name), "claims.json"), &records); err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	s.ManagedNodes = len(records)
	m.mu.Lock()
	s.Running, s.Paused, s.Message = m.runs[name] != nil, m.paused[name], m.failures[name]
	m.mu.Unlock()
	if !s.NodeSupported && s.Message == "" {
		s.Message = "kind topology is immutable; KEDA workload scaling is supported, node scaling requires k3s or k0s"
	}
	if cfg.Nodes.Enabled && live && s.Message == "" {
		if connection, err := m.connect(name); err != nil {
			s.Message = err.Error()
		} else if message, err := nodeStatus(ctx, connection.client); err != nil {
			s.Message = err.Error()
		} else {
			s.Message = message
		}
	}
	if cfg.KEDA && live {
		if connection, err := m.connect(name); err != nil {
			s.Message = err.Error()
		} else {
			s.KEDAReady = true
			for _, name := range []string{"keda-operator", "keda-operator-metrics-apiserver", "keda-admission-webhooks"} {
				var deployment appsv1.Deployment
				if err := connection.client.Get(ctx, types.NamespacedName{Namespace: "keda", Name: name}, &deployment); err != nil {
					s.KEDAReady, s.Message = false, err.Error()
					break
				}
				s.KEDAReady = s.KEDAReady && deployment.Labels[ownerLabel] == "porto" &&
					deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.AvailableReplicas > 0
			}
		}
	}
	return s, nil
}

func nodeStatus(ctx context.Context, c client.Client) (string, error) {
	var claims v1.NodeClaimList
	if err := c.List(ctx, &claims, client.MatchingLabels{ownerLabel: "porto"}); err != nil {
		return "", err
	}
	for _, claim := range claims.Items {
		for _, condition := range claim.Status.Conditions {
			if condition.Reason == "LaunchFailed" || condition.Status == metav1.ConditionFalse {
				return fmt.Sprintf("NodeClaim %s: %s: %s", claim.Name, condition.Reason, condition.Message), nil
			}
		}
	}
	var pool v1.NodePool
	if err := c.Get(ctx, types.NamespacedName{Name: managedName}, &pool); err != nil {
		return "", err
	}
	if condition := pool.StatusConditions().Get(status.ConditionReady); !condition.IsTrue() {
		return fmt.Sprintf("NodePool %s: %s: %s", pool.Name, condition.Reason, condition.Message), nil
	}
	return "", nil
}

// Forget removes only host-side settings after the daemon has deleted a cluster.
// It never deletes Kubernetes objects, VMs, or user workload configuration.
func (m *Manager) Forget(ctx context.Context, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if err := m.pause(ctx, name); err != nil {
		return err
	}
	return os.RemoveAll(m.directory(name))
}

// Rename moves paused host settings after a successful cluster rename. Existing
// claim identities keep the runtime name; Resume loads the new kubeconfig.
func (m *Manager) Rename(ctx context.Context, oldName, newName string) error {
	if err := errors.Join(validateName(oldName), validateName(newName)); err != nil {
		return err
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if err := m.pause(ctx, oldName); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	if _, err := os.Stat(m.directory(newName)); err == nil {
		return errors.New("destination already has autoscaling settings")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(m.directory(oldName), m.directory(newName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func configureNodePool(ctx context.Context, c client.Client, cfg NodeConfig) error {
	nodeClass := &PortoNodeClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: NodeClassGroupVersion.String(), Kind: "PortoNodeClass"},
		ObjectMeta: metav1.ObjectMeta{Name: managedName, Labels: map[string]string{ownerLabel: "porto"}},
		Spec:       PortoNodeClassSpec{Machine: cfg.Machine},
	}
	pool := &v1.NodePool{
		TypeMeta:   metav1.TypeMeta{APIVersion: "karpenter.sh/v1", Kind: "NodePool"},
		ObjectMeta: metav1.ObjectMeta{Name: managedName, Labels: map[string]string{ownerLabel: "porto"}},
		Spec: v1.NodePoolSpec{
			Template: v1.NodeClaimTemplate{ObjectMeta: v1.ObjectMeta{Labels: map[string]string{ownerLabel: "porto"}}, Spec: v1.NodeClaimTemplateSpec{
				NodeClassRef: &v1.NodeClassReference{Group: NodeClassGroupVersion.Group, Kind: "PortoNodeClass", Name: managedName},
				ExpireAfter:  v1.NillableDuration{},
				Requirements: []v1.NodeSelectorRequirementWithMinValues{{
					Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"},
				}},
			}},
			Disruption: v1.Disruption{
				ConsolidationPolicy: v1.ConsolidationPolicyWhenEmpty,
				ConsolidateAfter:    v1.NillableDuration{Duration: durationPointer()},
				Budgets:             []v1.Budget{{Nodes: "1"}},
			},
			Limits: v1.Limits(corev1.ResourceList{
				corev1.ResourceCPU:    quantity(cfg.MaxNodes * cfg.Machine.CPUs),
				corev1.ResourceMemory: memoryQuantity(cfg.MaxNodes * cfg.Machine.MemoryMiB),
			}),
		},
	}
	for _, object := range []client.Object{nodeClass, pool} {
		if err := applyOwnedObject(ctx, c, object); err != nil {
			return err
		}
	}
	nodeClass.StatusConditions().SetTrue(status.ConditionReady)
	return c.Status().Update(ctx, nodeClass)
}

func durationPointer() *time.Duration        { d := time.Minute; return &d }
func quantity(n int) resource.Quantity       { return resource.MustParse(fmt.Sprint(n)) }
func memoryQuantity(n int) resource.Quantity { return resource.MustParse(fmt.Sprintf("%dMi", n)) }

func applyOwnedObject(ctx context.Context, c client.Client, object client.Object) error {
	s, err := newScheme()
	if err != nil {
		return err
	}
	gvks, _, err := s.ObjectKinds(object)
	if err != nil || len(gvks) == 0 {
		return fmt.Errorf("resolve autoscaling object kind: %w", err)
	}
	converted, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: converted}
	u.SetGroupVersionKind(gvks[0])
	if err := checkOwned(ctx, c, u); err != nil {
		return err
	}
	return c.Patch(ctx, object, client.Apply, client.FieldOwner("porto-autoscaling"))
}
