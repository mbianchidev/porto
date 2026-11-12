package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

var appleBootstrapToken = regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)

func (p *ClusterProvisioner) createAppleManaged(ctx context.Context, request *ClusterRequest) (Cluster, error) {
	ctx, cancel := provisioningContext(ctx, "create native Apple cluster", ClusterCreationTimeout)
	defer cancel()
	system, err := p.appleSystem(ctx, true)
	if err != nil {
		return Cluster{}, err
	}
	identity, err := appleIdentity()
	if err != nil {
		return Cluster{}, err
	}
	request.Apple = &AppleClusterState{Format: appleNativeFormat, ID: identity,
		AppRoot: system.Paths.AppRoot, RuntimeVersion: system.Server.Version}
	var releasePort func()
	request.APIPort, releasePort, err = p.reserveAPIPort(0)
	if err != nil {
		return Cluster{}, err
	}
	defer releasePort()
	add := func(id, role, group string, machine MachineSpec) error {
		token, err := appleIdentity()
		if err != nil {
			return err
		}
		request.Apple.Nodes = append(request.Apple.Nodes, AppleNode{
			ID: id, Identity: token, Role: role, Group: group, Machine: machine,
		})
		return nil
	}
	nativeName := appleClusterName(*request)
	if err := add(nativeName, "control-plane", "", request.ControlPlane); err != nil {
		return Cluster{}, err
	}
	for _, group := range request.NodeGroups {
		for range group.Count {
			if err := add(nativeName+"-worker-"+strconv.Itoa(len(request.Apple.Nodes)), "worker", group.Name, group.Machine); err != nil {
				return Cluster{}, err
			}
		}
	}
	if err := p.writeClusterMetadata(*request); err != nil {
		return Cluster{}, err
	}
	image := appleNodeImage
	if request.Version != "" {
		image = "docker.io/kindest/node:" + request.Version
	}
	for index := range request.Apple.Nodes {
		node := &request.Apple.Nodes[index]
		args := []string{"create", "--name", node.ID,
			"--label", appleClusterLabel + "=" + request.Apple.ID,
			"--label", appleNodeLabel + "=" + node.Identity,
			"--label", "dev.porto.role=" + node.Role,
			"--platform", "linux/arm64", "--cpus", strconv.Itoa(node.Machine.CPUs),
			"--memory", strconv.Itoa(node.Machine.MemoryMiB) + "m",
			"--cap-add", "ALL", "--masked-path", "NONE", "--read-only-path", "NONE",
			"--entrypoint", "/bin/sh"}
		if node.Role == "control-plane" {
			args = append(args, "--publish", "127.0.0.1:"+strconv.Itoa(request.APIPort)+":6443")
		}
		args = append(args, image, "-c", appleNodeBootstrap)
		output, err := p.runApple(ctx, runtimes.Command{Args: args, Network: true})
		if err != nil {
			return Cluster{}, runtimes.CommandError("create exactly owned native node "+node.ID, output, err)
		}
		if err := p.captureCreatedAppleNode(ctx, request, node); err != nil {
			return Cluster{}, err
		}
		output, err = p.runApple(ctx, runtimes.Command{Args: []string{"start", node.ID}})
		if err != nil {
			return Cluster{}, runtimes.CommandError("boot new native node "+node.ID, output, err)
		}
		node.Storage, err = p.captureAppleStorage(request.Apple.AppRoot, node.ID)
		if err != nil {
			return Cluster{}, fmt.Errorf("record first-boot native backing storage: %w", err)
		}
		if err := p.writeClusterMetadata(*request); err != nil {
			return Cluster{}, err
		}
		if err := waitApple(ctx, "boot native node "+node.ID, func(probe context.Context) error {
			_, err := p.appleGuest(probe, node.ID, 10*time.Second, nil, "/bin/systemctl", "is-active", "--quiet", "containerd")
			return err
		}); err != nil {
			return Cluster{}, err
		}
		if _, err := p.appleGuest(ctx, node.ID, 10*time.Minute, []byte(appleNodePreparation), "/bin/sh", "-s"); err != nil {
			return Cluster{}, fmt.Errorf("prepare signed guest packages for graceful native shutdown: %w", err)
		}
		if err := p.waitAppleGuest(ctx, node); err != nil {
			return Cluster{}, err
		}
		current, err := p.inspectAppleNodes(ctx, []string{node.ID})
		if err != nil {
			return Cluster{}, err
		}
		node.IPv4, err = appleNodeIPv4(current[node.ID])
		if err != nil {
			return Cluster{}, err
		}
		if err := p.prepareAppleNetwork(ctx, node.ID); err != nil {
			return Cluster{}, err
		}
		if err := p.writeClusterMetadata(*request); err != nil {
			return Cluster{}, err
		}
	}
	controlPlane := appleNodeByRole(request, "control-plane")
	output, err := p.appleGuest(ctx, controlPlane.ID, 15*time.Second, nil, "/usr/bin/kubeadm", "version", "-o", "short")
	if err != nil {
		return Cluster{}, err
	}
	version := strings.TrimSpace(string(output))
	if _, err := parseUpgradeVersion("kind", version); err != nil {
		return Cluster{}, err
	}
	if request.Version != "" && request.Version != version {
		return Cluster{}, errors.New("native node image's Kubernetes version differs from the requested release")
	}
	request.Version = version
	for _, node := range request.Apple.Nodes {
		if err := p.prepareApplePauseImage(ctx, node.ID, version); err != nil {
			return Cluster{}, err
		}
	}
	configuration, err := appleInitConfiguration(*controlPlane, version)
	if err != nil {
		return Cluster{}, err
	}
	if err := p.appleWriteFile(ctx, controlPlane.ID, "/etc/porto/kubeadm.yaml", configuration); err != nil {
		return Cluster{}, err
	}
	if err := p.releaseAppleKubelet(ctx, controlPlane.ID, false); err != nil {
		return Cluster{}, err
	}
	if _, err := p.appleGuest(ctx, controlPlane.ID, 10*time.Minute, nil, "/usr/bin/kubeadm", "init",
		"--config", "/etc/porto/kubeadm.yaml",
		"--ignore-preflight-errors=Swap,SystemVerification,FileContent--proc-sys-net-bridge-bridge-nf-call-iptables"); err != nil {
		return Cluster{}, err
	}
	if err := p.refreshAppleKubeconfig(ctx, request); err != nil {
		return Cluster{}, err
	}
	changed, err := p.healAppleControlPlane(ctx, request, controlPlane)
	if err != nil {
		return Cluster{}, err
	}
	if err := p.releaseAppleKubelet(ctx, controlPlane.ID, changed); err != nil {
		return Cluster{}, err
	}
	if err := p.waitAppleDatastoreAndAPI(ctx, controlPlane.ID); err != nil {
		return Cluster{}, err
	}
	if err := p.joinAppleWorkers(ctx, request); err != nil {
		return Cluster{}, err
	}
	cniPath := filepath.Join(system.Paths.InstallRoot, "libexec", "container-plugins", "cluster", "resources", "kindnet.yaml")
	cni, err := os.ReadFile(cniPath)
	if err != nil {
		return Cluster{}, fmt.Errorf("read installed native kindnet manifest: %w", err)
	}
	run := p.addonCommandRunner(ctx, p.clusterKubeconfigPath(request.Name), clusterContextName(*request))
	if _, err := run("install native CNI", cni, "apply", "-f", "-"); err != nil {
		return Cluster{}, err
	}
	client, err := p.nativeAppleClient(*request)
	if err != nil {
		return Cluster{}, err
	}
	if len(request.Apple.Nodes) == 1 {
		node, err := client.CoreV1().Nodes().Get(ctx, controlPlane.ID, metav1.GetOptions{})
		if err != nil {
			return Cluster{}, err
		}
		for index := len(node.Spec.Taints) - 1; index >= 0; index-- {
			if node.Spec.Taints[index].Key == "node-role.kubernetes.io/control-plane" {
				node.Spec.Taints = append(node.Spec.Taints[:index], node.Spec.Taints[index+1:]...)
			}
		}
		if _, err := client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
			return Cluster{}, err
		}
	}
	if err := p.reconcileAppleEndpoints(ctx, *request); err != nil {
		return Cluster{}, err
	}
	if err := waitApple(ctx, "verify new native nodes and system networking", func(probe context.Context) error {
		if err := p.checkAppleNodes(probe, request, true); err != nil {
			return err
		}
		return p.checkAppleSystemPods(probe, *request)
	}); err != nil {
		return Cluster{}, err
	}
	for _, group := range request.NodeGroups {
		for _, node := range request.Apple.Nodes {
			if node.Group != group.Name {
				continue
			}
			for key, value := range group.Labels {
				if _, err := run("label native worker", nil, "label", "node", node.ID, key+"="+value, "--overwrite=true"); err != nil {
					return Cluster{}, err
				}
			}
			if len(group.Taints) > 0 {
				if _, err := run("taint native worker", nil, append([]string{"taint", "node", node.ID, "--overwrite=true"}, group.Taints...)...); err != nil {
					return Cluster{}, err
				}
			}
		}
	}
	if err := p.ensureKindMetricsServer(ctx, p.clusterKubeconfigPath(request.Name), clusterContextName(*request)); err != nil {
		return Cluster{}, err
	}
	if err := p.ensureClusterAddons(ctx, p.clusterKubeconfigPath(request.Name), clusterContextName(*request), "apple"); err != nil {
		return Cluster{}, err
	}
	request.Apple.Health, err = p.captureAppleHealth(ctx, *request)
	if err != nil {
		return Cluster{}, err
	}
	request.Phase, request.Error = "", ""
	request.RunningAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := p.writeClusterMetadata(*request); err != nil {
		return Cluster{}, err
	}
	cluster := Cluster{Name: request.Name, Provider: "apple", State: "running",
		Context: clusterContextName(*request), KubeconfigPath: p.clusterKubeconfigPath(request.Name),
		Server: "https://127.0.0.1:" + strconv.Itoa(request.APIPort), Nodes: appleNativeNodeIDs(*request),
		Version: request.Version, Capabilities: appleCapabilities(*request)}
	if _, err := p.registerKubeconfig(ctx, request.Name); err != nil {
		warning, fatal := kubeconfigRegistrationResult(request.Name, err)
		if fatal != nil {
			return Cluster{}, fatal
		}
		cluster.Message = warning
	}
	return cluster, nil
}

func (p *ClusterProvisioner) captureCreatedAppleNode(ctx context.Context, request *ClusterRequest, node *AppleNode) error {
	containers, err := p.inspectAppleNodes(ctx, []string{node.ID})
	if err != nil {
		return err
	}
	actual := containers[node.ID]
	var configuration appleNativeConfiguration
	if err := json.Unmarshal(actual.Configuration, &configuration); err != nil {
		return err
	}
	if configuration.ID != node.ID || configuration.Labels[appleClusterLabel] != request.Apple.ID ||
		configuration.Labels[appleNodeLabel] != node.Identity {
		return errors.New("new native node ownership does not match the recorded creation intent")
	}
	node.CreatedAt = configuration.CreationDate
	node.Configuration, err = canonicalAppleJSON(actual.Configuration)
	if err != nil {
		return err
	}
	directory := filepath.Join(request.Apple.AppRoot, "containers", node.ID)
	directoryID, err := p.appleFileIdentity(directory)
	if err != nil {
		return err
	}
	node.Storage = AppleNodeStorage{Directory: directory, DirectoryID: directoryID, Rootfs: filepath.Join(directory, "rootfs.ext4")}
	return p.writeClusterMetadata(*request)
}

func (p *ClusterProvisioner) prepareApplePauseImage(ctx context.Context, node, version string) error {
	output, err := p.appleGuest(ctx, node, time.Minute, nil, "/usr/bin/kubeadm", "config", "images", "list", "--kubernetes-version", version)
	if err != nil {
		return err
	}
	pause := ""
	for _, image := range strings.Fields(string(output)) {
		if strings.HasPrefix(image, "registry.k8s.io/pause:") {
			pause = image
		}
	}
	if pause == "" || strings.ContainsAny(pause, "\"\r\n") {
		return errors.New("native node image did not identify its reviewed pause image")
	}
	configuration := "[plugins.'io.containerd.cri.v1.images'.pinned_images]\nsandbox = \"" + pause + "\"\n"
	if err := p.appleWriteFile(ctx, node, "/etc/containerd/conf.d/porto-sandbox.toml", []byte(configuration)); err != nil {
		return err
	}
	const script = `set -eu
test ! -f /var/lib/kubelet/config.yaml
systemctl restart containerd.service`
	_, err = p.appleGuest(ctx, node, time.Minute, nil, "/bin/sh", "-c", script)
	return err
}

func (p *ClusterProvisioner) joinAppleWorkers(ctx context.Context, request *ClusterRequest) (err error) {
	if len(request.Apple.Nodes) == 1 {
		return nil
	}
	controlPlane := appleNodeByRole(request, "control-plane")
	authority, err := p.appleReadFile(ctx, controlPlane.ID, "/etc/kubernetes/pki/ca.crt")
	if err != nil {
		return err
	}
	discoveryHash, err := appleAuthorityDiscoveryHash(authority)
	if err != nil {
		return err
	}
	output, err := p.appleGuest(ctx, controlPlane.ID, time.Minute, nil, "/usr/bin/kubeadm", "token", "create", "--ttl", "10m")
	if err != nil {
		return err
	}
	token := strings.TrimSpace(string(output))
	if !appleBootstrapToken.MatchString(token) {
		return errors.New("native bootstrap returned an invalid token; its value is withheld")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, cleanupErr := p.appleGuest(cleanup, controlPlane.ID, time.Minute, nil, "/usr/bin/kubeadm", "token", "delete", token[:6])
		err = errors.Join(err, cleanupErr)
	}()
	for index := range request.Apple.Nodes {
		node := &request.Apple.Nodes[index]
		if node.Role != "worker" {
			continue
		}
		document := map[string]any{
			"apiVersion": "kubeadm.k8s.io/v1beta4", "kind": "JoinConfiguration",
			"nodeRegistration": appleNodeRegistration(*node),
			"discovery": map[string]any{"bootstrapToken": map[string]any{
				"apiServerEndpoint": net.JoinHostPort(controlPlane.IPv4, "6443"),
				"token":             token, "caCertHashes": []string{discoveryHash},
			}},
		}
		data, err := yaml.Marshal(document)
		if err != nil {
			return err
		}
		if err := p.appleWriteFile(ctx, node.ID, "/etc/porto/bootstrap-join.yaml", data); err != nil {
			return err
		}
		if err := p.releaseAppleKubelet(ctx, node.ID, false); err != nil {
			return err
		}
		if _, err := p.appleGuest(ctx, node.ID, 8*time.Minute, nil, "/usr/bin/kubeadm", "join",
			"--config", "/etc/porto/bootstrap-join.yaml",
			"--ignore-preflight-errors=Swap,SystemVerification,FileContent--proc-sys-net-bridge-bridge-nf-call-iptables"); err != nil {
			return err
		}
		if _, err := p.appleGuest(ctx, node.ID, 15*time.Second, nil, "/bin/rm", "-f", "/etc/porto/bootstrap-join.yaml"); err != nil {
			return err
		}
		if _, err := p.pointAppleGuestClient(ctx, node.ID, "/etc/kubernetes/kubelet.conf",
			"https://"+net.JoinHostPort(controlPlane.IPv4, "6443"), controlPlane.ID); err != nil {
			return err
		}
	}
	return nil
}
