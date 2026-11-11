package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/providers"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type appleClusterResource struct {
	ID            string `json:"id"`
	Configuration struct {
		Name              string `json:"name"`
		KubernetesVersion string `json:"kubernetesVersion"`
		CreationDate      string `json:"creationDate"`
		Nodes             []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"configuration"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

func unsupportedAppleOperation(action string) error {
	return fmt.Errorf("Apple Cluster does not support %s; use a Lima/VZ k3s or k0s cluster for this operation. Existing cluster data was not changed", action)
}

func (p *ClusterProvisioner) CheckProviderOperation(name, action string) error {
	switch action {
	case "start", "stop", "scale", "upgrade", "upgrade-recover":
	default:
		return nil
	}
	if !clusterNamePattern.MatchString(name) {
		return fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	request, err := p.readClusterMetadata(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Kubernetes cluster capabilities: %w", err)
	}
	if normalizeProvider(request.Provider) == "apple" {
		return unsupportedAppleOperation(action)
	}
	return nil
}

func (p *ClusterProvisioner) validateAppleRequest(request ClusterRequest) error {
	if err := p.requireAppleHost(); err != nil {
		return err
	}
	if request.APIPort != 0 || request.ControlPlane.DiskGiB != 0 {
		return errors.New("Apple Cluster assigns its API port and node disk sizes; apiPort and diskGiB must be omitted")
	}
	for _, group := range request.NodeGroups {
		if group.Machine.DiskGiB != 0 {
			return errors.New("Apple Cluster does not support worker disk sizing; diskGiB must be omitted")
		}
	}
	if request.Version != "" {
		if _, err := parseUpgradeVersion("kind", request.Version); err != nil {
			return fmt.Errorf("Apple Cluster node image version: %w", err)
		}
	}
	return nil
}

func (p *ClusterProvisioner) requireAppleHost() error {
	if p.goos != "darwin" || p.goarch != "arm64" {
		return errors.New("Apple Cluster is available only on Apple silicon Macs with macOS 26 or newer; Lima/VZ providers remain available")
	}
	return nil
}

func appleClusterName(request ClusterRequest) string {
	return "porto-apple-" + clusterRuntimeName(request)
}

func appleNodeNames(request ClusterRequest) []string {
	name := appleClusterName(request)
	nodes := []string{name}
	for _, group := range request.NodeGroups {
		for range group.Count {
			nodes = append(nodes, name+"-worker-"+strconv.Itoa(len(nodes)))
		}
	}
	return nodes
}

func (p *ClusterProvisioner) runApple(ctx context.Context, command runtimes.Command) ([]byte, error) {
	path, err := p.appleLookPath("container")
	if err != nil {
		return nil, fmt.Errorf("find Apple container CLI; install it with 'brew install container': %w", err)
	}
	command.Name = path
	return p.runner.Run(ctx, command)
}

func (p *ClusterProvisioner) appleClusters(ctx context.Context) ([]appleClusterResource, error) {
	if err := p.requireAppleHost(); err != nil {
		return nil, err
	}
	probeContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := p.runApple(probeContext, runtimes.Command{
		Args: []string{"--version"}, StdoutOnly: true,
	})
	if err != nil {
		return nil, runtimes.CommandError("inspect Apple container CLI; install it with 'brew install container'", output, err)
	}
	if err := providers.ValidateAppleContainerVersion(string(output)); err != nil {
		return nil, err
	}
	output, err = p.runApple(probeContext, runtimes.Command{
		Args: []string{"cluster", "list", "--format", "json"}, StdoutOnly: true,
	})
	if err != nil {
		return nil, runtimes.CommandError("list Apple clusters; start compatible services with 'container system start'", output, err)
	}
	var resources []appleClusterResource
	if err := json.Unmarshal(output, &resources); err != nil {
		return nil, fmt.Errorf("decode Apple clusters: %w", err)
	}
	if resources == nil {
		return nil, errors.New("Apple cluster inventory must be a JSON array, not null")
	}
	for _, resource := range resources {
		if resource.ID == "" || resource.ID != resource.Configuration.Name ||
			resource.Configuration.KubernetesVersion == "" || len(resource.Configuration.Nodes) == 0 {
			return nil, errors.New("Apple cluster inventory contains an invalid cluster identity")
		}
		if _, err := time.Parse(time.RFC3339, resource.Configuration.CreationDate); err != nil {
			return nil, fmt.Errorf("read Apple cluster creation identity: %w", err)
		}
		switch resource.Status.State {
		case "running", "degraded", "stopped":
		default:
			return nil, fmt.Errorf("unknown Apple cluster state %q", resource.Status.State)
		}
	}
	return resources, nil
}

func (p *ClusterProvisioner) ensureAppleRuntimeAbsent(ctx context.Context, request ClusterRequest) error {
	resources, err := p.appleClusters(ctx)
	if err != nil {
		return err
	}
	expected := appleNodeNames(request)
	for _, resource := range resources {
		if resource.ID == appleClusterName(request) {
			return fmt.Errorf("%w: Apple cluster %s already exists and will not be adopted", errClusterRuntimeCollision, resource.ID)
		}
		for _, node := range resource.Configuration.Nodes {
			if slices.Contains(expected, node.Name) {
				return fmt.Errorf("%w: Apple node %s already belongs to another cluster", errClusterRuntimeCollision, node.Name)
			}
		}
	}
	return nil
}

func ownedAppleCluster(request ClusterRequest, resources []appleClusterResource) (*appleClusterResource, error) {
	for _, resource := range resources {
		if resource.ID != appleClusterName(request) {
			continue
		}
		if request.AppleCreatedAt == "" || request.AppleCreatedAt != resource.Configuration.CreationDate {
			return nil, fmt.Errorf("Apple cluster %s has missing or changed ownership; refusing to operate on a possibly recreated cluster", resource.ID)
		}
		nodes := make([]string, 0, len(resource.Configuration.Nodes))
		for _, node := range resource.Configuration.Nodes {
			nodes = append(nodes, node.Name)
		}
		if !slices.Equal(nodes, appleNodeNames(request)) {
			return nil, fmt.Errorf("Apple cluster %s node identity changed; refusing to operate on unowned nodes", resource.ID)
		}
		return &resource, nil
	}
	return nil, nil
}

func (p *ClusterProvisioner) createApple(ctx context.Context, request *ClusterRequest) (Cluster, error) {
	kubeconfigPath := p.clusterKubeconfigPath(request.Name)
	if err := os.MkdirAll(p.kubeconfigRoot, 0o700); err != nil {
		return Cluster{}, fmt.Errorf("create Apple cluster kubeconfig directory: %w", err)
	}
	args := []string{
		"cluster", "create", "--name", appleClusterName(*request),
		"--cpus", strconv.Itoa(request.ControlPlane.CPUs),
		"--memory", strconv.Itoa(request.ControlPlane.MemoryMiB) + "m",
	}
	if request.Version != "" {
		args = append(args, "--node-image", "docker.io/kindest/node:"+request.Version)
	}
	for _, group := range request.NodeGroups {
		if group.Count > 0 {
			args = append(args, "--workers", fmt.Sprintf("count=%d,cpus=%d,memory=%dm",
				group.Count, group.Machine.CPUs, group.Machine.MemoryMiB))
		}
	}
	createContext, cancel := provisioningContext(ctx, "create Apple cluster", kindCreationTimeout)
	defer cancel()
	output, err := p.runApple(createContext, runtimes.Command{
		Args: args, Env: []string{"KUBECONFIG=" + kubeconfigPath}, Network: true,
	})
	if err != nil || createContext.Err() != nil {
		return Cluster{}, provisioningCommandError(createContext, "create Apple cluster", output, err)
	}
	resources, err := p.appleClusters(ctx)
	if err != nil {
		return Cluster{}, fmt.Errorf("Apple cluster creation succeeded but ownership could not be recorded: %w", err)
	}
	for _, resource := range resources {
		if resource.ID == appleClusterName(*request) {
			request.AppleCreatedAt = resource.Configuration.CreationDate
			request.Version = resource.Configuration.KubernetesVersion
			break
		}
	}
	resource, err := ownedAppleCluster(*request, resources)
	if err != nil {
		return Cluster{}, err
	}
	if resource == nil {
		return Cluster{}, errors.New("Apple cluster creation completed but the cluster is missing from native inventory")
	}
	if err := p.writeClusterMetadata(*request); err != nil {
		return Cluster{}, err
	}
	contextName := clusterContextName(*request)
	if err := p.normalizeKubeconfig(ctx, kubeconfigPath, contextName); err != nil {
		return Cluster{}, err
	}
	run := p.addonCommandRunner(ctx, kubeconfigPath, contextName)
	worker := 1
	for _, group := range request.NodeGroups {
		for range group.Count {
			node := appleClusterName(*request) + "-worker-" + strconv.Itoa(worker)
			worker++
			keys := make([]string, 0, len(group.Labels))
			for key := range group.Labels {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			if len(keys) > 0 {
				args := []string{"label", "node", node, "--overwrite=true"}
				for _, key := range keys {
					args = append(args, key+"="+group.Labels[key])
				}
				if _, err := run("label Apple cluster node", nil, args...); err != nil {
					return Cluster{}, err
				}
			}
			if len(group.Taints) > 0 {
				args := append([]string{"taint", "node", node, "--overwrite=true"}, group.Taints...)
				if _, err := run("taint Apple cluster node", nil, args...); err != nil {
					return Cluster{}, err
				}
			}
		}
	}
	if err := p.ensureKindMetricsServer(ctx, kubeconfigPath, contextName); err != nil {
		return Cluster{}, err
	}
	if err := p.ensureClusterAddons(ctx, kubeconfigPath, contextName, request.Provider); err != nil {
		return Cluster{}, err
	}
	cluster := Cluster{
		Name: request.Name, Provider: "apple", State: "running", Context: contextName,
		KubeconfigPath: kubeconfigPath, Version: request.Version, Nodes: appleNodeNames(*request),
	}
	if err := p.appleClusterServer(ctx, &cluster); err != nil {
		return Cluster{}, err
	}
	request.Phase, request.Error = "", ""
	request.RunningAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := p.writeClusterMetadata(*request); err != nil {
		return Cluster{}, err
	}
	if _, err := p.registerKubeconfig(ctx, request.Name); err != nil {
		warning, fatalErr := kubeconfigRegistrationResult(request.Name, err)
		if fatalErr != nil {
			return Cluster{}, fatalErr
		}
		cluster.Message = warning
	}
	return cluster, nil
}

func (p *ClusterProvisioner) appleClusterServer(ctx context.Context, cluster *Cluster) error {
	probeContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := p.runner.Run(probeContext, runtimes.Command{
		Name: "kubectl",
		Args: []string{"--kubeconfig", cluster.KubeconfigPath, "--context", cluster.Context,
			"config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}"},
		StdoutOnly: true,
	})
	if err != nil {
		return runtimes.CommandError("read Apple cluster API endpoint", output, err)
	}
	cluster.Server = strings.TrimSpace(string(output))
	if cluster.Server == "" {
		return errors.New("Apple cluster kubeconfig has no API endpoint")
	}
	return nil
}

func (p *ClusterProvisioner) observeAppleCluster(
	ctx context.Context,
	request ClusterRequest,
	cluster *Cluster,
	resources []appleClusterResource,
	inventoryErr error,
) {
	resource, err := ownedAppleCluster(request, resources)
	if inventoryErr != nil {
		err = inventoryErr
	}
	if err == nil && resource == nil {
		err = errors.New("Apple cluster is missing; recovery requires explicit deletion and recreation, not Start")
	}
	if err == nil {
		cluster.Nodes = appleNodeNames(request)
		cluster.Version = resource.Configuration.KubernetesVersion
		if request.Phase == "" {
			cluster.State = resource.Status.State
		}
		err = p.appleClusterServer(ctx, cluster)
	}
	if err != nil {
		if request.Phase == "" {
			cluster.State = "unavailable"
		}
		if cluster.Message != "" {
			cluster.Message += " "
		}
		cluster.Message += err.Error()
	}
}

func (p *ClusterProvisioner) deleteApple(ctx context.Context, request ClusterRequest) error {
	resources, err := p.appleClusters(ctx)
	if err != nil {
		return err
	}
	resource, err := ownedAppleCluster(request, resources)
	if err != nil || resource == nil {
		return err
	}
	deleteContext, cancel := provisioningContext(ctx, "delete Apple cluster", 5*time.Minute)
	defer cancel()
	output, err := p.runApple(deleteContext, runtimes.Command{
		Args: []string{"cluster", "delete", "--name", appleClusterName(request)},
		Env:  []string{"KUBECONFIG=" + p.clusterKubeconfigPath(request.Name)},
	})
	if err != nil || deleteContext.Err() != nil {
		return provisioningCommandError(deleteContext, "delete Apple cluster", output, err)
	}
	return nil
}

func (p *ClusterProvisioner) importAppleImage(ctx context.Context, request ClusterRequest, image string) (err error) {
	resources, err := p.appleClusters(ctx)
	if err != nil {
		return err
	}
	resource, err := ownedAppleCluster(request, resources)
	if err != nil {
		return err
	}
	if resource == nil || resource.Status.State != "running" {
		return errors.New("image import requires a running, owned Apple cluster; stopped Apple clusters require explicit recreation")
	}
	archive, err := os.CreateTemp(p.kubeconfigRoot, ".apple-image-*.tar")
	if err != nil {
		return fmt.Errorf("create private Apple image archive: %w", err)
	}
	archivePath := archive.Name()
	defer func() {
		if removeErr := os.Remove(archivePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove private Apple image archive: %w", removeErr))
		}
	}()
	if err := archive.Close(); err != nil {
		return fmt.Errorf("close private Apple image archive: %w", err)
	}
	importContext, cancel := provisioningContext(ctx, "import Apple cluster image", 30*time.Minute)
	defer cancel()
	output, err := p.runApple(importContext, runtimes.Command{
		Args: []string{"image", "save", "--platform", "linux/arm64", "--output", archivePath, image},
	})
	if err != nil || importContext.Err() != nil {
		return provisioningCommandError(importContext, "export image from the Apple container image store", output, err)
	}
	var importErrors []error
	for _, node := range appleNodeNames(request) {
		input, err := os.Open(archivePath)
		if err != nil {
			return errors.Join(errors.Join(importErrors...), fmt.Errorf("open private Apple image archive: %w", err))
		}
		output, runErr := p.runApple(importContext, runtimes.Command{
			Args: []string{"exec", "--interactive", node, "ctr", "--namespace", "k8s.io",
				"images", "import", "--platform", "linux/arm64", "-"},
			StdinReader: input,
		})
		runErr = errors.Join(runErr, input.Close())
		if runErr != nil || importContext.Err() != nil {
			importErrors = append(importErrors, provisioningCommandError(importContext, "import image into Apple node "+node, output, runErr))
			if importContext.Err() != nil {
				break
			}
		}
	}
	return errors.Join(importErrors...)
}
