package kubernetes

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// AutoscalingNodeName validates VM ownership before a provider creates, adopts, or
// deletes a worker. The operation gate must be held by the caller.
func (p *ClusterProvisioner) AutoscalingNodeName(ctx context.Context, clusterName, groupName string) (string, error) {
	if !clusterNamePattern.MatchString(clusterName) || !clusterNamePattern.MatchString(groupName) || !strings.HasPrefix(groupName, "as-") {
		return "", fmt.Errorf("invalid autoscaling cluster or reserved node group name")
	}

	request, err := p.readMutableClusterMetadata(clusterName)
	if err != nil {
		return "", err
	}
	provider := normalizeProvider(request.Provider)
	if provider != "k3s" && provider != "k0s" {
		return "", fmt.Errorf("real node autoscaling requires a k3s or k0s VM cluster; kind topology is immutable")
	}
	machineName := clusterMachineName(clusterRuntimeName(request), groupName, 1)
	owned, err := p.vms.KubernetesNodeNames(clusterName)
	if err != nil {
		return "", err
	}
	instances, err := p.vms.ListAll(ctx)
	if err != nil {
		return "", err
	}
	for _, instance := range instances {
		if instance.Name == machineName && !slices.Contains(owned, machineName) {
			return "", fmt.Errorf("refusing autoscaling mutation: VM %s is not owned by cluster %s", machineName, clusterName)
		}
	}
	if provider == "k0s" {
		return "lima-" + machineName, nil
	}
	return machineName, nil
}

// DeleteAutoscalingNode is the cloud-provider half of upstream termination.
// Karpenter already drained and detached volumes. Re-entering kubectl delete
// here would deadlock against its Node finalizer.
func (p *ClusterProvisioner) DeleteAutoscalingNode(ctx context.Context, clusterName, groupName string) error {
	if _, err := p.AutoscalingNodeName(ctx, clusterName, groupName); err != nil {
		return err
	}
	request, err := p.readMutableClusterMetadata(clusterName)
	if err != nil {
		return err
	}
	release, err := p.reserveRuntimeName(clusterRuntimeName(request), request.Name)
	if err != nil {
		return err
	}
	defer release()
	machineName := clusterMachineName(clusterRuntimeName(request), groupName, 1)
	instances, err := p.vms.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, instance := range instances {
		if instance.Name == machineName {
			if err := p.vms.Delete(ctx, machineName, true); err != nil {
				return err
			}
			break
		}
	}
	request.NodeGroups = slices.DeleteFunc(request.NodeGroups, func(group NodeGroupSpec) bool { return group.Name == groupName })
	return p.writeClusterMetadata(request)
}

// AutoscalingNodeExists reports actual VM existence, not just a claim ledger
// entry. It refuses a same-named VM owned by somebody else.
func (p *ClusterProvisioner) AutoscalingNodeExists(ctx context.Context, clusterName, groupName string) (bool, error) {
	if _, err := p.AutoscalingNodeName(ctx, clusterName, groupName); err != nil {
		return false, err
	}
	request, err := p.readMutableClusterMetadata(clusterName)
	if err != nil {
		return false, err
	}
	name := clusterMachineName(clusterRuntimeName(request), groupName, 1)
	instances, err := p.vms.ListAll(ctx)
	if err != nil {
		return false, err
	}
	for _, instance := range instances {
		if instance.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// EnsureAutoscalingNode resumes interrupted worker installation as well as VM
// creation. A process may have exited after CreateNode but before writing group
// metadata; ordinary ScaleNodeGroup deliberately skips existing VMs. The actual
// controller release is authoritative even for legacy unpinned metadata.
func (p *ClusterProvisioner) EnsureAutoscalingNode(ctx context.Context, clusterName string, group NodeGroupSpec, version string) error {
	if group.Count != 1 {
		return fmt.Errorf("an autoscaling claim must own exactly one worker")
	}
	exists, err := p.AutoscalingNodeExists(ctx, clusterName, group.Name)
	if err != nil {
		return err
	}
	request, err := p.readMutableClusterMetadata(clusterName)
	if err != nil {
		return err
	}
	serverName := clusterMachineName(clusterRuntimeName(request), "server", 1)
	version, err = p.installedVMVersion(ctx, serverName, normalizeProvider(request.Provider))
	if err != nil {
		return fmt.Errorf("resolve controller release before provisioning autoscaled worker (no channel fallback): %w", err)
	}
	if err := p.ScaleNodeGroup(ctx, clusterName, group, version); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	request, err = p.readMutableClusterMetadata(clusterName)
	if err != nil {
		return err
	}
	release, err := p.reserveRuntimeName(clusterRuntimeName(request), request.Name)
	if err != nil {
		return err
	}
	defer release()
	machineName := clusterMachineName(clusterRuntimeName(request), group.Name, 1)
	health := []string{"sudo", "systemctl", "is-active", "k3s-agent"}
	if request.Provider == "k0s" {
		health = []string{"sudo", "k0s", "status"}
	}
	if _, err := p.vms.Exec(ctx, machineName, health, nil); err == nil {
		return nil
	}
	ip, err := p.vms.Exec(ctx, serverName, []string{"hostname", "-I"}, nil)
	if err != nil {
		return err
	}
	token, err := p.clusterToken(ctx, normalizeProvider(request.Provider), serverName)
	if err != nil {
		return err
	}
	return p.installVMKubernetes(ctx, normalizeProvider(request.Provider), machineName, version, false,
		firstField(ip), strings.TrimSpace(string(token)), group.Labels, group.Taints)
}
