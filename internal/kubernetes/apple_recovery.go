package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

func (p *ClusterProvisioner) startAppleNode(ctx context.Context, request *ClusterRequest, node *AppleNode) error {
	containers, err := p.ownedAppleNodes(ctx, *request)
	if err != nil {
		return err
	}
	current := containers[node.ID]
	switch current.Status.State {
	case "stopped":
		startContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
		output, err := p.runApple(startContext, runtimes.Command{Args: []string{"start", node.ID}})
		cancel()
		if err != nil {
			return runtimes.CommandError("start owned native node "+node.ID, output, err)
		}
	case "running":
	default:
		return fmt.Errorf("native node %s is %s; retry its pending transition", node.ID, current.Status.State)
	}
	if err := p.waitAppleGuest(ctx, node); err != nil {
		return err
	}
	containers, err = p.ownedAppleNodes(ctx, *request)
	if err != nil {
		return err
	}
	node.IPv4, err = appleNodeIPv4(containers[node.ID])
	if err != nil {
		return err
	}
	if err := p.prepareAppleNetwork(ctx, node.ID); err != nil {
		return err
	}
	return p.appleProgress(request, "node-services-ready", node.ID, "running")
}

func (p *ClusterProvisioner) waitAppleDatastoreAndAPI(ctx context.Context, controlPlane string) error {
	return waitApple(ctx, "wait for native datastore and verified API", func(probe context.Context) error {
		output, err := p.appleGuest(probe, controlPlane, 10*time.Second, nil, "/usr/bin/curl",
			"--fail", "--silent", "--show-error", "--connect-timeout", "2", "--max-time", "5",
			"--cacert", "/etc/kubernetes/pki/etcd/ca.crt",
			"--cert", "/etc/kubernetes/pki/etcd/healthcheck-client.crt",
			"--key", "/etc/kubernetes/pki/etcd/healthcheck-client.key",
			"https://127.0.0.1:2379/health")
		if err != nil {
			return err
		}
		var health struct {
			Health string `json:"health"`
		}
		if err := json.Unmarshal(output, &health); err != nil || health.Health != "true" {
			return errors.New("native etcd has not recovered its existing datastore")
		}
		output, err = p.appleGuest(probe, controlPlane, 10*time.Second, nil,
			"/usr/bin/kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "get", "--raw", "/readyz")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(output)) != "ok" {
			return errors.New("native Kubernetes API has not completed readiness")
		}
		return nil
	})
}

func (p *ClusterProvisioner) reconcileAppleEndpoints(ctx context.Context, request ClusterRequest) error {
	controlPlane := appleNodeByRole(&request, "control-plane")
	if controlPlane == nil {
		return errors.New("native topology has no control plane")
	}
	client, err := p.nativeAppleClient(request)
	if err != nil {
		return err
	}
	server := "https://" + net.JoinHostPort(controlPlane.IPv4, "6443")
	proxy, err := client.CoreV1().ConfigMaps("kube-system").Get(ctx, "kube-proxy", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read native kube-proxy endpoint: %w", err)
	}
	original, exists := proxy.Data["kubeconfig.conf"]
	if !exists {
		return errors.New("native kube-proxy has no managed kubeconfig")
	}
	proxyConfiguration, err := clientcmd.Load([]byte(original))
	if err != nil {
		return fmt.Errorf("decode native kube-proxy identity: %w", err)
	}
	authority, err := p.appleReadFile(ctx, controlPlane.ID, "/etc/kubernetes/pki/ca.crt")
	if err != nil || appleDigest(authority) != request.Apple.CAHash {
		return errors.Join(errors.New("native control-plane TLS authority changed"), err)
	}
	for _, cluster := range proxyConfiguration.Clusters {
		if cluster.CertificateAuthority != "" &&
			cluster.CertificateAuthority != "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt" {
			return errors.New("native kube-proxy references an unexpected TLS authority file")
		}
		if len(cluster.CertificateAuthorityData) > 0 && appleDigest(cluster.CertificateAuthorityData) != request.Apple.CAHash {
			return errors.New("native kube-proxy embedded TLS authority changed")
		}
		cluster.CertificateAuthority, cluster.CertificateAuthorityData = "", authority
	}
	proxyData, err := clientcmd.Write(*proxyConfiguration)
	if err != nil {
		return err
	}
	normalized, authority, err := appleClientConfiguration(proxyData, server, controlPlane.ID, false)
	if err != nil || appleDigest(authority) != request.Apple.CAHash {
		return errors.Join(errors.New("native kube-proxy TLS identity does not match the cluster"), err)
	}
	if original != string(normalized) {
		proxy.Data["kubeconfig.conf"] = string(normalized)
		if _, err := client.CoreV1().ConfigMaps("kube-system").Update(ctx, proxy, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update native kube-proxy endpoint: %w", err)
		}
	}
	daemonSet, err := client.AppsV1().DaemonSets("kube-system").Get(ctx, "kube-proxy", metav1.GetOptions{})
	if err != nil {
		return err
	}
	identity := appleDigest([]byte(server + "\n" + controlPlane.ID + "\n" + request.Apple.CAHash))
	if daemonSet.Spec.Template.Annotations["porto.dev/native-api-endpoint"] != identity {
		if daemonSet.Spec.Template.Annotations == nil {
			daemonSet.Spec.Template.Annotations = make(map[string]string)
		}
		daemonSet.Spec.Template.Annotations["porto.dev/native-api-endpoint"] = identity
		if _, err := client.AppsV1().DaemonSets("kube-system").Update(ctx, daemonSet, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("let the Kubernetes controller reconcile kube-proxy's changed endpoint: %w", err)
		}
	}
	discovery, err := client.CoreV1().ConfigMaps("kube-public").Get(ctx, "cluster-info", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read native discovery endpoint: %w", err)
	}
	configuration, err := clientcmd.Load([]byte(discovery.Data["kubeconfig"]))
	if err != nil || len(configuration.Clusters) != 1 {
		return errors.Join(errors.New("native discovery kubeconfig is invalid"), err)
	}
	for _, cluster := range configuration.Clusters {
		if cluster.InsecureSkipTLSVerify || appleDigest(cluster.CertificateAuthorityData) != request.Apple.CAHash {
			return errors.New("native discovery TLS authority changed")
		}
		cluster.Server, cluster.TLSServerName = server, controlPlane.ID
	}
	encoded, err := clientcmd.Write(*configuration)
	if err != nil {
		return err
	}
	if discovery.Data["kubeconfig"] != string(encoded) {
		discovery.Data["kubeconfig"] = string(encoded)
		if _, err := client.CoreV1().ConfigMaps("kube-public").Update(ctx, discovery, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update native discovery endpoint: %w", err)
		}
	}
	bootstrap, err := client.CoreV1().ConfigMaps("kube-system").Get(ctx, "kubeadm-config", metav1.GetOptions{})
	if err != nil {
		return err
	}
	var document map[string]any
	if err := yaml.Unmarshal([]byte(bootstrap.Data["ClusterConfiguration"]), &document); err != nil {
		return err
	}
	endpoint := net.JoinHostPort(controlPlane.IPv4, "6443")
	if document["controlPlaneEndpoint"] != endpoint {
		document["controlPlaneEndpoint"] = endpoint
		data, err := yaml.Marshal(document)
		if err != nil {
			return err
		}
		bootstrap.Data["ClusterConfiguration"] = string(data)
		if _, err := client.CoreV1().ConfigMaps("kube-system").Update(ctx, bootstrap, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update native bootstrap endpoint without changing membership: %w", err)
		}
	}
	return nil
}

func (p *ClusterProvisioner) startAppleNative(ctx context.Context, request *ClusterRequest) (err error) {
	system, err := p.appleSystem(ctx, true)
	if err != nil {
		return err
	}
	if strings.TrimRight(system.Paths.AppRoot, "/") != strings.TrimRight(request.Apple.AppRoot, "/") {
		return errors.New("native runtime state root changed; recorded node storage will not be migrated or rebuilt")
	}
	containers, err := p.ownedAppleNodes(ctx, *request)
	if err != nil {
		return err
	}
	if operation := request.Apple.Operation; operation != nil && operation.Goal == "stopped" && operation.CompletedAt == "" {
		for _, node := range request.Apple.Nodes {
			if operation.Nodes[node.ID] == "shutdown-requested" {
				if err := p.stopAppleNode(ctx, *request, node); err != nil {
					return fmt.Errorf("finish the previously requested graceful shutdown before starting recovery: %w", err)
				}
			}
		}
	}
	unexpectedStop := request.Apple.Operation == nil ||
		request.Apple.Operation.CompletedAt != "" && request.Apple.Operation.Goal != "stopped"
	anyStopped := false
	for _, container := range containers {
		anyStopped = anyStopped || container.Status.State == "stopped"
	}
	if err := p.beginAppleLifecycle(request, "running"); err != nil {
		return err
	}
	if unexpectedStop && anyStopped {
		request.Apple.Operation.BaselineUnknown = true
	}
	defer func() { err = p.finishAppleLifecycle(request, err) }()
	recovery, cancel := context.WithTimeout(ctx, appleRecovery)
	defer cancel()
	controlPlane := appleNodeByRole(request, "control-plane")
	if controlPlane == nil {
		return errors.New("native topology has no owned control plane")
	}
	if err := p.appleProgress(request, "starting-control-plane", controlPlane.ID, "starting"); err != nil {
		return err
	}
	if err := p.startAppleNode(recovery, request, controlPlane); err != nil {
		return err
	}
	changed, err := p.healAppleControlPlane(recovery, request, controlPlane)
	if err != nil {
		return err
	}
	if err := p.releaseAppleKubelet(recovery, controlPlane.ID, changed); err != nil {
		return err
	}
	if err := p.waitAppleDatastoreAndAPI(recovery, controlPlane.ID); err != nil {
		return err
	}
	if err := p.refreshAppleKubeconfig(recovery, request); err != nil {
		return err
	}
	if err := p.appleProgress(request, "control-plane-ready", controlPlane.ID, "api-ready"); err != nil {
		return err
	}
	if request.Apple.Operation.BaselineUnknown {
		current, err := p.captureAppleHealth(recovery, *request)
		if err != nil {
			return fmt.Errorf("recover current workload declarations after an unexpected runtime stop: %w", err)
		}
		known := make(map[string]AppleWorkloadHealth)
		if request.Apple.Health != nil {
			for _, workload := range request.Apple.Health.Workloads {
				known[workload.UID] = workload
			}
		}
		for index := range current.Workloads {
			workload := &current.Workloads[index]
			prior, exists := known[workload.UID]
			if !workload.Ignored && (!exists || prior.Healthy) {
				workload.Healthy = true
			}
		}
		request.Apple.Health = current
		appleAppendWarning(request.Apple, "The runtime stopped without a completed Porto Stop. Recovery is checked against current workload declarations; pre-reboot health was not completely observable.")
		if err := p.writeClusterMetadata(*request); err != nil {
			return err
		}
	}
	if err := p.reconcileAppleEndpoints(recovery, *request); err != nil {
		return err
	}
	for index := range request.Apple.Nodes {
		node := &request.Apple.Nodes[index]
		if node.Role != "worker" {
			continue
		}
		if err := p.appleProgress(request, "starting-workers", node.ID, "starting"); err != nil {
			return err
		}
		if err := p.startAppleNode(recovery, request, node); err != nil {
			return err
		}
		changed, err := p.pointAppleGuestClient(recovery, node.ID, "/etc/kubernetes/kubelet.conf",
			"https://"+net.JoinHostPort(controlPlane.IPv4, "6443"), controlPlane.ID)
		if err != nil {
			return err
		}
		addressChanged, err := p.pointAppleKubelet(recovery, *node)
		if err != nil {
			return err
		}
		if err := p.releaseAppleKubelet(recovery, node.ID, changed || addressChanged); err != nil {
			return err
		}
		if err := p.appleProgress(request, "starting-workers", node.ID, "kubelet-ready"); err != nil {
			return err
		}
	}
	if err := p.appleProgress(request, "verifying-kubernetes-recovery", "", ""); err != nil {
		return err
	}
	if err := waitApple(recovery, "verify current native nodes, CNI and DNS", func(probe context.Context) error {
		if err := p.checkAppleNodes(probe, request, false); err != nil {
			return err
		}
		return p.checkAppleSystemPods(probe, *request)
	}); err != nil {
		return err
	}
	if err := waitApple(recovery, "recover previously healthy native workloads", func(probe context.Context) error {
		return p.checkAppleWorkloads(probe, *request, request.Apple.Health)
	}); err != nil {
		return err
	}
	if request.Apple.Health != nil {
		for _, service := range request.Apple.Health.Probes {
			if err := waitApple(recovery, "recover native Service "+service.Namespace+"/"+service.Name,
				func(probe context.Context) error { return p.probeAppleService(probe, *request, service) }); err != nil {
				return err
			}
		}
	}
	current, err := p.captureAppleHealth(recovery, *request)
	if err != nil {
		return err
	}
	var historical []string
	if request.Apple.Operation.BaselineUnknown {
		historical = append(historical, "The runtime stopped without a completed Porto Stop; current declared workload recovery was verified, but complete pre-reboot health history was unavailable.")
	}
	request.Apple.Warnings = append(historical, current.Failures...)
	request.Apple.Health = current
	request.RunningAt = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := p.registerKubeconfig(recovery, request.Name); err != nil {
		warning, fatal := kubeconfigRegistrationResult(request.Name, err)
		if fatal != nil {
			return fatal
		}
		appleAppendWarning(request.Apple, warning)
	}
	return nil
}

func (p *ClusterProvisioner) observeAppleNative(ctx context.Context, request ClusterRequest, cluster *Cluster) {
	cluster.Capabilities = appleCapabilities(request)
	cluster.Lifecycle = request.Apple.Operation
	cluster.Nodes = appleNativeNodeIDs(request)
	cluster.Server = "https://127.0.0.1:" + strconv.Itoa(request.APIPort)
	containers, err := p.ownedAppleNodes(ctx, request)
	if err != nil {
		cluster.State = "unavailable"
		cluster.Message = err.Error()
		return
	}
	running, stopped := 0, 0
	for _, container := range containers {
		if container.Status.State == "running" {
			running++
		}
		cluster.Capabilities.ImageLoading = running == len(containers)
		if request.Apple.Operation != nil && request.Apple.Operation.CompletedAt == "" {
			cluster.StateSince = request.Apple.Operation.StartedAt
		}
		if container.Status.State == "stopped" {
			stopped++
		}
	}
	if request.Phase != "" {
		cluster.State = request.Phase
		if (request.Phase == "starting" || request.Phase == "stopping") &&
			!p.runtimeNameReserved(clusterRuntimeName(request), request.Name) {
			active, err := p.appleOperationActive(request.Apple.ID)
			if err != nil {
				cluster.State, cluster.Message = "unavailable", err.Error()
				return
			}
			if !active {
				cluster.State = "degraded"
				cluster.Message = "Native lifecycle was interrupted. Retry Start or Stop; existing node filesystems and progress are retained."
			}
		}
		return
	}
	switch {
	case stopped == len(containers):
		cluster.State = "stopped"
	case running == len(containers):
		cluster.State = "running"
		cluster.Message = appleLifecycleMessage(request.Apple)
		if cluster.Message != "" {
			cluster.State = "degraded"
		}
	default:
		cluster.State = "degraded"
		cluster.Message = "Only part of this native cluster is running. Retry Start or Stop; Porto will not recreate missing resources."
	}
}
