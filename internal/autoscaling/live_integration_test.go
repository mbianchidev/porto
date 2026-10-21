package autoscaling

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeclient "k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// TestLocalAutoscalingLive creates real VMs only with explicit opt-in:
//
//	PORTO_AUTOSCALING_LIVE=1 go test ./internal/autoscaling -run '^TestLocalAutoscalingLive$' -count=1 -timeout=70m -v
//
// Add PORTO_AUTOSCALING_LIVE_KEDA=1 to start at zero workload replicas and
// activate them with a real KEDA cron ScaledObject. Both paths require Lima,
// kubectl, internet access and capacity for 2-CPU/4-GiB + 2-CPU/2-GiB VMs.
func TestLocalAutoscalingLive(t *testing.T) {
	if os.Getenv("PORTO_AUTOSCALING_LIVE") != "1" {
		t.Skip("real Lima autoscaling test; opt in with PORTO_AUTOSCALING_LIVE=1")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("live Lima autoscaling requires macOS or Linux")
	}
	for _, binary := range []string{"limactl", "kubectl"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatalf("live autoscaling prerequisite %s: %v", binary, err)
		}
	}
	withKEDA := os.Getenv("PORTO_AUTOSCALING_LIVE_KEDA") == "1"
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "al-" + hex.EncodeToString(nonce[:])
	root, err := os.MkdirTemp("/tmp", "pa-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cleanupRegistered := false
	t.Cleanup(func() {
		if !cleanupRegistered {
			if err := os.RemoveAll(root); err != nil {
				t.Errorf("remove unused private fixture state: %v", err)
			}
		}
	})
	longestSocket := filepath.Join(root, "l", "porto-"+name+"-as-0000000000-1", "ssh.sock.1234567890123456")
	unixPathMax := 108
	if runtime.GOOS == "darwin" {
		unixPathMax = 104
	}
	if len(longestSocket) >= unixPathMax {
		t.Fatalf("private Lima state cannot fit Unix socket limit: %d >= %d bytes (%s); run from a shorter physical repository path (a symlink does not help); no resources created",
			len(longestSocket), unixPathMax, longestSocket)
	}
	paths := map[string]string{
		"PORTO_HOME": filepath.Join(root, "porto"), "LIMA_HOME": filepath.Join(root, "l"),
		"HOME": filepath.Join(root, "home"), "TMPDIR": filepath.Join(root, "files"),
		"XDG_CONFIG_HOME": filepath.Join(root, "config"), "XDG_CACHE_HOME": filepath.Join(root, "cache"),
	}
	for key, path := range paths {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path)
	}
	privateConfig := filepath.Join(root, "unused-global-kubeconfig")
	t.Setenv("KUBECONFIG", privateConfig)
	paths["KUBECONFIG"] = privateConfig
	kubeRoot, err := config.KubernetesConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	vmRoot := filepath.Join(paths["PORTO_HOME"], "vms")
	if err := os.Mkdir(vmRoot, 0700); err != nil {
		t.Fatal(err)
	}
	kubePath := filepath.Join(kubeRoot, config.KubernetesClusterFileToken(name)+".yaml")
	runner := &liveAutoscalingRunner{
		name: name, kubePath: kubePath, workingDir: root, environment: paths, created: map[string]bool{},
		workerPattern: regexp.MustCompile(`^porto-` + regexp.QuoteMeta(name) + `-as-[0-9a-f]{10}-1$`),
	}
	vms := vm.NewWithStateDir(runner, vmRoot)
	// No KubeconfigRegistry is supplied: global context registration is disabled.
	provisioner := kubernetes.NewClusterProvisioner(vms, runner, kubeRoot)
	gate := make(chan struct{}, 1)
	operation := func(ctx context.Context, cluster, _ string) (func(), error) {
		if cluster != name {
			return nil, fmt.Errorf("live test refuses operation on cluster %q", cluster)
		}
		select {
		case gate <- struct{}{}:
			return func() { <-gate }, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	manager, err := New(provisioner, kubeRoot, operation)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	managerCtx, stopManager := context.WithCancel(ctx)
	var managerDone chan error
	var connection clusterConnection
	t.Cleanup(func() {
		cancel()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cleanupCancel()
		// Pause joins VM mutations, including their failure cleanup. Never
		// remove resources merely because a controller shutdown timed out.
		if err := manager.Pause(cleanupCtx, name); err != nil {
			t.Errorf("pause before exact-cluster cleanup: %v", err)
			return
		}
		stopManager()
		if managerDone != nil {
			select {
			case err := <-managerDone:
				if err != nil {
					t.Errorf("join autoscaling manager: %v", err)
				}
			case <-cleanupCtx.Done():
				t.Errorf("manager did not join; private resources retained at %s", root)
				return
			}
		}
		if t.Failed() && connection.client != nil {
			liveAutoscalingDiagnostics(t, cleanupCtx, connection.client)
		}
		release, err := operation(cleanupCtx, name, "live-test-delete")
		if err != nil {
			t.Errorf("acquire exact-cluster cleanup gate: %v", err)
			return
		}
		defer release()
		owned, err := vms.KubernetesNodeNames(name)
		if err != nil {
			t.Errorf("inspect synthetic VM ownership: %v", err)
			return
		}
		for _, machine := range owned {
			if !runner.wasCreated(machine) {
				t.Errorf("refusing cleanup of VM not created by this invocation: %s", machine)
				return
			}
		}
		if err := provisioner.Delete(cleanupCtx, name); err != nil {
			t.Errorf("delete exact synthetic cluster %s (state retained at %s): %v", name, root, err)
			return
		}
		remaining, err := vms.ListAll(cleanupCtx)
		if err != nil || len(remaining) != 0 {
			t.Errorf("private Lima VMs remain; retain %s: instances=%v error=%v", root, liveVMNames(remaining), err)
			return
		}
		if err := manager.Forget(cleanupCtx, name); err != nil {
			t.Errorf("remove synthetic autoscaling settings: %v", err)
			return
		}
		if _, err := os.Stat(privateConfig); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unexpected global-style kubeconfig write inside private root: %v", err)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove isolated synthetic test state: %v", err)
		}
	})
	cleanupRegistered = true
	t.Logf("isolated live autoscaling: cluster=%s state=%s KEDA=%t", name, root, withKEDA)

	createCtx, createCancel := context.WithTimeout(ctx, 20*time.Minute)
	cluster, err := func() (kubernetes.Cluster, error) {
		release, err := operation(createCtx, name, "live-test-create")
		if err != nil {
			return kubernetes.Cluster{}, err
		}
		defer release()
		return provisioner.Create(createCtx, kubernetes.ClusterRequest{
			Name: name, Provider: "k3s", // Deliberately exercise legacy unpinned metadata.
			ControlPlane: kubernetes.MachineSpec{CPUs: 2, MemoryMiB: 4096, DiskGiB: 20},
			NodeGroups:   []kubernetes.NodeGroupSpec{},
		})
	}()
	createCancel()
	if err != nil {
		t.Fatalf("create private zero-fixed-worker k3s cluster: %v", err)
	}
	controllerName := "porto-" + name + "-server-1"
	if cluster.Name != name || len(cluster.Nodes) != 1 || cluster.Nodes[0] != controllerName {
		t.Fatalf("unexpected initial topology: %+v", cluster)
	}
	connection, err = manager.connection(name)
	if err != nil {
		t.Fatal(err)
	}
	actualVersion, err := liveControllerRelease(ctx, vms, controllerName)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("actual controller release: %s; autoscaled worker must match exactly", actualVersion)
	managerDone = make(chan error, 1)
	go func() { managerDone <- manager.Run(managerCtx) }()
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(context.Context) (bool, error) {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.ctx != nil, nil
	}); err != nil {
		t.Fatalf("start host autoscaling manager: %v", err)
	}
	configureCtx, configureCancel := context.WithTimeout(ctx, 10*time.Minute)
	_, err = manager.Configure(configureCtx, name, Config{
		Nodes: NodeConfig{Enabled: true, MaxNodes: 1, Machine: kubernetes.MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}},
		KEDA:  withKEDA,
	})
	configureCancel()
	if err != nil {
		t.Fatalf("configure real upstream controllers: %v", err)
	}
	initial, err := vms.ListAll(ctx)
	if err != nil || len(initial) != 1 || initial[0].Name != controllerName {
		t.Fatalf("controllers must survive with zero workers: VMs=%v error=%v", liveVMNames(initial), err)
	}
	var initialClaims v1.NodeClaimList
	if err := connection.client.List(ctx, &initialClaims); err != nil || len(initialClaims.Items) != 0 {
		t.Fatalf("expected zero initial NodeClaims: count=%d error=%v", len(initialClaims.Items), err)
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "porto-autoscaling-live"}}
	if err := connection.client.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	replicas := int32(1)
	if withKEDA {
		replicas = 0
	}
	labels := map[string]string{"app.kubernetes.io/name": name}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "synthetic-http", Namespace: namespace.Name},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{v1.NodePoolLabelKey: managedName},
					Containers: []corev1.Container{{
						Name: "synthetic-http", Image: "docker.io/library/busybox:1.37.0",
						Command: []string{"sh", "-c", "mkdir -p /www && printf 'porto-autoscaling-live\\n' > /www/index.html && exec httpd -f -p 8080 -h /www"},
						Ports:   []corev1.ContainerPort{{ContainerPort: 8080}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
						},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(8080)},
						}, PeriodSeconds: 2},
					}},
				},
			},
		},
	}
	if err := connection.client.Create(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	if withKEDA {
		// The daily UTC window includes now, including across midnight. A long
		// end offset keeps cold VM/image downloads within the active window.
		now := time.Now().UTC()
		start, end := now.Add(-2*time.Minute), now.Add(2*time.Hour)
		if err := manager.ApplyScaledObject(ctx, name, ScaledObject{
			Namespace: namespace.Name, Name: "synthetic-cron", TargetName: deployment.Name,
			MinReplicas: 0, MaxReplicas: 1, PollingInterval: 5, CooldownPeriod: 15,
			Triggers: []Trigger{{Type: "cron", Metadata: map[string]string{
				"timezone": "Etc/UTC", "start": fmt.Sprintf("%d %d * * *", start.Minute(), start.Hour()),
				"end": fmt.Sprintf("%d %d * * *", end.Minute(), end.Hour()), "desiredReplicas": "1",
			}}},
		}); err != nil {
			t.Fatalf("create real KEDA cron workload scaler: %v", err)
		}
	}
	var claim v1.NodeClaim
	var worker corev1.Node
	var pod corev1.Pod
	var lastObservation string
	err = wait.PollUntilContextTimeout(ctx, 3*time.Second, 15*time.Minute, true, func(ctx context.Context) (bool, error) {
		var claims v1.NodeClaimList
		if err := connection.client.List(ctx, &claims, client.MatchingLabels{ownerLabel: "porto"}); err != nil {
			return false, err
		}
		if len(claims.Items) > 1 {
			return false, fmt.Errorf("hard maxNodes=1 exceeded: %d claims", len(claims.Items))
		}
		if len(claims.Items) == 0 {
			lastObservation = "waiting for upstream Karpenter to create a NodeClaim"
			return false, nil
		}
		claim = claims.Items[0]
		lastObservation = fmt.Sprintf("NodeClaim %s providerID=%q conditions=%v", claim.Name, claim.Status.ProviderID, claim.Status.Conditions)
		expectedWorker := "porto-" + name + "-" + claimGroup(string(claim.UID)) + "-1"
		if claim.Status.ProviderID == "" || claim.Status.NodeName == "" {
			return false, nil
		}
		if claim.Status.ProviderID != providerID(name, string(claim.UID)) || claim.Status.NodeName != expectedWorker {
			return false, fmt.Errorf("unexpected real worker identity: %s / %s", claim.Status.ProviderID, claim.Status.NodeName)
		}
		if err := connection.client.Get(ctx, types.NamespacedName{Name: expectedWorker}, &worker); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if worker.Spec.ProviderID != claim.Status.ProviderID || worker.Labels[claimLabel] != string(claim.UID) ||
			worker.Labels[runtimeLabel] != name || worker.Status.NodeInfo.KubeletVersion != actualVersion {
			return false, fmt.Errorf("worker kubelet identity or pinned release does not match: %+v", worker.Status.NodeInfo)
		}
		if !liveNodeReady(worker) || !claim.StatusConditions().IsTrue(status.ConditionReady) {
			return false, nil
		}
		instances, err := vms.ListAll(ctx)
		if err != nil {
			return false, err
		}
		names := liveVMNames(instances)
		if len(names) > 2 {
			return false, fmt.Errorf("hard VM bound exceeded: %v", names)
		}
		if len(names) != 2 || !slices.Contains(names, expectedWorker) || !slices.Contains(names, controllerName) || !runner.wasCreated(expectedWorker) {
			return false, fmt.Errorf("Ready node has no matching real private Lima VM: %v", names)
		}
		var current appsv1.Deployment
		if err := connection.client.Get(ctx, client.ObjectKeyFromObject(deployment), &current); err != nil {
			return false, err
		}
		if current.UID != deployment.UID || current.Spec.Replicas == nil || *current.Spec.Replicas != 1 || current.Status.AvailableReplicas != 1 {
			return false, nil
		}
		var pods corev1.PodList
		if err := connection.client.List(ctx, &pods, client.InNamespace(namespace.Name), client.MatchingLabels(labels)); err != nil {
			return false, err
		}
		if len(pods.Items) != 1 || pods.Items[0].Spec.NodeName != expectedWorker || !livePodReady(pods.Items[0]) {
			return false, nil
		}
		pod = pods.Items[0]
		return true, nil
	})
	if err != nil {
		t.Fatalf("real workload-to-VM/node/application scale-up failed: %v; last observation: %s", err, lastObservation)
	}
	kube, err := kubeclient.NewForConfig(connection.config)
	if err != nil {
		t.Fatal(err)
	}
	applicationCtx, applicationCancel := context.WithTimeout(ctx, 30*time.Second)
	body, err := kube.CoreV1().RESTClient().Get().Namespace(namespace.Name).
		Resource("pods").Name(pod.Name + ":8080").SubResource("proxy").Do(applicationCtx).Raw()
	applicationCancel()
	if err != nil || string(body) != "porto-autoscaling-live\n" {
		t.Fatalf("synthetic application HTTP failed: body=%q error=%v", body, err)
	}
	pinned, err := provisioner.Configuration(name)
	if err != nil || pinned.Version != actualVersion {
		t.Fatalf("worker provisioning did not pin actual controller release: version=%q error=%v", pinned.Version, err)
	}
	t.Logf("verified real worker VM=%s Ready node=%s release=%s application=%s/%s KEDA=%t",
		worker.Name, worker.Name, actualVersion, namespace.Name, pod.Name, withKEDA)
	if withKEDA {
		if err := manager.DeleteScaledObject(ctx, name, namespace.Name, "synthetic-cron"); err != nil {
			t.Fatal(err)
		}
	}
	foreground := metav1.DeletePropagationForeground
	if err := connection.client.Delete(ctx, deployment, &client.DeleteOptions{
		PropagationPolicy: &foreground, Preconditions: &metav1.Preconditions{UID: &deployment.UID},
	}); err != nil {
		t.Fatal(err)
	}
	err = wait.PollUntilContextTimeout(ctx, 3*time.Second, 8*time.Minute, true, func(ctx context.Context) (bool, error) {
		var pods corev1.PodList
		if err := connection.client.List(ctx, &pods, client.InNamespace(namespace.Name), client.MatchingLabels(labels)); err != nil {
			return false, err
		}
		if len(pods.Items) != 0 {
			return false, nil
		}
		var claims v1.NodeClaimList
		if err := connection.client.List(ctx, &claims, client.MatchingLabels{ownerLabel: "porto"}); err != nil {
			return false, err
		}
		var deletedNode corev1.Node
		nodeErr := connection.client.Get(ctx, types.NamespacedName{Name: worker.Name}, &deletedNode)
		if nodeErr != nil && !apierrors.IsNotFound(nodeErr) {
			return false, nodeErr
		}
		instances, err := vms.ListAll(ctx)
		if err != nil {
			return false, err
		}
		return len(claims.Items) == 0 && apierrors.IsNotFound(nodeErr) &&
			len(instances) == 1 && instances[0].Name == controllerName, nil
	})
	if err != nil {
		t.Fatalf("real scale-down to zero workers after workload deletion failed: %v", err)
	}
	var controller corev1.Node
	if err := connection.client.Get(ctx, types.NamespacedName{Name: controllerName}, &controller); err != nil || !liveNodeReady(controller) {
		t.Fatalf("control plane did not survive scale-to-zero: ready=%t error=%v", liveNodeReady(controller), err)
	}
	t.Log("verified upstream WhenEmpty consolidation removed the real worker VM and node; control plane remains Ready")
}

type liveAutoscalingRunner struct {
	name          string
	kubePath      string
	workingDir    string
	environment   map[string]string
	workerPattern *regexp.Regexp
	mu            sync.Mutex
	created       map[string]bool
}

func (r *liveAutoscalingRunner) wasCreated(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.created[name]
}

func (r *liveAutoscalingRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	switch filepath.Base(command.Name) {
	case "limactl":
		if len(command.Args) == 0 {
			return nil, errors.New("live test refuses unspecified Lima command")
		}
		switch command.Args[0] {
		case "--version", "list", "info":
		case "create":
			index := slices.Index(command.Args, "--name")
			if index < 0 || index+1 >= len(command.Args) {
				return nil, errors.New("live test requires an exact VM name")
			}
			name := command.Args[index+1]
			if name != "porto-"+r.name+"-server-1" && !r.workerPattern.MatchString(name) {
				return nil, fmt.Errorf("live test refuses VM creation outside synthetic topology: %s", name)
			}
			r.mu.Lock()
			r.created[name] = true
			r.mu.Unlock()
		case "start", "stop", "delete":
			if !r.wasCreated(command.Args[len(command.Args)-1]) {
				return nil, errors.New("live test refuses mutation of a VM not created by this invocation")
			}
		case "shell":
			beforeCommand := command.Args[1:]
			if index := slices.Index(beforeCommand, "--"); index >= 0 {
				beforeCommand = beforeCommand[:index]
			}
			if len(beforeCommand) == 0 || !r.wasCreated(beforeCommand[len(beforeCommand)-1]) {
				return nil, errors.New("live test refuses shell access to a VM not created by this invocation")
			}
		default:
			return nil, fmt.Errorf("live test refuses unexpected Lima command %q", command.Args[0])
		}
	case "kubectl":
		index := slices.Index(command.Args, "--kubeconfig")
		if index < 0 || index+1 >= len(command.Args) {
			return nil, errors.New("live test refuses kubectl without its exact private kubeconfig")
		}
		filename := command.Args[index+1]
		if filename != r.kubePath {
			staged := index == 0 && filepath.Dir(filename) == filepath.Dir(r.kubePath) &&
				strings.HasPrefix(filepath.Base(filename), ".porto-kubeconfig-refresh-") &&
				strings.Join(command.Args[index+2:], " ") == "config view --raw -o json --flatten"
			info, err := os.Lstat(filename)
			if !staged || err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				return nil, errors.New("live test refuses kubectl outside its private kubeconfig or protected staged normalization")
			}
		}
	default:
		return nil, fmt.Errorf("live test refuses unexpected host executable %q", command.Name)
	}
	for key, value := range r.environment {
		command.Env = append(command.Env, key+"="+value)
	}
	command.Dir = r.workingDir
	output, err := (runtimes.ExecRunner{}).Run(ctx, command)
	if err != nil && filepath.Base(command.Name) == "kubectl" && slices.Contains(command.Args, "wait") {
		for _, args := range [][]string{{"get", "nodes", "-o", "wide"}, {"get", "pods", "-A", "-o", "wide"}, {"get", "events", "-A", "--sort-by=.metadata.creationTimestamp"}} {
			diagnostic := command
			diagnostic.Args = append([]string{"--kubeconfig", r.kubePath, "--context", "porto-k3s-" + r.name}, args...)
			diagnostic.Stdin, diagnostic.StdinReader = nil, nil
			diagnosticCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			data, diagnosticErr := (runtimes.ExecRunner{}).Run(diagnosticCtx, diagnostic)
			stop()
			if len(data) > 64*1024 {
				data = data[len(data)-64*1024:]
			}
			output = append(output, []byte(fmt.Sprintf("\nPrivate cluster diagnostic %v (error=%v):\n%s", args, diagnosticErr, data))...)
		}
	}
	return output, err
}

func liveControllerRelease(ctx context.Context, vms *vm.Manager, name string) (string, error) {
	output, err := vms.Exec(ctx, name, []string{"sudo", "k3s", "--version"}, nil)
	if err != nil {
		return "", err
	}
	fullRelease := regexp.MustCompile(`^v1\.[0-9]+\.[0-9]+\+k3s[0-9]+$`)
	for _, field := range strings.Fields(string(output)) {
		if fullRelease.MatchString(field) {
			return field, nil
		}
	}
	return "", errors.New("synthetic controller did not report a full k3s release")
}

func liveVMNames(instances []vm.Instance) []string {
	names := make([]string, 0, len(instances))
	for _, instance := range instances {
		names = append(names, instance.Name)
	}
	return names
}

func liveNodeReady(node corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func livePodReady(pod corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func liveAutoscalingDiagnostics(t *testing.T, parent context.Context, c client.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	var claims v1.NodeClaimList
	if err := c.List(ctx, &claims); err == nil {
		for _, claim := range claims.Items {
			t.Logf("live diagnostic NodeClaim %s providerID=%q conditions=%v", claim.Name, claim.Status.ProviderID, claim.Status.Conditions)
		}

	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods); err == nil {
		for _, pod := range pods.Items {
			t.Logf("live diagnostic Pod %s/%s node=%q phase=%s conditions=%v", pod.Namespace, pod.Name, pod.Spec.NodeName, pod.Status.Phase, pod.Status.Conditions)
		}
	}
	var events corev1.EventList
	if err := c.List(ctx, &events, client.Limit(100)); err == nil {
		for _, event := range events.Items {
			if event.Type == corev1.EventTypeWarning {
				t.Logf("live diagnostic warning %s/%s %s: %.512s", event.Namespace, event.InvolvedObject.Name, event.Reason, event.Message)
			}
		}
	}
}

func TestLiveAutoscalingRunnerRefusesForeignRuntimeMutations(t *testing.T) {
	runner := &liveAutoscalingRunner{
		name: "al-synthetic", kubePath: "private-kubeconfig", created: map[string]bool{},
		workerPattern: regexp.MustCompile(`^porto-al-synthetic-as-[0-9a-f]{10}-1$`),
	}
	for _, command := range []runtimes.Command{
		{Name: "limactl", Args: []string{"delete", "--force", "porto-k0s-smoke-server-1"}},
		{Name: "limactl", Args: []string{"stop", "porto-k0s-smoke-server-1"}},
		{Name: "limactl", Args: []string{"start", "porto-k0s-smoke-server-1"}},
		{Name: "limactl", Args: []string{"shell", "porto-k0s-smoke-server-1", "--", "true"}},
		{Name: "limactl", Args: []string{"create", "--name", "porto-k0s-smoke-server-1"}},
		{Name: "kubectl", Args: []string{"config", "delete-context", "foreign"}},
		{Name: "kubectl", Args: []string{"--kubeconfig", "foreign-kubeconfig", "delete", "node", "foreign"}},
		{Name: "docker", Args: []string{"rm", "-f", "foreign"}},
	} {
		if _, err := runner.Run(context.Background(), command); err == nil {
			t.Fatalf("unsafe integration command was not rejected: %s %v", command.Name, command.Args)
		}
	}
}
