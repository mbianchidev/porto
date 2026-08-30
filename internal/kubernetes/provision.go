package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

var clusterNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
var versionPattern = regexp.MustCompile(`^[A-Za-z0-9.+-]+$`)

type ClusterProvisioner struct {
	vms            *vm.Manager
	runner         runtimes.Runner
	kubeconfigRoot string
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
	Version      string          `json:"version"`
	ControlPlane MachineSpec     `json:"controlPlane"`
	NodeGroups   []NodeGroupSpec `json:"nodeGroups"`
}

type Cluster struct {
	Name           string   `json:"name"`
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
	return &ClusterProvisioner{vms: vms, runner: runner, kubeconfigRoot: kubeconfigRoot}
}

func (p *ClusterProvisioner) Create(ctx context.Context, request ClusterRequest) (cluster Cluster, err error) {
	if !clusterNamePattern.MatchString(request.Name) {
		return Cluster{}, fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	if request.Version != "" && !versionPattern.MatchString(request.Version) {
		return Cluster{}, fmt.Errorf("invalid k3s version %q", request.Version)
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
		err = errors.Join(err, errors.Join(cleanupErrors...))
	}()

	serverName := clusterMachineName(request.Name, "server", 1)
	if _, err = p.vms.Create(ctx, vm.CreateRequest{
		Name:      serverName,
		Image:     "ubuntu-24.04",
		CPUs:      request.ControlPlane.CPUs,
		MemoryMiB: request.ControlPlane.MemoryMiB,
		DiskGiB:   request.ControlPlane.DiskGiB,
		Start:     true,
	}); err != nil {
		return Cluster{}, fmt.Errorf("create Kubernetes control-plane VM: %w", err)
	}
	created = append(created, serverName)
	if err = p.installK3s(ctx, serverName, request.Version, true, "", "", nil, nil); err != nil {
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
	tokenOutput, err := p.vms.Exec(ctx, serverName, []string{"sudo", "cat", "/var/lib/rancher/k3s/server/node-token"}, nil)
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
			if _, err = p.vms.Create(ctx, vm.CreateRequest{
				Name:      nodeName,
				Image:     "ubuntu-24.04",
				CPUs:      group.Machine.CPUs,
				MemoryMiB: group.Machine.MemoryMiB,
				DiskGiB:   group.Machine.DiskGiB,
				Start:     true,
			}); err != nil {
				return Cluster{}, fmt.Errorf("create Kubernetes worker VM %s: %w", nodeName, err)
			}
			created = append(created, nodeName)
			if err = p.installK3s(ctx, nodeName, request.Version, false, serverIP, token, group.Labels, group.Taints); err != nil {
				return Cluster{}, err
			}
			nodes = append(nodes, nodeName)
		}
	}

	kubeconfigOutput, err := p.vms.Exec(ctx, serverName, []string{"sudo", "cat", "/etc/rancher/k3s/k3s.yaml"}, nil)
	if err != nil {
		return Cluster{}, fmt.Errorf("read Kubernetes kubeconfig: %w", err)
	}
	kubeconfig := strings.ReplaceAll(string(kubeconfigOutput), "https://127.0.0.1:6443", "https://"+serverIP+":6443")
	kubeconfigPath := filepath.Join(p.kubeconfigRoot, request.Name+".yaml")
	if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o700); err != nil {
		return Cluster{}, fmt.Errorf("create kubeconfig directory: %w", err)
	}
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0o600); err != nil {
		return Cluster{}, fmt.Errorf("write kubeconfig: %w", err)
	}
	contextName := "porto-" + request.Name
	renameContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := p.runner.Run(renameContext, runtimes.Command{
		Name: "kubectl",
		Args: []string{"--kubeconfig", kubeconfigPath, "config", "rename-context", "default", contextName},
	})
	if err != nil {
		return Cluster{}, runtimes.CommandError("rename Porto Kubernetes context", output, err)
	}
	cluster = Cluster{
		Name:           request.Name,
		Context:        contextName,
		KubeconfigPath: kubeconfigPath,
		Server:         "https://" + serverIP + ":6443",
		Nodes:          nodes,
	}
	if err = p.writeClusterMetadata(request); err != nil {
		return Cluster{}, err
	}
	return cluster, nil
}

func (p *ClusterProvisioner) Delete(ctx context.Context, clusterName string) error {
	if !clusterNamePattern.MatchString(clusterName) {
		return fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	instances, err := p.vms.List(ctx)
	if err != nil {
		return err
	}
	prefix := "porto-" + clusterName + "-"
	var deleteErrors []error
	for _, instance := range instances {
		if strings.HasPrefix(instance.Name, prefix) {
			if err := p.vms.Delete(ctx, instance.Name, true); err != nil {
				deleteErrors = append(deleteErrors, err)
			}
		}
	}
	kubeconfigPath := filepath.Join(p.kubeconfigRoot, clusterName+".yaml")
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
	instances, instanceErr := p.vms.List(ctx)
	clusters := make([]Cluster, 0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".yaml")
		if !clusterNamePattern.MatchString(name) {
			continue
		}
		cluster := Cluster{
			Name:           name,
			Context:        "porto-" + name,
			KubeconfigPath: filepath.Join(p.kubeconfigRoot, entry.Name()),
		}
		if instanceErr == nil {
			prefix := "porto-" + name + "-"
			for _, instance := range instances {
				if strings.HasPrefix(instance.Name, prefix) {
					cluster.Nodes = append(cluster.Nodes, instance.Name)
				}
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
		return fmt.Errorf("invalid k3s version %q", version)
	}
	if err := validateNodeMetadata(group.Labels, group.Taints); err != nil {
		return err
	}
	group.Machine = normalizeMachine(group.Machine)
	instances, err := p.vms.List(ctx)
	if err != nil {
		return err
	}
	prefix := "porto-" + clusterName + "-" + group.Name + "-"
	current := make([]string, 0)
	for _, instance := range instances {
		if strings.HasPrefix(instance.Name, prefix) {
			current = append(current, instance.Name)
		}
	}
	sort.Strings(current)
	if len(current) == group.Count {
		return p.updateNodeGroupMetadata(clusterName, group, version)
	}
	serverName := clusterMachineName(clusterName, "server", 1)
	if len(current) < group.Count {
		serverIPOutput, err := p.vms.Exec(ctx, serverName, []string{"hostname", "-I"}, nil)
		if err != nil {
			return fmt.Errorf("resolve Kubernetes server address: %w", err)
		}
		tokenOutput, err := p.vms.Exec(ctx, serverName, []string{"sudo", "cat", "/var/lib/rancher/k3s/server/node-token"}, nil)
		if err != nil {
			return fmt.Errorf("read Kubernetes node token: %w", err)
		}
		serverIP := firstField(serverIPOutput)
		token := strings.TrimSpace(string(tokenOutput))
		created := make([]string, 0)
		for index := len(current) + 1; index <= group.Count; index++ {
			nodeName := clusterMachineName(clusterName, group.Name, index)
			if _, createErr := p.vms.Create(ctx, vm.CreateRequest{
				Name:      nodeName,
				Image:     "ubuntu-24.04",
				CPUs:      group.Machine.CPUs,
				MemoryMiB: group.Machine.MemoryMiB,
				DiskGiB:   group.Machine.DiskGiB,
				Start:     true,
			}); createErr != nil {
				return errors.Join(createErr, p.cleanupVMs(created))
			}
			created = append(created, nodeName)
			if installErr := p.installK3s(ctx, nodeName, version, false, serverIP, token, group.Labels, group.Taints); installErr != nil {
				return errors.Join(installErr, p.cleanupVMs(created))
			}
		}
	} else {
		for index := len(current) - 1; index >= group.Count; index-- {
			nodeName := current[index]
			if _, err := p.vms.Exec(ctx, serverName, []string{"sudo", "k3s", "kubectl", "delete", "node", nodeName, "--ignore-not-found=true"}, nil); err != nil {
				return fmt.Errorf("remove Kubernetes node %s: %w", nodeName, err)
			}
			if err := p.vms.Delete(ctx, nodeName, true); err != nil {
				return err
			}
		}
	}
	return p.updateNodeGroupMetadata(clusterName, group, version)
}

func (p *ClusterProvisioner) SetRunning(ctx context.Context, clusterName string, running bool) error {
	if !clusterNamePattern.MatchString(clusterName) {
		return fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	instances, err := p.vms.List(ctx)
	if err != nil {
		return err
	}
	prefix := "porto-" + clusterName + "-"
	var actionErrors []error
	for _, instance := range instances {
		if !strings.HasPrefix(instance.Name, prefix) {
			continue
		}
		if running {
			err = p.vms.Start(ctx, instance.Name)
		} else {
			err = p.vms.Stop(ctx, instance.Name)
		}
		if err != nil {
			actionErrors = append(actionErrors, err)
		}
	}
	return errors.Join(actionErrors...)
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
	return filepath.Join(p.kubeconfigRoot, clusterName+".json")
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

func firstField(output []byte) string {
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}
