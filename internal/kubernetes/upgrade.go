package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
)

const ClusterUpgradeTimeout = 2 * time.Hour

type UpgradeRequest struct {
	Version string `json:"version"`
	Confirm bool   `json:"confirm"`
}

type UpgradeState struct {
	Version           string          `json:"version"`
	StartedAt         string          `json:"startedAt"`
	BackupPath        string          `json:"backupPath,omitempty"`
	Error             string          `json:"error,omitempty"`
	InitiallyCordoned map[string]bool `json:"initiallyCordoned,omitempty"`
}

type UpgradeResult struct {
	Version    string `json:"version"`
	BackupPath string `json:"backupPath,omitempty"`
	Message    string `json:"message"`
}

var upgradeVersionPattern = regexp.MustCompile(`^v(1)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:\+(k3s)([1-9][0-9]*)|\+(k0s)\.(0|[1-9][0-9]*))?$`)

// Configuration returns only a validated, mutable Porto-owned cluster.
func (p *ClusterProvisioner) Configuration(name string) (ClusterRequest, error) {
	if !clusterNamePattern.MatchString(name) {
		return ClusterRequest{}, fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	spec, err := p.readMutableClusterMetadata(name)
	if err != nil {
		return ClusterRequest{}, err
	}
	if err := validateUpgradeOwnership(name, spec); err != nil {
		return ClusterRequest{}, err
	}
	return spec, nil
}

func (p *ClusterProvisioner) ManagedNames() ([]string, error) {
	entries, err := os.ReadDir(p.kubeconfigRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.kubeconfigRoot, entry.Name()))
		if err != nil {
			return nil, err
		}
		var spec ClusterRequest
		if err := json.Unmarshal(data, &spec); err != nil {
			return nil, err
		}
		if !clusterNamePattern.MatchString(spec.Name) || entry.Name() != config.KubernetesClusterFileToken(spec.Name)+".json" {
			return nil, errors.New("invalid managed cluster metadata identity")
		}
		names = append(names, spec.Name)
	}
	return names, nil
}

func validateUpgradeOwnership(name string, spec ClusterRequest) error {
	if spec.Name != name || normalizeProvider(spec.Provider) == "" || !clusterNamePattern.MatchString(clusterRuntimeName(spec)) {
		return errors.New("cluster ownership metadata does not match the requested cluster")
	}
	for _, group := range spec.NodeGroups {
		if !clusterNamePattern.MatchString(group.Name) || group.Count < 0 || group.Count > 32 {
			return errors.New("cluster ownership metadata contains an invalid node group")
		}
	}
	return nil
}

func parseUpgradeVersion(provider, version string) ([4]int, error) {
	var result [4]int
	match := upgradeVersionPattern.FindStringSubmatch(version)
	if match == nil || (provider == "k3s" && match[4] != "k3s") ||
		(provider == "k0s" && match[6] != "k0s") ||
		(provider == "kind" && (match[4] != "" || match[6] != "")) {
		return result, fmt.Errorf("use an explicit stable %s release version, not a channel or image reference: %q", provider, version)
	}
	fields := []string{match[1], match[2], match[3]}
	build := match[5] + match[7]
	if build == "" {
		build = "0"
	}
	for index, value := range append(fields, build) {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return result, fmt.Errorf("invalid Kubernetes version %q: %w", version, err)
		}
		result[index] = parsed
	}
	return result, nil
}

func validateUpgradeVersion(provider, current, target string) error {
	from, err := parseUpgradeVersion(provider, current)
	if err != nil {
		return fmt.Errorf("read installed version: %w", err)
	}
	to, err := parseUpgradeVersion(provider, target)
	if err != nil {
		return err
	}
	if slices.Compare(to[:], from[:]) < 0 {
		return fmt.Errorf("Kubernetes downgrade from %s to %s is not supported", current, target)
	}
	if to[1] > from[1]+1 {
		return fmt.Errorf("cannot skip Kubernetes minor versions: upgrade %s one minor release at a time", current)
	}
	return nil
}

type upgradeNode struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels,omitempty"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool             `json:"unschedulable"`
		Taints        []map[string]any `json:"taints,omitempty"`
	} `json:"spec"`
	Status struct {
		NodeInfo struct {
			Version string `json:"kubeletVersion"`
		} `json:"nodeInfo"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

func (n upgradeNode) ready() bool {
	for _, condition := range n.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			return true
		}
	}
	return false
}

func (p *ClusterProvisioner) upgradeNodes(ctx context.Context, spec ClusterRequest) ([]upgradeNode, error) {
	run := p.addonCommandRunner(ctx, p.clusterKubeconfigPath(spec.Name), clusterContextName(spec))
	output, err := run("inspect nodes for Kubernetes upgrade", nil, "get", "nodes", "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []upgradeNode `json:"items"`
	}
	if err := json.Unmarshal(output, &list); err != nil {
		return nil, fmt.Errorf("decode Kubernetes upgrade nodes: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, errors.New("no Kubernetes nodes found; start the cluster before upgrading")
	}
	return list.Items, nil
}

func (p *ClusterProvisioner) UpgradeCluster(ctx context.Context, name string, upgrade UpgradeRequest) (result UpgradeResult, err error) {
	if !upgrade.Confirm {
		return result, errors.New("confirm=true is required: upgrading interrupts workloads; kind replacement preserves supported persistent volumes and application manifests")
	}
	if !clusterNamePattern.MatchString(name) {
		return result, fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	spec, err := p.readClusterMetadata(name)
	if err != nil {
		return result, fmt.Errorf("read Kubernetes cluster ownership: %w", err)
	}
	if err := validateUpgradeOwnership(name, spec); err != nil {
		return result, err
	}
	spec.Provider = normalizeProvider(spec.Provider)
	if _, err := parseUpgradeVersion(spec.Provider, upgrade.Version); err != nil {
		return result, err
	}
	if spec.Phase != "" && spec.Phase != "upgrade-failed" && spec.Phase != "upgrading" {
		return result, fmt.Errorf("cannot upgrade cluster in state %s", spec.Phase)
	}
	release, err := p.reserveRuntimeName(clusterRuntimeName(spec), name)
	if err != nil {
		return result, err
	}
	defer release()
	if spec.Provider == "kind" && spec.Upgrade != nil && spec.Upgrade.BackupPath != "" && spec.Phase != "" {
		return result, fmt.Errorf("kind upgrade needs recovery from %s before another upgrade; use cluster upgrade-recover", spec.Upgrade.BackupPath)
	}
	nodes, err := p.upgradeNodes(ctx, spec)
	if err != nil {
		return result, err
	}
	if spec.Provider != "kind" {
		for index := range nodes {
			machine := nodes[index].Metadata.Name
			if spec.Provider == "k0s" {
				machine = strings.TrimPrefix(machine, "lima-")
			}
			if !slices.Contains(clusterNodeNames(spec), machine) {
				return result, fmt.Errorf("refuse to inspect unmanaged node %s", machine)
			}
			nodes[index].Status.NodeInfo.Version, err = p.installedVMVersion(ctx, machine, spec.Provider)
			if err != nil {
				return result, err
			}
		}
	}
	if err := validateUpgradeNodes(spec, nodes, upgrade.Version); err != nil {
		return result, err
	}
	if spec.Version != "" {
		if err := validateUpgradeVersion(spec.Provider, spec.Version, upgrade.Version); err != nil {
			return result, err
		}
	}
	if spec.Version == upgrade.Version && spec.Phase == "" {
		return result, fmt.Errorf("cluster already uses %s", upgrade.Version)
	}
	initiallyCordoned := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		initiallyCordoned[node.Metadata.Name] = node.Spec.Unschedulable
	}
	if spec.Phase != "" && spec.Upgrade != nil && spec.Upgrade.InitiallyCordoned != nil {
		initiallyCordoned = spec.Upgrade.InitiallyCordoned
	}
	spec.Phase = "upgrading"
	spec.Error = ""
	spec.Upgrade = &UpgradeState{Version: upgrade.Version, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), InitiallyCordoned: initiallyCordoned}
	if err := p.writeClusterMetadata(spec); err != nil {
		return result, err
	}
	defer func() {
		if err == nil {
			return
		}
		log.Printf("upgrade Kubernetes cluster %s to %s: %v", name, upgrade.Version, err)
		spec.Phase = "upgrade-failed"
		spec.Error = err.Error()
		if spec.Upgrade == nil {
			spec.Upgrade = &UpgradeState{Version: upgrade.Version, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		}
		spec.Upgrade.Error = err.Error()
		if spec.Provider == "kind" && spec.Upgrade.BackupPath == "" {
			spec.Phase = ""
			spec.Upgrade = nil
		}
		err = errors.Join(err, p.writeClusterMetadata(spec))
	}()
	if spec.Provider == "kind" {
		return p.upgradeKind(ctx, &spec, upgrade.Version)
	}
	if err = p.upgradeVMNodes(ctx, spec, upgrade.Version); err != nil {
		return result, err
	}
	spec.Version = upgrade.Version
	spec.Phase, spec.Error = "", ""
	spec.Upgrade = nil
	if err = p.writeClusterMetadata(spec); err != nil {
		return result, err
	}
	return UpgradeResult{Version: upgrade.Version, Message: "All nodes upgraded and Ready."}, nil
}

func validateUpgradeNodes(spec ClusterRequest, nodes []upgradeNode, target string) error {
	expected := clusterNodeNames(spec)
	if spec.Provider == "kind" {
		expected = kindNodeNames(spec)
	} else if spec.Provider == "k0s" {
		for index := range expected {
			expected[index] = "lima-" + expected[index]
		}
	}
	if len(nodes) != len(expected) {
		return errors.New("cluster node inventory differs from Porto ownership metadata; repair missing or unmanaged nodes before upgrading")
	}
	for _, node := range nodes {
		if !slices.Contains(expected, node.Metadata.Name) {
			return fmt.Errorf("refuse to upgrade unmanaged node %s", node.Metadata.Name)
		}
		// k0s kubelets may omit the distribution patch number.
		current := node.Status.NodeInfo.Version
		if spec.Provider == "k0s" && strings.HasSuffix(current, "+k0s") {
			current += ".0"
		}
		if err := validateUpgradeVersion(spec.Provider, current, target); err != nil {
			return fmt.Errorf("node %s: %w", node.Metadata.Name, err)
		}
		if !node.ready() {
			return fmt.Errorf("node %s is not Ready; restore node health before upgrading", node.Metadata.Name)
		}
	}
	return nil
}

func (p *ClusterProvisioner) upgradeVMNodes(ctx context.Context, spec ClusterRequest, target string) error {
	run := p.addonCommandRunner(ctx, p.clusterKubeconfigPath(spec.Name), clusterContextName(spec))
	for index, machine := range clusterNodeNames(spec) {
		nodeName := machine
		if spec.Provider == "k0s" {
			nodeName = "lima-" + machine
		}
		previouslyCordoned := spec.Upgrade.InitiallyCordoned[nodeName]
		if index > 0 {
			if _, err := run("drain worker before upgrade", nil, "drain", nodeName, "--ignore-daemonsets", "--delete-emptydir-data=false", "--timeout=5m"); err != nil {
				return err
			}
		}
		role := "agent"
		if index == 0 {
			role = "server"
		}
		if _, err := p.vms.Exec(ctx, machine, []string{"sudo", "sh", "-c", upgradeNodeScript, "porto-upgrade-node", target, spec.Provider, role}, nil); err != nil {
			return fmt.Errorf("upgrade node %s; retry the same target version after resolving the error: %w", nodeName, err)
		}
		if installed, err := p.installedVMVersion(ctx, machine, spec.Provider); err != nil || installed != target {
			return fmt.Errorf("verify upgraded binary on %s: installed %q, expected %q: %w", machine, installed, target, errors.Join(err, errors.New("distribution version verification failed")))
		}
		if index == 0 {
			if err := p.refreshVMKubeconfig(ctx, spec, machine); err != nil {
				return err
			}
			if _, err := p.registerKubeconfig(ctx, spec.Name); err != nil {
				return err
			}
		}
		if err := p.waitForUpgradedNode(ctx, spec, nodeName, target); err != nil {
			return err
		}
		if index > 0 && !previouslyCordoned {
			if _, err := run("uncordon upgraded worker", nil, "uncordon", nodeName); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *ClusterProvisioner) installedVMVersion(ctx context.Context, machine, provider string) (string, error) {
	output, err := p.vms.Exec(ctx, machine, []string{"sudo", provider, "--version"}, nil)
	if err != nil {
		return "", fmt.Errorf("read installed %s version on %s: %w", provider, machine, err)
	}
	for _, field := range strings.Fields(string(output)) {
		if _, err := parseUpgradeVersion(provider, field); err == nil {
			return field, nil
		}
	}
	return "", fmt.Errorf("node %s did not return a valid full %s release version", machine, provider)
}

func (p *ClusterProvisioner) waitForUpgradedNode(ctx context.Context, spec ClusterRequest, name, target string) error {
	wait, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	want := strings.Split(target, "+")[0]
	var lastErr error
	for {
		nodes, err := p.upgradeNodes(wait, spec)
		lastErr = err
		if err == nil {
			for _, node := range nodes {
				if node.Metadata.Name == name && node.ready() && strings.Split(node.Status.NodeInfo.Version, "+")[0] == want {
					return nil
				}
			}
			lastErr = fmt.Errorf("node %s has not reported Ready at %s", name, target)
		}
		select {
		case <-wait.Done():
			return errors.Join(lastErr, wait.Err())
		case <-time.After(time.Second):
		}
	}
}

const upgradeNodeScript = `set -eu
version=$1
provider=$2
role=$3
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported node architecture" >&2; exit 1 ;;
esac
directory=$(mktemp -d)
trap 'rm -f "$directory/binary" "$directory/checksums" "$directory/check"; rmdir "$directory"' EXIT
if [ "$provider" = k3s ]; then
  repository=k3s-io/k3s
  asset=k3s
  if [ "$arch" = arm64 ]; then asset=k3s-arm64; fi
  checksums=sha256sum-$arch.txt
else
  repository=k0sproject/k0s
  asset=k0s-$version-$arch
  checksums=sha256sums.txt
fi
url=https://github.com/$repository/releases/download/$version
curl --proto '=https' --tlsv1.2 -fsSL --retry 3 --max-time 300 "$url/$asset" -o "$directory/binary"
curl --proto '=https' --tlsv1.2 -fsSL --retry 3 --max-time 60 "$url/$checksums" -o "$directory/checksums"
checksum=$(awk -v asset="$asset" '$2 == asset || $2 == "*" asset {print $1}' "$directory/checksums")
test "${#checksum}" -eq 64
printf '%s  binary\n' "$checksum" > "$directory/check"
(cd "$directory" && sha256sum -c check)
chmod 755 "$directory/binary"
if [ "$provider" = k0s ]; then
  k0s stop
  install -m 755 "$directory/binary" /usr/local/bin/k0s
  k0s start
else
  install -m 755 "$directory/binary" /usr/local/bin/k3s
  service=k3s-agent
  if [ "$role" = server ]; then service=k3s; fi
  systemctl restart "$service"
fi
`
