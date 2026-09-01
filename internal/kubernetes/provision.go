package kubernetes

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

var clusterNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9.+-]+$`)

//go:embed metrics-server.yaml
var metricsServerManifest []byte

type ClusterProvisioner struct {
	vms            *vm.Manager
	runner         runtimes.Runner
	kubeconfigRoot string
	metricsMu      sync.Mutex
	metricsRuns    map[string]*metricsServerRun
}

type metricsServerRun struct {
	done chan struct{}
	err  error
}

type MachineSpec struct {
	CPUs      int `json:"cpus"`
	MemoryMiB int `json:"memoryMiB"`
	DiskGiB   int `json:"diskGiB"`
}

type NodeGroupSpec struct {
	Name    string            `json:"name"`
	Count   int               `json:"count"`
	Machine MachineSpec       `json:"machine"`
	Labels  map[string]string `json:"labels"`
	Taints  []string          `json:"taints"`
}

type ClusterRequest struct {
	Name         string          `json:"name"`
	Provider     string          `json:"provider"`
	Version      string          `json:"version"`
	APIPort      int             `json:"apiPort,omitempty"`
	ControlPlane MachineSpec     `json:"controlPlane"`
	NodeGroups   []NodeGroupSpec `json:"nodeGroups"`
}

type Cluster struct {
	Name           string   `json:"name"`
	Provider       string   `json:"provider"`
	State          string   `json:"state"`
	Context        string   `json:"context"`
	KubeconfigPath string   `json:"kubeconfigPath"`
	Server         string   `json:"server"`
	Nodes          []string `json:"nodes"`
}

func NewClusterProvisioner(vms *vm.Manager, runner runtimes.Runner, kubeconfigRoot string) *ClusterProvisioner {
	if vms == nil {
		vms = vm.New(runner)
	}
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	return &ClusterProvisioner{
		vms:            vms,
		runner:         runner,
		kubeconfigRoot: kubeconfigRoot,
		metricsRuns:    make(map[string]*metricsServerRun),
	}
}

func (p *ClusterProvisioner) Create(ctx context.Context, request ClusterRequest) (cluster Cluster, err error) {
	if !clusterNamePattern.MatchString(request.Name) {
		return Cluster{}, fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	request.Provider = normalizeProvider(request.Provider)
	if request.Provider == "" {
		return Cluster{}, errors.New("Kubernetes provider must be kind, k0s, or k3s")
	}
	if request.Version != "" && !versionPattern.MatchString(request.Version) {
		return Cluster{}, fmt.Errorf("invalid Kubernetes version %q", request.Version)
	}
	kubeconfigPath := p.clusterKubeconfigPath(request.Name)
	for _, existingPath := range []string{kubeconfigPath, p.clusterMetadataPath(request.Name)} {
		if _, statErr := os.Stat(existingPath); statErr == nil {
			return Cluster{}, fmt.Errorf("Kubernetes cluster %s already exists", request.Name)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return Cluster{}, fmt.Errorf("inspect Kubernetes cluster state: %w", statErr)
		}
	}
	request.ControlPlane = normalizeMachine(request.ControlPlane)
	for index := range request.NodeGroups {
		group := &request.NodeGroups[index]
		if !clusterNamePattern.MatchString(group.Name) {
			return Cluster{}, fmt.Errorf("node group name must match %s", clusterNamePattern)
		}
		if group.Count < 0 || group.Count > 32 {
			return Cluster{}, fmt.Errorf("node group %s count must be between 0 and 32", group.Name)
		}
		group.Machine = normalizeMachine(group.Machine)
		if err := validateNodeMetadata(group.Labels, group.Taints); err != nil {
			return Cluster{}, fmt.Errorf("node group %s: %w", group.Name, err)
		}
	}
	if request.Provider == "kind" {
		return p.createKind(ctx, request)
	}
	if request.APIPort == 0 {
		request.APIPort, err = availableLocalPort()
		if err != nil {
			return Cluster{}, err
		}
	}

	created := make([]string, 0)
	defer func() {
		if err == nil {
			return
		}
		var cleanupErrors []error
		for index := len(created) - 1; index >= 0; index-- {
			if deleteErr := p.vms.Delete(context.Background(), created[index], true); deleteErr != nil {
				cleanupErrors = append(cleanupErrors, deleteErr)
			}
		}
		for _, cleanupPath := range []string{kubeconfigPath, p.clusterMetadataPath(request.Name)} {
			if removeErr := os.Remove(cleanupPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				cleanupErrors = append(cleanupErrors, removeErr)
			}
		}
		err = errors.Join(err, errors.Join(cleanupErrors...))
	}()

	serverName := clusterMachineName(request.Name, "server", 1)
	if _, err = p.vms.CreateNode(ctx, vm.CreateRequest{
		Name:      serverName,
		Image:     "ubuntu-24.04",
		CPUs:      request.ControlPlane.CPUs,
		MemoryMiB: request.ControlPlane.MemoryMiB,
		DiskGiB:   request.ControlPlane.DiskGiB,
		Network:   "user-v2",
		PortForwards: []vm.PortForward{{
			GuestPort: 6443,
			HostPort:  request.APIPort,
		}},
		Start: true,
	}); err != nil {
		return Cluster{}, fmt.Errorf("create Kubernetes control-plane VM: %w", err)
	}
	created = append(created, serverName)
	if err = p.installVMKubernetes(ctx, request.Provider, serverName, request.Version, true, "", "", nil, nil); err != nil {
		return Cluster{}, err
	}
	serverIPOutput, err := p.vms.Exec(ctx, serverName, []string{"hostname", "-I"}, nil)
	if err != nil {
		return Cluster{}, fmt.Errorf("resolve Kubernetes server address: %w", err)
	}
	serverIP := firstField(serverIPOutput)
	if serverIP == "" {
		return Cluster{}, errors.New("Kubernetes server VM reported no IP address")
	}
	tokenOutput, err := p.clusterToken(ctx, request.Provider, serverName)
	if err != nil {
		return Cluster{}, fmt.Errorf("read Kubernetes node token: %w", err)
	}
	token := strings.TrimSpace(string(tokenOutput))
	if token == "" {
		return Cluster{}, errors.New("Kubernetes server returned an empty node token")
	}

	nodes := []string{serverName}
	for _, group := range request.NodeGroups {
		for nodeIndex := 1; nodeIndex <= group.Count; nodeIndex++ {
			nodeName := clusterMachineName(request.Name, group.Name, nodeIndex)
			if _, err = p.vms.CreateNode(ctx, vm.CreateRequest{
				Name:      nodeName,
				Image:     "ubuntu-24.04",
				CPUs:      group.Machine.CPUs,
				MemoryMiB: group.Machine.MemoryMiB,
				DiskGiB:   group.Machine.DiskGiB,
				Network:   "user-v2",
				Start:     true,
			}); err != nil {
				return Cluster{}, fmt.Errorf("create Kubernetes worker VM %s: %w", nodeName, err)
			}
			created = append(created, nodeName)
			if err = p.installVMKubernetes(ctx, request.Provider, nodeName, request.Version, false, serverIP, token, group.Labels, group.Taints); err != nil {
				return Cluster{}, err
			}
			nodes = append(nodes, nodeName)
		}
	}

	kubeconfigOutput, err := p.clusterKubeconfig(ctx, request.Provider, serverName)
	if err != nil {
		return Cluster{}, fmt.Errorf("read Kubernetes kubeconfig: %w", err)
	}
	contextName := "porto-" + request.Name
	kubeconfig := strings.ReplaceAll(
		string(kubeconfigOutput),
		"https://127.0.0.1:6443",
		"https://127.0.0.1:"+strconv.Itoa(request.APIPort),
	)
	kubeconfig = strings.ReplaceAll(
		kubeconfig,
		"https://"+serverIP+":6443",
		"https://127.0.0.1:"+strconv.Itoa(request.APIPort),
	)
	if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o700); err != nil {
		return Cluster{}, fmt.Errorf("create kubeconfig directory: %w", err)
	}
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0o600); err != nil {
		return Cluster{}, fmt.Errorf("write kubeconfig: %w", err)
	}
	if err := p.normalizeKubeconfig(ctx, kubeconfigPath, contextName); err != nil {
		return Cluster{}, err
	}
	cluster = Cluster{
		Name:           request.Name,
		Provider:       request.Provider,
		State:          "running",
		Context:        contextName,
		KubeconfigPath: kubeconfigPath,
		Server:         "https://127.0.0.1:" + strconv.Itoa(request.APIPort),
		Nodes:          nodes,
	}
	if err = p.writeClusterMetadata(request); err != nil {
		return Cluster{}, err
	}
	return cluster, nil
}

func (p *ClusterProvisioner) createKind(ctx context.Context, request ClusterRequest) (cluster Cluster, err error) {
	kubeconfigPath := p.clusterKubeconfigPath(request.Name)
	kindName := "porto-" + request.Name
	configFile, err := writeKindConfig(request)
	if err != nil {
		return Cluster{}, err
	}
	defer os.Remove(configFile)
	defer func() {
		if err == nil {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		output, cleanupErr := p.runner.Run(cleanupContext, runtimes.Command{
			Name: "kind",
			Args: []string{"delete", "cluster", "--name", kindName},
			Env:  portoDockerEnv(),
		})
		if cleanupErr != nil {
			err = errors.Join(err, runtimes.CommandError("clean up kind cluster", output, cleanupErr))
		}
		for _, cleanupPath := range []string{kubeconfigPath, p.clusterMetadataPath(request.Name)} {
			if removeErr := os.Remove(cleanupPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()
	if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o700); err != nil {
		return Cluster{}, fmt.Errorf("create kubeconfig directory: %w", err)
	}
	args := []string{
		"create", "cluster",
		"--name", kindName,
		"--config", configFile,
		"--kubeconfig", kubeconfigPath,
		"--wait", "5m",
	}
	if request.Version != "" {
		args = append(args, "--image", "kindest/node:"+request.Version)
	}
	commandContext, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	output, err := p.runner.Run(commandContext, runtimes.Command{Name: "kind", Args: args, Env: portoDockerEnv()})
	if err != nil {
		return Cluster{}, runtimes.CommandError("create kind cluster", output, err)
	}
	contextName := "porto-" + request.Name
	if err := p.normalizeKubeconfig(ctx, kubeconfigPath, contextName); err != nil {
		return Cluster{}, err
	}
	nodes := kindNodeNames(request)
	for index, node := range nodes {
		machine := request.ControlPlane
		if index > 0 {
			machine = kindWorkerMachine(request, index-1)
		}
		output, err = p.runner.Run(ctx, runtimes.Command{
			Name: "docker",
			Args: []string{
				"update",
				"--cpus", strconv.Itoa(machine.CPUs),
				"--memory", strconv.Itoa(machine.MemoryMiB) + "m",
				"--memory-swap", strconv.Itoa(machine.MemoryMiB) + "m",
				node,
			},
			Env: portoDockerEnv(),
		})
		if err != nil {
			return Cluster{}, runtimes.CommandError("apply kind node resources", output, err)
		}
	}
	if err := p.ensureKindMetricsServer(ctx, kubeconfigPath, contextName); err != nil {
		return Cluster{}, err
	}
	request.APIPort = 0
	if err := p.writeClusterMetadata(request); err != nil {
		return Cluster{}, err
	}
	serverOutput, err := p.runner.Run(ctx, runtimes.Command{
		Name: "kubectl",
		Args: []string{
			"--kubeconfig", kubeconfigPath,
			"--context", contextName,
			"config", "view", "--minify",
			"-o", "jsonpath={.clusters[0].cluster.server}",
		},
	})
	if err != nil {
		return Cluster{}, runtimes.CommandError("read kind API endpoint", serverOutput, err)
	}
	return Cluster{
		Name:           request.Name,
		Provider:       "kind",
		State:          "running",
		Context:        contextName,
		KubeconfigPath: kubeconfigPath,
		Server:         strings.TrimSpace(string(serverOutput)),
		Nodes:          nodes,
	}, nil
}

func writeKindConfig(request ClusterRequest) (string, error) {
	var builder strings.Builder
	builder.WriteString("kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\nnodes:\n")
	builder.WriteString("  - role: control-plane\n")
	for _, group := range request.NodeGroups {
		for range group.Count {
			builder.WriteString("  - role: worker\n")
		}
	}
	file, err := os.CreateTemp("", "porto-kind-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create kind configuration: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if _, err := file.WriteString(builder.String()); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write kind configuration: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func kindNodeNames(request ClusterRequest) []string {
	kindName := "porto-" + request.Name
	names := []string{kindName + "-control-plane"}
	workers := 0
	for _, group := range request.NodeGroups {
		workers += group.Count
	}
	for index := 1; index <= workers; index++ {
		name := kindName + "-worker"
		if index > 1 {
			name += strconv.Itoa(index)
		}
		names = append(names, name)
	}
	return names
}

func runningKindNodeNames(request ClusterRequest, output []byte) []string {
	expected := make(map[string]struct{})
	for _, name := range kindNodeNames(request) {
		expected[name] = struct{}{}
	}
	names := make([]string, 0, len(expected))
	for _, name := range strings.Fields(string(output)) {
		if _, ok := expected[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

func kindWorkerMachine(request ClusterRequest, workerIndex int) MachineSpec {
	offset := 0
	for _, group := range request.NodeGroups {
		if workerIndex < offset+group.Count {
			return group.Machine
		}
		offset += group.Count
	}
	return MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}
}

func (p *ClusterProvisioner) Delete(ctx context.Context, clusterName string) error {
	if !clusterNamePattern.MatchString(clusterName) {
		return fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	request, err := p.readClusterMetadata(clusterName)
	if err != nil {
		return fmt.Errorf("read Kubernetes cluster ownership: %w", err)
	}
	request.Provider = normalizeProvider(request.Provider)
	var deleteErrors []error
	if request.Provider == "kind" {
		output, deleteErr := p.runner.Run(ctx, runtimes.Command{
			Name: "kind",
			Args: []string{"delete", "cluster", "--name", "porto-" + clusterName},
			Env:  portoDockerEnv(),
		})
		if deleteErr != nil {
			deleteErrors = append(deleteErrors, runtimes.CommandError("delete kind cluster", output, deleteErr))
		}
	} else {
		names := clusterNodeNames(request)
		for index := len(names) - 1; index >= 0; index-- {
			if err := p.vms.Delete(ctx, names[index], true); err != nil {
				deleteErrors = append(deleteErrors, err)
			}
		}
	}
	kubeconfigPath := p.clusterKubeconfigPath(clusterName)
	if err := os.Remove(kubeconfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		deleteErrors = append(deleteErrors, err)
	}
	if err := os.Remove(p.clusterMetadataPath(clusterName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		deleteErrors = append(deleteErrors, err)
	}
	return errors.Join(deleteErrors...)
}

func (p *ClusterProvisioner) List(ctx context.Context) ([]Cluster, error) {
	entries, err := os.ReadDir(p.kubeconfigRoot)
	if errors.Is(err, os.ErrNotExist) {
		return []Cluster{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list Porto kubeconfigs: %w", err)
	}
	instances, instanceErr := p.vms.ListAll(ctx)
	instanceByName := make(map[string]vm.Instance, len(instances))
	for _, instance := range instances {
		instanceByName[instance.Name] = instance
	}
	clusters := make([]Cluster, 0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(p.kubeconfigRoot, entry.Name()))
		if readErr != nil {
			return nil, fmt.Errorf("read Kubernetes cluster metadata %s: %w", entry.Name(), readErr)
		}
		var request ClusterRequest
		if decodeErr := json.Unmarshal(data, &request); decodeErr != nil {
			return nil, fmt.Errorf("decode Kubernetes cluster metadata %s: %w", entry.Name(), decodeErr)
		}
		request.Provider = normalizeProvider(request.Provider)
		name := request.Name
		if !clusterNamePattern.MatchString(name) || entry.Name() != config.KubernetesClusterFileToken(name)+".json" {
			return nil, fmt.Errorf("invalid Kubernetes cluster metadata identity in %s", entry.Name())
		}
		cluster := Cluster{
			Name:           name,
			Provider:       normalizeProvider(request.Provider),
			State:          "stopped",
			Context:        "porto-" + name,
			KubeconfigPath: p.clusterKubeconfigPath(name),
		}
		if cluster.Provider == "kind" {
			output, kindErr := p.runner.Run(ctx, runtimes.Command{
				Name: "kind",
				Args: []string{"get", "nodes", "--name", "porto-" + name},
				Env:  portoDockerEnv(),
			})
			if kindErr == nil {
				cluster.Nodes = runningKindNodeNames(request, output)
				runningNodes := 0
				for _, node := range cluster.Nodes {
					stateOutput, stateErr := p.runner.Run(ctx, runtimes.Command{
						Name: "docker",
						Args: []string{"inspect", "--format", "{{.State.Running}}", node},
						Env:  portoDockerEnv(),
					})
					if stateErr == nil && strings.EqualFold(strings.TrimSpace(string(stateOutput)), "true") {
						runningNodes++
					}
				}
				switch {
				case len(cluster.Nodes) == len(kindNodeNames(request)) && runningNodes == len(cluster.Nodes):
					cluster.State = "running"
				case runningNodes > 0:
					cluster.State = "degraded"
				}
			}
			serverOutput, serverErr := p.runner.Run(ctx, runtimes.Command{
				Name: "kubectl",
				Args: []string{
					"--kubeconfig", cluster.KubeconfigPath,
					"--context", cluster.Context,
					"config", "view", "--minify",
					"-o", "jsonpath={.clusters[0].cluster.server}",
				},
			})
			if serverErr == nil {
				cluster.Server = strings.TrimSpace(string(serverOutput))
			}
		} else if request.APIPort > 0 {
			cluster.Server = "https://127.0.0.1:" + strconv.Itoa(request.APIPort)
		}
		if cluster.Provider != "kind" && instanceErr == nil {
			runningNodes := 0
			for _, nodeName := range clusterNodeNames(request) {
				if instance, ok := instanceByName[nodeName]; ok {
					cluster.Nodes = append(cluster.Nodes, nodeName)
					if strings.EqualFold(instance.Status, "running") {
						runningNodes++
					}
				}
			}
			switch {
			case runningNodes == len(clusterNodeNames(request)):
				cluster.State = "running"
			case runningNodes > 0:
				cluster.State = "degraded"
			}
			sort.Strings(cluster.Nodes)
		}
		clusters = append(clusters, cluster)
	}
	sort.Slice(clusters, func(left, right int) bool {
		return clusters[left].Name < clusters[right].Name
	})
	return clusters, nil
}

func (p *ClusterProvisioner) ScaleNodeGroup(
	ctx context.Context,
	clusterName string,
	group NodeGroupSpec,
	version string,
) error {
	if !clusterNamePattern.MatchString(clusterName) || !clusterNamePattern.MatchString(group.Name) {
		return fmt.Errorf("cluster and node group names must match %s", clusterNamePattern)
	}
	if group.Count < 0 || group.Count > 32 {
		return errors.New("node group count must be between 0 and 32")
	}
	if version != "" && !versionPattern.MatchString(version) {
		return fmt.Errorf("invalid Kubernetes version %q", version)
	}
	if err := validateNodeMetadata(group.Labels, group.Taints); err != nil {
		return err
	}
	group.Machine = normalizeMachine(group.Machine)
	request, err := p.readClusterMetadata(clusterName)
	if err != nil {
		return fmt.Errorf("read Kubernetes cluster ownership: %w", err)
	}
	request.Provider = normalizeProvider(request.Provider)
	if version == "" {
		version = request.Version
	}
	if request.Provider == "kind" {
		return errors.New("kind node count is immutable; recreate the cluster with the desired node groups")
	}
	currentGroup := NodeGroupSpec{Name: group.Name, Count: 0, Machine: group.Machine}
	groupIndex := -1
	for index := range request.NodeGroups {
		if request.NodeGroups[index].Name == group.Name {
			currentGroup = request.NodeGroups[index]
			groupIndex = index
			break
		}
	}
	instances, err := p.vms.ListAll(ctx)
	if err != nil {
		return err
	}
	instanceByName := make(map[string]vm.Instance, len(instances))
	for _, instance := range instances {
		instanceByName[instance.Name] = instance
	}
	currentComplete := true
	for index := 1; index <= currentGroup.Count; index++ {
		if _, exists := instanceByName[clusterMachineName(clusterName, group.Name, index)]; !exists {
			currentComplete = false
			break
		}
	}
	if currentGroup.Count == group.Count && currentComplete {
		return p.updateNodeGroupMetadata(clusterName, group, version)
	}
	serverName := clusterMachineName(clusterName, "server", 1)
	if currentGroup.Count <= group.Count {
		serverIPOutput, err := p.vms.Exec(ctx, serverName, []string{"hostname", "-I"}, nil)
		if err != nil {
			return fmt.Errorf("resolve Kubernetes server address: %w", err)
		}
		tokenOutput, err := p.clusterToken(ctx, request.Provider, serverName)
		if err != nil {
			return fmt.Errorf("read Kubernetes node token: %w", err)
		}
		serverIP := firstField(serverIPOutput)
		token := strings.TrimSpace(string(tokenOutput))
		created := make([]string, 0)
		for index := 1; index <= group.Count; index++ {
			nodeName := clusterMachineName(clusterName, group.Name, index)
			if _, exists := instanceByName[nodeName]; exists {
				continue
			}
			if _, createErr := p.vms.CreateNode(ctx, vm.CreateRequest{
				Name:      nodeName,
				Image:     "ubuntu-24.04",
				CPUs:      group.Machine.CPUs,
				MemoryMiB: group.Machine.MemoryMiB,
				DiskGiB:   group.Machine.DiskGiB,
				Network:   "user-v2",
				Start:     true,
			}); createErr != nil {
				return errors.Join(createErr, p.cleanupVMs(created))
			}
			created = append(created, nodeName)
			if installErr := p.installVMKubernetes(ctx, request.Provider, nodeName, version, false, serverIP, token, group.Labels, group.Taints); installErr != nil {
				return errors.Join(installErr, p.cleanupVMs(created))
			}
		}
	} else {
		for index := currentGroup.Count; index > group.Count; index-- {
			nodeName := clusterMachineName(clusterName, group.Name, index)
			if _, exists := instanceByName[nodeName]; !exists {
				continue
			}
			kubernetesNodeName := nodeName
			if request.Provider == "k0s" {
				kubernetesNodeName = "lima-" + nodeName
			}
			if _, err := p.vms.Exec(ctx, serverName, p.nodeKubectlCommand(request.Provider, "cordon", kubernetesNodeName), nil); err != nil {
				return fmt.Errorf("cordon Kubernetes node %s: %w", nodeName, err)
			}
			if _, err := p.vms.Exec(ctx, serverName, p.nodeKubectlCommand(
				request.Provider,
				"drain", kubernetesNodeName,
				"--ignore-daemonsets",
				"--delete-emptydir-data=false",
				"--timeout=2m",
			), nil); err != nil {
				return fmt.Errorf("drain Kubernetes node %s: %w", nodeName, err)
			}
			if _, err := p.vms.Exec(ctx, serverName, p.nodeKubectlCommand(request.Provider, "delete", "node", kubernetesNodeName, "--ignore-not-found=true"), nil); err != nil {
				return fmt.Errorf("remove Kubernetes node %s: %w", nodeName, err)
			}
			if err := p.vms.Delete(ctx, nodeName, true); err != nil {
				return err
			}
		}
	}
	if groupIndex >= 0 {
		request.NodeGroups[groupIndex] = group
	} else {
		request.NodeGroups = append(request.NodeGroups, group)
	}
	if version != "" {
		request.Version = version
	}
	if err := p.writeClusterMetadata(request); err != nil {
		return err
	}
	return nil
}

func (p *ClusterProvisioner) ImportImage(ctx context.Context, clusterName, image string) error {
	if !clusterNamePattern.MatchString(clusterName) {
		return fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	image = strings.TrimSpace(image)
	if image == "" || strings.HasPrefix(image, "-") || strings.ContainsAny(image, "\x00\r\n") {
		return errors.New("invalid container image reference")
	}
	request, err := p.readClusterMetadata(clusterName)
	if err != nil {
		return fmt.Errorf("read Kubernetes cluster ownership: %w", err)
	}
	request.Provider = normalizeProvider(request.Provider)
	if request.Provider == "kind" {
		output, err := p.runner.Run(ctx, runtimes.Command{
			Name: "kind",
			Args: []string{"load", "docker-image", image, "--name", "porto-" + clusterName},
			Env:  portoDockerEnv(),
		})
		if err != nil {
			return runtimes.CommandError("load image into kind cluster", output, err)
		}
		return nil
	}
	saveContext, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	imageArchive, err := p.runner.Run(saveContext, runtimes.Command{
		Name: "docker",
		Args: []string{"image", "save", image},
	})
	if err != nil {
		return runtimes.CommandError("export Docker image "+image, imageArchive, err)
	}
	imported := 0
	var importErrors []error
	for _, nodeName := range clusterNodeNames(request) {
		command := []string{"sudo", "k3s", "ctr", "images", "import", "-"}
		if request.Provider == "k0s" {
			command = []string{"sudo", "k0s", "ctr", "images", "import", "-"}
		}
		if _, err := p.vms.Exec(ctx, nodeName, command, imageArchive); err != nil {
			importErrors = append(importErrors, fmt.Errorf("import image into %s: %w", nodeName, err))
			continue
		}
		imported++
	}
	if imported == 0 && len(importErrors) == 0 {
		return fmt.Errorf("no node VMs found for Kubernetes cluster %s", clusterName)
	}
	return errors.Join(importErrors...)
}

func (p *ClusterProvisioner) SetRunning(ctx context.Context, clusterName string, running bool) error {
	if !clusterNamePattern.MatchString(clusterName) {
		return fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	request, err := p.readClusterMetadata(clusterName)
	if err != nil {
		return fmt.Errorf("read Kubernetes cluster ownership: %w", err)
	}
	request.Provider = normalizeProvider(request.Provider)
	names := clusterNodeNames(request)
	if request.Provider == "kind" {
		var actionErrors []error
		action := "start"
		if !running {
			action = "stop"
		}
		for _, name := range kindNodeNames(request) {
			output, actionErr := p.runner.Run(ctx, runtimes.Command{
				Name: "docker",
				Args: []string{action, name},
				Env:  portoDockerEnv(),
			})
			if actionErr != nil {
				actionErrors = append(actionErrors, runtimes.CommandError(action+" kind node "+name, output, actionErr))
			}
		}
		if err := errors.Join(actionErrors...); err != nil || !running {
			return err
		}
		if err := p.ensureKindMetricsServer(ctx, p.clusterKubeconfigPath(clusterName), "porto-"+clusterName); err != nil {
			log.Printf("kind cluster %s started without metrics-server: %v", clusterName, err)
		}
		return nil
	}
	if !running {
		for left, right := 0, len(names)-1; left < right; left, right = left+1, right-1 {
			names[left], names[right] = names[right], names[left]
		}
	}
	var actionErrors []error
	for _, name := range names {
		if running {
			err = p.vms.Start(ctx, name)
		} else {
			err = p.vms.Stop(ctx, name)
		}
		if err != nil {
			actionErrors = append(actionErrors, err)
		}
	}
	return errors.Join(actionErrors...)
}

func (p *ClusterProvisioner) EnsureMetricsServer(ctx context.Context, contextName string) (bool, error) {
	clusterName, ok := strings.CutPrefix(contextName, "porto-")
	if !ok || !clusterNamePattern.MatchString(clusterName) {
		return false, nil
	}
	request, err := p.readClusterMetadata(clusterName)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("read Kubernetes cluster ownership: %w", err)
	}
	if normalizeProvider(request.Provider) != "kind" {
		return false, nil
	}
	return true, p.ensureKindMetricsServer(ctx, p.clusterKubeconfigPath(clusterName), contextName)
}

func (p *ClusterProvisioner) ensureKindMetricsServer(ctx context.Context, kubeconfigPath, contextName string) error {
	p.metricsMu.Lock()
	if existing := p.metricsRuns[contextName]; existing != nil {
		p.metricsMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-existing.done:
			return existing.err
		}
	}
	run := &metricsServerRun{done: make(chan struct{})}
	p.metricsRuns[contextName] = run
	p.metricsMu.Unlock()

	operationContext, cancel := context.WithTimeout(ctx, 6*time.Minute)
	run.err = p.installKindMetricsServer(operationContext, kubeconfigPath, contextName)
	cancel()

	p.metricsMu.Lock()
	delete(p.metricsRuns, contextName)
	close(run.done)
	p.metricsMu.Unlock()
	return run.err
}

func (p *ClusterProvisioner) installKindMetricsServer(ctx context.Context, kubeconfigPath, contextName string) error {
	baseArgs := []string{"--kubeconfig", kubeconfigPath, "--context", contextName}
	run := func(action string, stdin []byte, args ...string) ([]byte, error) {
		commandArgs := append(append([]string(nil), baseArgs...), args...)
		output, err := p.runner.Run(ctx, runtimes.Command{Name: "kubectl", Args: commandArgs, Stdin: stdin})
		if err != nil {
			return output, runtimes.CommandError(action, output, err)
		}
		return output, nil
	}
	output, err := run(
		"inspect Metrics API",
		nil,
		"get", "apiservice", "v1beta1.metrics.k8s.io",
		"-o", `jsonpath={.status.conditions[?(@.type=="Available")].status}`,
	)
	if err == nil && strings.EqualFold(strings.TrimSpace(string(output)), "true") {
		return nil
	}
	if _, err := run("wait for kind nodes", nil, "wait", "--for=condition=Ready", "nodes", "--all", "--timeout=5m"); err != nil {
		return err
	}
	if _, err := run("install metrics-server", metricsServerManifest, "apply", "-f", "-"); err != nil {
		return err
	}
	if _, err := run(
		"wait for metrics-server deployment",
		nil,
		"--namespace", "kube-system",
		"wait", "--for=condition=Available", "deployment/metrics-server", "--timeout=5m",
	); err != nil {
		return err
	}
	if _, err := run(
		"wait for Metrics API",
		nil,
		"wait", "--for=condition=Available", "apiservice/v1beta1.metrics.k8s.io", "--timeout=5m",
	); err != nil {
		return err
	}
	return nil
}

func (p *ClusterProvisioner) writeClusterMetadata(request ClusterRequest) error {
	if err := os.MkdirAll(p.kubeconfigRoot, 0o700); err != nil {
		return fmt.Errorf("create Kubernetes metadata directory: %w", err)
	}
	data, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Kubernetes cluster metadata: %w", err)
	}
	if err := os.WriteFile(p.clusterMetadataPath(request.Name), data, 0o600); err != nil {
		return fmt.Errorf("write Kubernetes cluster metadata: %w", err)
	}
	return nil
}

func (p *ClusterProvisioner) updateNodeGroupMetadata(clusterName string, group NodeGroupSpec, version string) error {
	request, err := p.readClusterMetadata(clusterName)
	if err != nil {
		request = ClusterRequest{Name: clusterName, Version: version}
	}
	found := false
	for index := range request.NodeGroups {
		if request.NodeGroups[index].Name == group.Name {
			request.NodeGroups[index] = group
			found = true
			break
		}
	}
	if !found {
		request.NodeGroups = append(request.NodeGroups, group)
	}
	if version != "" {
		request.Version = version
	}
	return p.writeClusterMetadata(request)
}

func (p *ClusterProvisioner) readClusterMetadata(clusterName string) (ClusterRequest, error) {
	data, err := os.ReadFile(p.clusterMetadataPath(clusterName))
	if err != nil {
		return ClusterRequest{}, err
	}
	var request ClusterRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return ClusterRequest{}, fmt.Errorf("decode Kubernetes cluster metadata: %w", err)
	}
	return request, nil
}

func (p *ClusterProvisioner) clusterMetadataPath(clusterName string) string {
	return filepath.Join(p.kubeconfigRoot, config.KubernetesClusterFileToken(clusterName)+".json")
}

func (p *ClusterProvisioner) clusterKubeconfigPath(clusterName string) string {
	return filepath.Join(p.kubeconfigRoot, config.KubernetesClusterFileToken(clusterName)+".yaml")
}

func (p *ClusterProvisioner) cleanupVMs(names []string) error {
	var cleanupErrors []error
	for index := len(names) - 1; index >= 0; index-- {
		if err := p.vms.Delete(context.Background(), names[index], true); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	return errors.Join(cleanupErrors...)
}

func normalizeProvider(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return "k3s"
	}
	switch provider {
	case "kind", "k0s", "k3s":
		return provider
	default:
		return ""
	}
}

func (p *ClusterProvisioner) installVMKubernetes(
	ctx context.Context,
	provider string,
	machineName string,
	version string,
	server bool,
	serverIP string,
	token string,
	labels map[string]string,
	taints []string,
) error {
	switch provider {
	case "k0s":
		return p.installK0s(ctx, machineName, version, server, token, labels, taints)
	case "k3s":
		return p.installK3s(ctx, machineName, version, server, serverIP, token, labels, taints)
	default:
		return fmt.Errorf("provider %q does not use VM provisioning", provider)
	}
}

func (p *ClusterProvisioner) clusterToken(ctx context.Context, provider, serverName string) ([]byte, error) {
	switch provider {
	case "k0s":
		return p.vms.Exec(ctx, serverName, []string{"sudo", "k0s", "token", "create", "--role=worker", "--expiry=1h"}, nil)
	case "k3s":
		return p.vms.Exec(ctx, serverName, []string{"sudo", "cat", "/var/lib/rancher/k3s/server/node-token"}, nil)
	default:
		return nil, fmt.Errorf("provider %q does not expose a VM join token", provider)
	}
}

func (p *ClusterProvisioner) clusterKubeconfig(ctx context.Context, provider, serverName string) ([]byte, error) {
	switch provider {
	case "k0s":
		return p.vms.Exec(ctx, serverName, []string{"sudo", "k0s", "kubeconfig", "admin"}, nil)
	case "k3s":
		return p.vms.Exec(ctx, serverName, []string{"sudo", "cat", "/etc/rancher/k3s/k3s.yaml"}, nil)
	default:
		return nil, fmt.Errorf("provider %q does not expose a VM kubeconfig", provider)
	}
}

func (p *ClusterProvisioner) nodeKubectlCommand(provider string, args ...string) []string {
	command := []string{"sudo", "k3s", "kubectl"}
	if provider == "k0s" {
		command = []string{"sudo", "k0s", "kubectl"}
	}
	return append(command, args...)
}

func (p *ClusterProvisioner) installK0s(
	ctx context.Context,
	machineName string,
	version string,
	controller bool,
	token string,
	labels map[string]string,
	taints []string,
) error {
	if _, err := p.vms.Exec(ctx, machineName, []string{
		"sh", "-c", "curl -sSLf https://get.k0s.sh -o /tmp/porto-install-k0s.sh",
	}, nil); err != nil {
		return fmt.Errorf("download k0s installer on %s: %w", machineName, err)
	}
	install := []string{"sudo", "env"}
	if version != "" {
		install = append(install, "K0S_VERSION="+version)
	}
	install = append(install, "sh", "/tmp/porto-install-k0s.sh")
	if _, err := p.vms.Exec(ctx, machineName, install, nil); err != nil {
		return fmt.Errorf("install k0s binary on %s: %w", machineName, err)
	}
	if controller {
		if _, err := p.vms.Exec(ctx, machineName, []string{
			"sudo", "k0s", "install", "controller", "--enable-worker", "--no-taints",
		}, nil); err != nil {
			return fmt.Errorf("configure k0s controller on %s: %w", machineName, err)
		}
	} else {
		if strings.TrimSpace(token) == "" {
			return errors.New("k0s worker token is empty")
		}
		if _, err := p.vms.Exec(ctx, machineName, []string{
			"sh", "-c", "cat > /tmp/porto-k0s-worker-token",
		}, []byte(token)); err != nil {
			return fmt.Errorf("write k0s worker token on %s: %w", machineName, err)
		}
		workerInstall := []string{"sudo", "k0s", "install", "worker", "--token-file", "/tmp/porto-k0s-worker-token"}
		labelKeys := make([]string, 0, len(labels))
		for key := range labels {
			labelKeys = append(labelKeys, key)
		}
		sort.Strings(labelKeys)
		for _, key := range labelKeys {
			workerInstall = append(workerInstall, "--labels", key+"="+labels[key])
		}
		for _, taint := range taints {
			workerInstall = append(workerInstall, "--taints", taint)
		}
		if _, err := p.vms.Exec(ctx, machineName, workerInstall, nil); err != nil {
			return fmt.Errorf("configure k0s worker on %s: %w", machineName, err)
		}
	}
	if _, err := p.vms.Exec(ctx, machineName, []string{"sudo", "k0s", "start"}, nil); err != nil {
		return fmt.Errorf("start k0s on %s: %w", machineName, err)
	}
	return p.waitForK0s(ctx, machineName)
}

func (p *ClusterProvisioner) waitForK0s(ctx context.Context, machineName string) error {
	waitContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		if _, err := p.vms.Exec(waitContext, machineName, []string{"sudo", "k0s", "status"}, nil); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-waitContext.Done():
			return fmt.Errorf("k0s on %s did not become ready: %w", machineName, errors.Join(lastErr, waitContext.Err()))
		case <-ticker.C:
		}
	}
}

func (p *ClusterProvisioner) installK3s(
	ctx context.Context,
	machineName string,
	version string,
	server bool,
	serverIP string,
	token string,
	labels map[string]string,
	taints []string,
) error {
	if _, err := p.vms.Exec(ctx, machineName, []string{
		"sh", "-c", "curl -sfL https://get.k3s.io -o /tmp/porto-install-k3s.sh",
	}, nil); err != nil {
		return fmt.Errorf("download k3s installer on %s: %w", machineName, err)
	}
	env := []string{"sudo", "env"}
	if version != "" {
		env = append(env, "INSTALL_K3S_VERSION="+version)
	}
	if !server {
		env = append(env, "K3S_URL=https://"+serverIP+":6443", "K3S_TOKEN="+token)
	}
	args := append(env, "sh", "/tmp/porto-install-k3s.sh")
	if server {
		args = append(args, "server", "--node-name", machineName, "--write-kubeconfig-mode", "600")
	} else {
		args = append(args, "agent", "--node-name", machineName)
	}
	for key, value := range labels {
		args = append(args, "--node-label", key+"="+value)
	}
	for _, taint := range taints {
		args = append(args, "--node-taint", taint)
	}
	if _, err := p.vms.Exec(ctx, machineName, args, nil); err != nil {
		return fmt.Errorf("install k3s on %s: %w", machineName, err)
	}
	return nil
}

func normalizeMachine(spec MachineSpec) MachineSpec {
	if spec.CPUs <= 0 {
		spec.CPUs = 2
	}
	if spec.MemoryMiB <= 0 {
		spec.MemoryMiB = 2048
	}
	if spec.DiskGiB <= 0 {
		spec.DiskGiB = 20
	}
	return spec
}

func validateNodeMetadata(labels map[string]string, taints []string) error {
	for key, value := range labels {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key+value, "\x00\r\n") {
			return errors.New("node labels cannot be empty or contain control characters")
		}
	}
	for _, taint := range taints {
		if strings.TrimSpace(taint) == "" || strings.ContainsAny(taint, "\x00\r\n") || strings.HasPrefix(taint, "-") {
			return errors.New("invalid node taint")
		}
	}
	return nil
}

func clusterMachineName(cluster, group string, index int) string {
	return "porto-" + cluster + "-" + group + "-" + strconv.Itoa(index)
}

func clusterNodeNames(request ClusterRequest) []string {
	names := []string{clusterMachineName(request.Name, "server", 1)}
	for _, group := range request.NodeGroups {
		for index := 1; index <= group.Count; index++ {
			names = append(names, clusterMachineName(request.Name, group.Name, index))
		}
	}
	return names
}

func firstField(output []byte) string {
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func availableLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate Kubernetes API port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func portoDockerEnv() []string {
	socketPath, err := config.DockerSocketPath()
	if err != nil || socketPath == "" {
		return nil
	}
	endpoint := "unix://" + socketPath
	if strings.HasPrefix(socketPath, `\\.\pipe\`) {
		endpoint = "npipe:////./pipe/" + strings.TrimPrefix(socketPath, `\\.\pipe\`)
	}
	return []string{"DOCKER_HOST=" + endpoint, "DOCKER_CONTEXT="}
}

func (p *ClusterProvisioner) normalizeKubeconfig(ctx context.Context, kubeconfigPath, name string) error {
	output, err := p.runner.Run(ctx, runtimes.Command{
		Name: "kubectl",
		Args: []string{"--kubeconfig", kubeconfigPath, "config", "view", "--raw", "-o", "json"},
	})
	if err != nil {
		return runtimes.CommandError("normalize Kubernetes kubeconfig", output, err)
	}
	var configDocument map[string]any
	if err := json.Unmarshal(output, &configDocument); err != nil {
		return fmt.Errorf("decode normalized kubeconfig: %w", err)
	}
	configDocument["current-context"] = name
	renameKubeconfigEntries(configDocument["clusters"], name, nil)
	renameKubeconfigEntries(configDocument["users"], name, nil)
	renameKubeconfigEntries(configDocument["contexts"], name, func(entry map[string]any) {
		contextValue, ok := entry["context"].(map[string]any)
		if !ok {
			return
		}
		contextValue["cluster"] = name
		contextValue["user"] = name
	})
	normalized, err := json.MarshalIndent(configDocument, "", "  ")
	if err != nil {
		return fmt.Errorf("encode normalized kubeconfig: %w", err)
	}
	if err := os.WriteFile(kubeconfigPath, normalized, 0o600); err != nil {
		return fmt.Errorf("write normalized kubeconfig: %w", err)
	}
	return nil
}

func renameKubeconfigEntries(value any, name string, update func(map[string]any)) {
	entries, ok := value.([]any)
	if !ok {
		return
	}
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			continue
		}
		entry["name"] = name
		if update != nil {
			update(entry)
		}
	}
}
