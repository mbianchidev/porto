package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubeclient "k8s.io/client-go/kubernetes"
)

type AppleHealthSnapshot struct {
	CapturedAt string                   `json:"capturedAt"`
	Workloads  []AppleWorkloadHealth    `json:"workloads"`
	Probes     []AppleServiceProbe      `json:"probes,omitempty"`
	Failures   []string                 `json:"failures,omitempty"`
	Shutdown   map[string]ApplePodGrace `json:"shutdown"`
}

type ApplePodGrace struct {
	Regular  int64 `json:"regular"`
	Critical int64 `json:"critical"`
}

type AppleWorkloadHealth struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
	Desired    int32  `json:"desired"`
	Healthy    bool   `json:"healthy"`
	Ignored    bool   `json:"ignored,omitempty"`
}

type AppleServiceProbe struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Port      int32  `json:"port"`
	HTTP      bool   `json:"http"`
}

func (p *ClusterProvisioner) nativeAppleClient(request ClusterRequest) (kubeclient.Interface, error) {
	if p.appleClient != nil {
		return p.appleClient(request)
	}
	configuration, _, err := NewWithKubeconfigRoot(p.runner, p.kubeconfigRoot).AdminConfig(clusterContextName(request))
	if err != nil {
		return nil, err
	}
	if configuration.Insecure || len(configuration.CAData) == 0 || request.Apple == nil ||
		appleDigest(configuration.CAData) != request.Apple.CAHash {
		return nil, errors.New("native kubeconfig TLS authority does not match the recorded cluster identity")
	}
	configuration.Timeout = 15 * time.Second
	return kubeclient.NewForConfig(configuration)
}

func appleDesired(value *int32) int32 {
	if value == nil {
		return 1
	}
	return *value
}

func appleDeploymentHealthy(deployment appsv1.Deployment) (bool, bool) {
	desired := appleDesired(deployment.Spec.Replicas)
	ignored := deployment.Spec.Paused || desired == 0
	return deployment.Status.ObservedGeneration >= deployment.Generation &&
		deployment.Status.AvailableReplicas >= desired && deployment.Status.ReadyReplicas >= desired, ignored
}

func appleStatefulSetHealthy(set appsv1.StatefulSet) (bool, bool) {
	desired := appleDesired(set.Spec.Replicas)
	return set.Status.ObservedGeneration >= set.Generation && set.Status.ReadyReplicas >= desired, desired == 0
}

func appleDaemonSetHealthy(set appsv1.DaemonSet) (bool, bool) {
	return set.Status.ObservedGeneration >= set.Generation && set.Status.NumberReady >= set.Status.DesiredNumberScheduled &&
		set.Status.NumberAvailable >= set.Status.DesiredNumberScheduled, set.Status.DesiredNumberScheduled == 0
}

func appleJobHealthy(job batchv1.Job) (bool, bool) {
	if job.Spec.Suspend != nil && *job.Spec.Suspend {
		return true, true
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true, true
		}
	}
	return job.Status.Ready != nil && *job.Status.Ready > 0, false
}

func applePodReady(pod corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodSucceeded {
		return true
	}
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (p *ClusterProvisioner) captureAppleHealth(ctx context.Context, request ClusterRequest) (*AppleHealthSnapshot, error) {
	client, err := p.nativeAppleClient(request)
	if err != nil {
		return nil, err
	}
	snapshot := &AppleHealthSnapshot{CapturedAt: time.Now().UTC().Format(time.RFC3339Nano), Shutdown: make(map[string]ApplePodGrace)}
	type activeTemplate struct {
		namespace string
		labels    map[string]string
	}
	var active []activeTemplate
	add := func(kind string, metadata metav1.ObjectMeta, desired int32, healthy, ignored bool) {
		snapshot.Workloads = append(snapshot.Workloads, AppleWorkloadHealth{
			Kind: kind, Namespace: metadata.Namespace, Name: metadata.Name, UID: string(metadata.UID),
			Generation: metadata.Generation, Desired: desired, Healthy: healthy, Ignored: ignored,
		})
		if !healthy && !ignored {
			snapshot.Failures = append(snapshot.Failures, fmt.Sprintf("%s %s/%s was already unavailable", kind, metadata.Namespace, metadata.Name))
		}
	}
	deployments, err := client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("snapshot native Deployments: %w", err)
	}
	for _, item := range deployments.Items {
		healthy, ignored := appleDeploymentHealthy(item)
		add("Deployment", item.ObjectMeta, appleDesired(item.Spec.Replicas), healthy, ignored)
		if healthy && !ignored {
			active = append(active, activeTemplate{item.Namespace, item.Spec.Template.Labels})
		}
	}
	sets, err := client.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("snapshot native StatefulSets: %w", err)
	}
	for _, item := range sets.Items {
		healthy, ignored := appleStatefulSetHealthy(item)
		add("StatefulSet", item.ObjectMeta, appleDesired(item.Spec.Replicas), healthy, ignored)
		if healthy && !ignored {
			active = append(active, activeTemplate{item.Namespace, item.Spec.Template.Labels})
		}
	}
	daemonSets, err := client.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("snapshot native DaemonSets: %w", err)
	}
	for _, item := range daemonSets.Items {
		healthy, ignored := appleDaemonSetHealthy(item)
		add("DaemonSet", item.ObjectMeta, item.Status.DesiredNumberScheduled, healthy, ignored)
		if healthy && !ignored {
			active = append(active, activeTemplate{item.Namespace, item.Spec.Template.Labels})
		}
	}
	jobs, err := client.BatchV1().Jobs("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("snapshot native Jobs: %w", err)
	}
	for _, item := range jobs.Items {
		healthy, ignored := appleJobHealthy(item)
		add("Job", item.ObjectMeta, 1, healthy, ignored)
	}
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("snapshot native standalone Pods: %w", err)
	}
	for _, item := range pods.Items {
		if item.Status.Phase != corev1.PodSucceeded && item.Status.Phase != corev1.PodFailed && item.Spec.NodeName != "" {
			grace := int64(30)
			if item.Spec.TerminationGracePeriodSeconds != nil {
				grace = *item.Spec.TerminationGracePeriodSeconds
			}
			budget := snapshot.Shutdown[item.Spec.NodeName]
			if item.Spec.Priority != nil && *item.Spec.Priority >= 2000000000 {
				budget.Critical = max(budget.Critical, grace)
			} else {
				budget.Regular = max(budget.Regular, grace)
			}
			snapshot.Shutdown[item.Spec.NodeName] = budget
		}
		if len(item.OwnerReferences) == 0 {
			ignored := item.Status.Phase == corev1.PodSucceeded || len(item.Spec.SchedulingGates) > 0
			add("Pod", item.ObjectMeta, 1, applePodReady(item), ignored)
			if applePodReady(item) && !ignored {
				active = append(active, activeTemplate{item.Namespace, item.Labels})
			}
		}
	}
	services, err := client.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("snapshot native Services: %w", err)
	}
	slices.SortFunc(services.Items, func(a, b corev1.Service) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	for _, service := range services.Items {
		if appleSystemNamespace(service.Namespace) || service.Spec.Type == corev1.ServiceTypeExternalName {
			continue
		}
		matchesHealthyWork := false
		if len(service.Spec.Selector) > 0 {
			selector := labels.SelectorFromSet(service.Spec.Selector)
			for _, template := range active {
				matchesHealthyWork = matchesHealthyWork || template.namespace == service.Namespace && selector.Matches(labels.Set(template.labels))
			}
		}
		if !matchesHealthyWork {
			continue
		}
		address, err := netip.ParseAddr(service.Spec.ClusterIP)
		if err != nil || !address.Is4() {
			continue
		}
		endpoints, err := client.DiscoveryV1().EndpointSlices(service.Namespace).List(ctx,
			metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=" + service.Name})
		if err != nil {
			return nil, fmt.Errorf("snapshot native Service endpoints: %w", err)
		}
		ready := false
		for _, slice := range endpoints.Items {
			for _, endpoint := range slice.Endpoints {
				ready = ready || endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready
			}
		}
		if !ready {
			continue
		}
		for _, port := range service.Spec.Ports {
			if port.Protocol != corev1.ProtocolTCP && port.Protocol != "" {
				continue
			}
			probe := AppleServiceProbe{Namespace: service.Namespace, Name: service.Name, UID: string(service.UID), Port: port.Port}
			probe.HTTP = port.Name == "http" || port.Name == "web" || strings.HasPrefix(port.Name, "http-") ||
				(port.AppProtocol != nil && *port.AppProtocol == "http")
			if err := p.probeAppleService(ctx, request, probe); err == nil {
				snapshot.Probes = append(snapshot.Probes, probe)
				break
			} else {
				snapshot.Failures = append(snapshot.Failures, fmt.Sprintf("Service %s/%s did not answer its pre-stop endpoint probe", service.Namespace, service.Name))
			}
		}
		if len(snapshot.Probes) > 0 {
			break
		}
	}
	return snapshot, nil
}

func appleSystemNamespace(namespace string) bool {
	return slices.Contains([]string{"kube-system", "kube-public", "kube-node-lease", "porto-system", "envoy-gateway-system", "local-path-storage"}, namespace)
}

func (p *ClusterProvisioner) checkAppleWorkloads(ctx context.Context, request ClusterRequest, snapshot *AppleHealthSnapshot) error {
	if snapshot == nil {
		return nil
	}
	client, err := p.nativeAppleClient(request)
	if err != nil {
		return err
	}
	for _, expected := range snapshot.Workloads {
		if expected.Ignored || !expected.Healthy {
			continue
		}
		var metadata metav1.ObjectMeta
		healthy, ignored := false, false
		switch expected.Kind {
		case "Deployment":
			item, getErr := client.AppsV1().Deployments(expected.Namespace).Get(ctx, expected.Name, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("recover Deployment %s/%s: %w", expected.Namespace, expected.Name, getErr)
			}
			metadata = item.ObjectMeta
			healthy, ignored = appleDeploymentHealthy(*item)
		case "StatefulSet":
			item, getErr := client.AppsV1().StatefulSets(expected.Namespace).Get(ctx, expected.Name, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("recover StatefulSet %s/%s: %w", expected.Namespace, expected.Name, getErr)
			}
			metadata = item.ObjectMeta
			healthy, ignored = appleStatefulSetHealthy(*item)
		case "DaemonSet":
			item, getErr := client.AppsV1().DaemonSets(expected.Namespace).Get(ctx, expected.Name, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("recover DaemonSet %s/%s: %w", expected.Namespace, expected.Name, getErr)
			}
			metadata = item.ObjectMeta
			healthy, ignored = appleDaemonSetHealthy(*item)
		case "Job":
			item, getErr := client.BatchV1().Jobs(expected.Namespace).Get(ctx, expected.Name, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("recover Job %s/%s: %w", expected.Namespace, expected.Name, getErr)
			}
			metadata = item.ObjectMeta
			healthy, ignored = appleJobHealthy(*item)
		case "Pod":
			item, getErr := client.CoreV1().Pods(expected.Namespace).Get(ctx, expected.Name, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("recover Pod %s/%s: %w", expected.Namespace, expected.Name, getErr)
			}
			metadata, healthy = item.ObjectMeta, applePodReady(*item)
		default:
			return fmt.Errorf("unsupported recovery workload kind %s", expected.Kind)
		}
		if string(metadata.UID) != expected.UID {
			return fmt.Errorf("%s %s/%s was replaced; prior workload recovery cannot be assumed", expected.Kind, expected.Namespace, expected.Name)
		}
		if !healthy && !ignored {
			return fmt.Errorf("previously healthy %s %s/%s has not recovered", expected.Kind, expected.Namespace, expected.Name)
		}
	}
	return nil
}

func (p *ClusterProvisioner) checkAppleNodes(ctx context.Context, request *ClusterRequest, initial bool) error {
	client, err := p.nativeAppleClient(*request)
	if err != nil {
		return err
	}
	for index := range request.Apple.Nodes {
		expected := &request.Apple.Nodes[index]
		node, err := client.CoreV1().Nodes().Get(ctx, expected.ID, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("wait for native Kubernetes node %s: %w", expected.ID, err)
		}
		if node.UID == "" || !initial && expected.KubernetesUID == "" {
			return fmt.Errorf("native node %s has no recorded Kubernetes membership identity", expected.ID)
		}
		if !initial && expected.KubernetesUID != "" && string(node.UID) != expected.KubernetesUID {
			return fmt.Errorf("Kubernetes membership identity changed for native node %s", expected.ID)
		}
		ready, address := false, false
		for _, condition := range node.Status.Conditions {
			ready = ready || condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue
		}
		for _, item := range node.Status.Addresses {
			address = address || item.Type == corev1.NodeInternalIP && item.Address == expected.IPv4
		}
		if !ready || !address || expected.BootID == "" || node.Status.NodeInfo.BootID != expected.BootID {
			return fmt.Errorf("native node %s has not registered its current boot, address and Ready condition", expected.ID)
		}
		if expected.PodCIDR != "" && node.Spec.PodCIDR != expected.PodCIDR {
			return fmt.Errorf("native node %s pod network membership changed", expected.ID)
		}
		if initial {
			expected.KubernetesUID, expected.PodCIDR = string(node.UID), node.Spec.PodCIDR
		}
	}
	return nil
}

func (p *ClusterProvisioner) checkAppleSystemPods(ctx context.Context, request ClusterRequest) error {
	client, err := p.nativeAppleClient(request)
	if err != nil {
		return err
	}
	network, err := client.AppsV1().DaemonSets("kube-system").Get(ctx, "kindnet", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read native CNI readiness: %w", err)
	}
	if healthy, ignored := appleDaemonSetHealthy(*network); !healthy || ignored || network.Status.NumberReady < int32(len(request.Apple.Nodes)) {
		return errors.New("native CNI has not recovered on every expected node")
	}
	dns, err := client.AppsV1().Deployments("kube-system").Get(ctx, "coredns", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read native DNS readiness: %w", err)
	}
	if healthy, ignored := appleDeploymentHealthy(*dns); !healthy || ignored {
		return errors.New("native DNS has not recovered")
	}
	return nil
}

func (p *ClusterProvisioner) probeAppleService(ctx context.Context, request ClusterRequest, expected AppleServiceProbe) error {
	client, err := p.nativeAppleClient(request)
	if err != nil {
		return err
	}
	service, err := client.CoreV1().Services(expected.Namespace).Get(ctx, expected.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(service.UID) != expected.UID {
		return fmt.Errorf("representative Service %s/%s was replaced", expected.Namespace, expected.Name)
	}
	address, err := netip.ParseAddr(service.Spec.ClusterIP)
	if err != nil || !address.Is4() || expected.Port <= 0 || expected.Port > 65535 {
		return errors.New("representative Service has no valid IPv4/TCP endpoint")
	}
	controlPlane := ""
	for _, node := range request.Apple.Nodes {
		if node.Role == "control-plane" {
			controlPlane = node.ID
		}
	}
	if controlPlane == "" {
		return errors.New("representative Service probe has no owned control-plane node")
	}
	if expected.HTTP {
		output, err := p.appleGuest(ctx, controlPlane, 10*time.Second, nil, "/usr/bin/curl",
			"--silent", "--show-error", "--output", "/dev/null", "--write-out", "%{http_code}",
			"--connect-timeout", "2", "--max-time", "5", "http://"+net.JoinHostPort(address.String(), strconv.Itoa(int(expected.Port)))+"/")
		if err != nil {
			return err
		}
		status, err := strconv.Atoi(strings.TrimSpace(string(output)))
		if err != nil || status < 200 || status >= 500 {
			return fmt.Errorf("representative Service %s/%s returned HTTP %q", expected.Namespace, expected.Name, output)
		}
		return nil
	}
	_, err = p.appleGuest(ctx, controlPlane, 10*time.Second, nil, "/usr/bin/timeout", "5",
		"/bin/bash", "-c", `exec 3<>/dev/tcp/"$1"/"$2"; exec 3<&-; exec 3>&-`, "sh", address.String(), strconv.Itoa(int(expected.Port)))
	return err
}
