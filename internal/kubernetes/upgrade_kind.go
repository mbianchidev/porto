package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type kindUpgradeBackup struct {
	Original    ClusterRequest   `json:"original"`
	Replacement ClusterRequest   `json:"replacement"`
	Kubeconfig  []byte           `json:"kubeconfig"`
	Objects     []map[string]any `json:"objects"`
	Volumes     []kindVolumeCopy `json:"volumes"`
	Images      []kindImageCopy  `json:"images,omitempty"`
	Nodes       []upgradeNode    `json:"nodes"`
}

type kindVolumeCopy struct {
	Name   string `json:"name"`
	Node   string `json:"node"`
	Path   string `json:"path"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

func (p *ClusterProvisioner) upgradeKind(ctx context.Context, spec *ClusterRequest, target string) (UpgradeResult, error) {
	backup, err := p.prepareKindUpgrade(ctx, *spec, target)
	if err != nil {
		return UpgradeResult{}, err
	}
	directory, err := os.MkdirTemp(filepath.Join(p.kubeconfigRoot), ".upgrade-"+spec.Name+"-")
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("create private upgrade backup: %w", err)
	}
	if err := writeKindUpgradeBackup(directory, backup); err != nil {
		return UpgradeResult{}, err
	}
	spec.Upgrade.BackupPath = directory
	if err := p.writeClusterMetadata(*spec); err != nil {
		return UpgradeResult{}, err
	}
	releaseReplacement, err := p.reserveRuntimeName(clusterRuntimeName(backup.Replacement), spec.Name)
	if err != nil {
		return UpgradeResult{}, err
	}
	defer releaseReplacement()
	for index := range backup.Images {
		image := &backup.Images[index]
		image.File = fmt.Sprintf("images-%d.tar", index)
		image.SHA256, err = p.exportKindArchive(ctx, runtimes.Command{
			Name: "docker",
			Args: append([]string{"exec", image.Node, "ctr", "--namespace", "k8s.io", "images", "export", "--platform", image.Platform, "-"}, image.References...),
			Env:  p.portoDockerEnv(),
		}, filepath.Join(directory, image.File))
		if err != nil {
			return UpgradeResult{}, fmt.Errorf("save application images before replacement: %w", err)
		}
		if err := writeKindUpgradeBackup(directory, backup); err != nil {
			return UpgradeResult{}, err
		}
	}
	if err := p.kindUpgradePower(ctx, backup.Original, false); err != nil {
		return UpgradeResult{}, fmt.Errorf("stop original kind nodes; recovery backup is %s: %w", directory, err)
	}
	for index := range backup.Volumes {
		volume := &backup.Volumes[index]
		volume.File = fmt.Sprintf("volume-%d.tar", index)
		volume.SHA256, err = p.exportKindVolume(ctx, volume.Node, volume.Path, filepath.Join(directory, volume.File))
		if err != nil {
			return UpgradeResult{}, fmt.Errorf("back up volume %s; original data remains in stopped nodes: %w", volume.Name, err)
		}
		if err := writeKindUpgradeBackup(directory, backup); err != nil {
			return UpgradeResult{}, err
		}
	}
	if _, err := p.createKind(ctx, backup.Replacement); err != nil {
		return UpgradeResult{}, fmt.Errorf("create upgraded kind cluster; use upgrade-recover to restart the original cluster: %w", err)
	}
	// createKind publishes its spec; retain the transaction marker until restoration is verified.
	replacement := backup.Replacement
	replacement.Phase = "upgrading"
	replacement.Upgrade = spec.Upgrade
	if err := p.writeClusterMetadata(replacement); err != nil {
		return UpgradeResult{}, err
	}
	for _, image := range backup.Images {
		filename := filepath.Join(directory, image.File)
		if err := verifyUpgradeArchive(filename, image.SHA256); err != nil {
			return UpgradeResult{}, err
		}
		output, err := p.runner.Run(ctx, runtimes.Command{Name: "kind", Args: []string{
			"load", "image-archive", filename, "--name", "porto-" + clusterRuntimeName(replacement),
		}, Env: p.portoDockerEnv()})
		if err != nil {
			return UpgradeResult{}, runtimes.CommandError("restore application images", output, err)
		}
	}
	newNodes := kindNodeNames(replacement)
	oldNodes := kindNodeNames(backup.Original)
	renames := make(map[string]string, len(oldNodes))
	for index, node := range oldNodes {
		renames[node] = newNodes[index]
	}
	for _, volume := range backup.Volumes {
		output, err := p.runner.Run(ctx, runtimes.Command{
			Name: "docker", Args: []string{"exec", renames[volume.Node], "mkdir", "-p", path.Dir(volume.Path)}, Env: p.portoDockerEnv(),
		})
		if err != nil {
			return UpgradeResult{}, runtimes.CommandError("prepare persistent volume directory", output, err)
		}
	}
	if err := p.kindUpgradePower(ctx, replacement, false); err != nil {
		return UpgradeResult{}, err
	}
	for _, volume := range backup.Volumes {
		if err := p.restoreKindVolume(ctx, renames[volume.Node], directory, volume); err != nil {
			return UpgradeResult{}, fmt.Errorf("restore persistent volume %s: %w", volume.Name, err)
		}
	}
	if err := p.kindUpgradePower(ctx, replacement, true); err != nil {
		return UpgradeResult{}, err
	}
	if err := p.waitForClusterAPI(ctx, p.clusterKubeconfigPath(spec.Name), clusterContextName(replacement)); err != nil {
		return UpgradeResult{}, err
	}
	for _, node := range newNodes {
		if err := p.waitForUpgradedNode(ctx, replacement, node, target); err != nil {
			return UpgradeResult{}, err
		}
	}
	run := p.addonCommandRunner(ctx, p.clusterKubeconfigPath(spec.Name), clusterContextName(replacement))
	for _, node := range backup.Nodes {
		labels := node.Metadata.Labels
		if labels != nil {
			labels["kubernetes.io/hostname"] = renames[node.Metadata.Name]
		}
		patch, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"labels": labels},
			"spec":     map[string]any{"taints": node.Spec.Taints, "unschedulable": node.Spec.Unschedulable},
		})
		if err != nil {
			return UpgradeResult{}, err
		}
		if _, err := run("restore node scheduling configuration", nil, "patch", "node", renames[node.Metadata.Name], "--type=merge", "-p", string(patch)); err != nil {
			return UpgradeResult{}, err
		}
	}
	if err := p.restoreKindObjects(ctx, replacement, backup.Objects, renames); err != nil {
		return UpgradeResult{}, fmt.Errorf("restore applications; original cluster and backup are retained at %s: %w", directory, err)
	}
	replacement.Phase, replacement.Error = "", ""
	if err := p.writeClusterMetadata(replacement); err != nil {
		return UpgradeResult{}, err
	}
	*spec = replacement
	return UpgradeResult{
		Version: target, BackupPath: directory,
		Message: "kind replacement is Ready. Persistent volumes reattached and applications restored. Original stopped nodes and a private recovery backup are retained.",
	}, nil
}

func writeKindUpgradeBackup(directory string, backup kindUpgradeBackup) error {
	data, err := json.Marshal(backup)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(directory, "recovery.json"), data)
}

func (p *ClusterProvisioner) prepareKindUpgrade(ctx context.Context, spec ClusterRequest, target string) (kindUpgradeBackup, error) {
	backup := kindUpgradeBackup{Original: spec, Replacement: spec}
	backup.Original.Phase, backup.Original.Error, backup.Original.Upgrade = "", "", nil
	backup.Replacement.Version = target
	backup.Replacement.Phase, backup.Replacement.Error = "", ""
	backup.Replacement.RuntimeName = suffixedRuntimeName(spec.Name, int(time.Now().UnixNano()%1_000_000_000))
	if err := p.ensureRuntimeNameAvailable(backup.Replacement.RuntimeName, spec.Name); err != nil {
		return backup, err
	}
	if err := p.ensureKindRuntimeAbsent(ctx, backup.Replacement); err != nil {
		return backup, err
	}
	data, err := os.ReadFile(p.clusterKubeconfigPath(spec.Name))
	if err != nil {
		return backup, fmt.Errorf("save original kubeconfig: %w", err)
	}
	backup.Kubeconfig = data
	backup.Nodes, err = p.upgradeNodes(ctx, spec)
	if err != nil {
		return backup, err
	}
	run := p.addonCommandRunner(ctx, p.clusterKubeconfigPath(spec.Name), clusterContextName(spec))
	output, err := run("discover restorable Kubernetes resources", nil, "api-resources", "--verbs=list,create", "-o", "name")
	if err != nil {
		return backup, err
	}
	resources := strings.Fields(string(output))
	if len(resources) == 0 {
		return backup, errors.New("Kubernetes API discovery returned no restorable resources")
	}
	sort.Strings(resources)
	images := make(map[string]struct{})
	for _, resource := range resources {
		if skipUpgradeResource(resource) {
			continue
		}
		output, err := run("export "+resource+" for upgrade", nil, "get", resource, "--all-namespaces", "-o", "json")
		if err != nil {
			return backup, err
		}
		var list struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(output, &list); err != nil {
			return backup, fmt.Errorf("decode exported %s: %w", resource, err)
		}
		for _, object := range list.Items {
			metadata, _ := object["metadata"].(map[string]any)
			namespace, _ := metadata["namespace"].(string)
			if !upgradeSystemNamespace(namespace) {
				collectUpgradeImages(object["spec"], images)
			}
			if keepUpgradeObject(object) {
				if err := validateUpgradePodVolumes(object); err != nil {
					return backup, err
				}
				if object["kind"] == "PersistentVolume" {
					volume, err := kindVolumeForUpgrade(object, kindNodeNames(spec))
					if err != nil {
						return backup, err
					}
					backup.Volumes = append(backup.Volumes, volume)
				}
				cleanUpgradeObject(object)
				backup.Objects = append(backup.Objects, object)
			}
		}
	}
	sort.SliceStable(backup.Objects, func(i, j int) bool {
		return upgradeObjectPriority(backup.Objects[i]) < upgradeObjectPriority(backup.Objects[j])
	})
	backup.Images, err = p.findKindUpgradeImages(ctx, spec, images)
	if err != nil {
		return backup, err
	}
	return backup, nil
}

func skipUpgradeResource(resource string) bool {
	base, _, _ := strings.Cut(resource, ".")
	return slices.Contains([]string{
		"nodes", "events", "endpoints", "endpointslices", "leases", "replicasets",
		"controllerrevisions", "componentstatuses", "bindings", "tokenreviews",
		"subjectaccessreviews", "selfsubjectaccessreviews", "selfsubjectrulesreviews",
		"localsubjectaccessreviews", "certificatesigningrequests", "csistoragecapacities",
		"volumeattachments", "flowschemas", "prioritylevelconfigurations",
		"nodeclaims", "nodepools", "portonodeclasses",
	}, base)
}

func upgradeSystemNamespace(namespace string) bool {
	return slices.Contains([]string{"kube-system", "kube-public", "kube-node-lease", "local-path-storage", "envoy-gateway-system"}, namespace)
}

func keepUpgradeObject(object map[string]any) bool {
	metadata, _ := object["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	namespace, _ := metadata["namespace"].(string)
	if upgradeSystemNamespace(namespace) || (object["kind"] == "Namespace" && upgradeSystemNamespace(name)) {
		return false
	}
	if name == "kube-root-ca.crt" || (object["kind"] == "Service" && namespace == "default" && name == "kubernetes") {
		return false
	}
	if namespace == "" && (object["kind"] == "ClusterRole" || object["kind"] == "ClusterRoleBinding") && strings.HasPrefix(name, "system:") {
		return false
	}
	if object["kind"] == "Secret" && object["type"] == "kubernetes.io/service-account-token" {
		return false
	}
	owners, _ := metadata["ownerReferences"].([]any)
	if len(owners) > 0 && (object["kind"] == "Pod" || object["kind"] == "Job") {
		return false
	}
	if object["kind"] == "Job" {
		status, _ := object["status"].(map[string]any)
		conditions, _ := status["conditions"].([]any)
		for _, raw := range conditions {
			condition, _ := raw.(map[string]any)
			if condition["status"] == "True" && (condition["type"] == "Complete" || condition["type"] == "Failed") {
				return false
			}
		}
	}
	return true
}

func cleanUpgradeObject(object map[string]any) {
	delete(object, "status")
	metadata, _ := object["metadata"].(map[string]any)
	if owners, ok := metadata["ownerReferences"].([]any); ok && len(owners) > 0 {
		object["portoUpgradeOwners"] = owners
	}
	for _, field := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds", "managedFields", "ownerReferences", "finalizers"} {
		delete(metadata, field)
	}
	annotations, _ := metadata["annotations"].(map[string]any)
	for _, key := range []string{"kubectl.kubernetes.io/last-applied-configuration", "pv.kubernetes.io/bind-completed", "pv.kubernetes.io/bound-by-controller", "volume.kubernetes.io/selected-node"} {
		delete(annotations, key)
	}
	spec, _ := object["spec"].(map[string]any)
	if object["kind"] == "Service" {
		for _, field := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy", "healthCheckNodePort"} {
			if field == "clusterIP" && spec[field] == "None" {
				continue
			}
			delete(spec, field)
		}
	}
	if object["kind"] == "PersistentVolume" {
		claim, _ := spec["claimRef"].(map[string]any)
		for _, key := range []string{"uid", "resourceVersion"} {
			delete(claim, key)
		}
	}
	if object["kind"] == "Job" {
		delete(spec, "selector")
		template, _ := spec["template"].(map[string]any)
		meta, _ := template["metadata"].(map[string]any)
		labels, _ := meta["labels"].(map[string]any)
		for _, key := range []string{"controller-uid", "batch.kubernetes.io/controller-uid", "job-name", "batch.kubernetes.io/job-name"} {
			delete(labels, key)
		}
	}
}

func upgradeObjectPriority(object map[string]any) int {
	switch object["kind"] {
	case "CustomResourceDefinition":
		return 0
	case "Namespace":
		return 1
	case "StorageClass", "ClusterRole", "ClusterRoleBinding", "PriorityClass", "RuntimeClass":
		return 2
	case "PersistentVolume":
		return 3
	case "Secret", "ConfigMap", "ServiceAccount", "Role", "RoleBinding":
		return 4
	case "PersistentVolumeClaim":
		return 5
	case "Deployment", "StatefulSet", "DaemonSet", "Pod", "Job", "CronJob":
		return 8
	default:
		return 6
	}
}

func kindVolumeForUpgrade(object map[string]any, nodes []string) (kindVolumeCopy, error) {
	metadata, _ := object["metadata"].(map[string]any)
	volume := kindVolumeCopy{Name: fmt.Sprint(metadata["name"])}
	spec, _ := object["spec"].(map[string]any)
	for _, key := range []string{"hostPath", "local"} {
		source, _ := spec[key].(map[string]any)
		if value, ok := source["path"].(string); ok {
			volume.Path = value
		}
	}
	if volume.Path == "" || !strings.HasPrefix(path.Clean(volume.Path), "/var/local-path-provisioner/") || path.Clean(volume.Path) != volume.Path {
		return volume, fmt.Errorf("volume %s is not a supported kind local-path volume; migrate or back up this storage before upgrading", volume.Name)
	}
	affinity, _ := spec["nodeAffinity"].(map[string]any)
	required, _ := affinity["required"].(map[string]any)
	terms, _ := required["nodeSelectorTerms"].([]any)
	if len(terms) == 1 {
		term, _ := terms[0].(map[string]any)
		expressions, _ := term["matchExpressions"].([]any)
		for _, raw := range expressions {
			expression, _ := raw.(map[string]any)
			values, _ := expression["values"].([]any)
			if expression["key"] == "kubernetes.io/hostname" && expression["operator"] == "In" && len(values) == 1 {
				volume.Node, _ = values[0].(string)
			}
		}
	}
	if !slices.Contains(nodes, volume.Node) {
		return volume, fmt.Errorf("volume %s does not identify exactly one Porto-owned kind node", volume.Name)
	}
	return volume, nil
}

func (p *ClusterProvisioner) kindUpgradePower(ctx context.Context, spec ClusterRequest, running bool) error {
	nodes := kindNodeNames(spec)
	action := "start"
	if !running {
		action = "stop"
		slices.Reverse(nodes)
	}
	var failures []error
	for _, node := range nodes {
		output, err := p.runner.Run(ctx, runtimes.Command{Name: "docker", Args: []string{action, node}, Env: p.portoDockerEnv()})
		if err != nil {
			failures = append(failures, runtimes.CommandError(action+" kind node "+node, output, err))
		}
	}
	return errors.Join(failures...)
}

func (p *ClusterProvisioner) exportKindVolume(ctx context.Context, node, source, destination string) (string, error) {
	return p.exportKindArchive(ctx, runtimes.Command{Name: "docker", Args: []string{"cp", "-a", node + ":" + source, "-"}, Env: p.portoDockerEnv()}, destination)
}

func (p *ClusterProvisioner) exportKindArchive(ctx context.Context, command runtimes.Command, destination string) (string, error) {
	runner, ok := p.runner.(interface {
		RunStreaming(context.Context, runtimes.Command, func(runtimes.OutputChunk) error) ([]byte, error)
	})
	if !ok {
		return "", errors.New("kind volume backup requires a streaming runtime runner")
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	var size int64
	var diagnostic []byte
	_, runErr := runner.RunStreaming(ctx, command, func(chunk runtimes.OutputChunk) error {
		if chunk.Stream != "stdout" {
			if remaining := 64*1024 - len(diagnostic); remaining > 0 {
				diagnostic = append(diagnostic, chunk.Data[:min(remaining, len(chunk.Data))]...)
			}
			return nil
		}
		size += int64(len(chunk.Data))
		if size > 2<<30 {
			return errors.New("volume archive exceeds the Docker API 2 GiB restore limit; original volume remains untouched")
		}
		_, err := io.MultiWriter(file, hash).Write(chunk.Data)
		return err
	})
	err = errors.Join(runErr, file.Sync(), file.Close())
	if err != nil {
		return "", runtimes.CommandError("archive persistent volume or image", diagnostic, err)
	}
	if size == 0 {
		return "", errors.New("persistent volume backup was empty")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyUpgradeArchive(filename, checksum string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if checksum == "" || hex.EncodeToString(hash.Sum(nil)) != checksum {
		return fmt.Errorf("upgrade backup checksum mismatch: %s", filename)
	}
	return nil
}

func (p *ClusterProvisioner) restoreKindVolume(ctx context.Context, node, directory string, volume kindVolumeCopy) error {
	file, err := os.Open(filepath.Join(directory, volume.File))
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if volume.SHA256 == "" || hex.EncodeToString(hash.Sum(nil)) != volume.SHA256 {
		return fmt.Errorf("backup checksum mismatch for volume %s", volume.Name)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// docker cp can restore stopped nodes, but the destination parent must exist.
	output, err := p.runner.Run(ctx, runtimes.Command{Name: "docker", Args: []string{"cp", "-a", "-", node + ":" + path.Dir(volume.Path)}, Env: p.portoDockerEnv(), StdinReader: file})
	if err != nil {
		return runtimes.CommandError("reattach persistent volume data", output, err)
	}
	return nil
}

func (p *ClusterProvisioner) restoreKindObjects(ctx context.Context, spec ClusterRequest, objects []map[string]any, renames map[string]string) error {
	run := p.addonCommandRunner(ctx, p.clusterKubeconfigPath(spec.Name), clusterContextName(spec))
	owners := make(map[int][]any)
	for index, object := range objects {
		rewriteUpgradeNodeReferences(object, renames)
		owners[index], _ = object["portoUpgradeOwners"].([]any)
		delete(object, "portoUpgradeOwners")
		data, err := json.Marshal(object)
		if err != nil {
			return err
		}
		if _, err := run("restore "+fmt.Sprint(object["kind"]), data, "apply", "--server-side", "--field-manager=porto-upgrade", "-f", "-"); err != nil {
			return err
		}
		if object["kind"] == "CustomResourceDefinition" {
			metadata, _ := object["metadata"].(map[string]any)
			if _, err := run("wait for restored CRD", nil, "wait", "--for=condition=Established", "crd/"+fmt.Sprint(metadata["name"]), "--timeout=2m"); err != nil {
				return err
			}
		}
	}
	for index, references := range owners {
		if len(references) == 0 {
			continue
		}
		object := objects[index]
		meta, _ := object["metadata"].(map[string]any)
		namespace, _ := meta["namespace"].(string)
		for _, raw := range references {
			owner, _ := raw.(map[string]any)
			kind, _ := owner["kind"].(string)
			apiVersion, _ := owner["apiVersion"].(string)
			resource := upgradeQualifiedKind(kind, apiVersion)
			ownerName, _ := owner["name"].(string)
			output, err := run("resolve restored application owner", nil, "-n", namespace, "get", resource, ownerName, "-o", "json")
			if err != nil {
				return err
			}
			var current struct {
				Metadata struct {
					UID string `json:"uid"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(output, &current); err != nil || current.Metadata.UID == "" {
				return fmt.Errorf("restored owner %s/%s has no valid UID: %w", resource, ownerName, errors.Join(err, errors.New("missing owner identity")))
			}
			owner["uid"] = current.Metadata.UID
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": references}})
		if err != nil {
			return err
		}
		kind, _ := object["kind"].(string)
		apiVersion, _ := object["apiVersion"].(string)
		if _, err := run("restore application ownership", nil, "-n", namespace, "patch", upgradeQualifiedKind(kind, apiVersion), fmt.Sprint(meta["name"]), "--type=merge", "-p", string(patch)); err != nil {
			return err
		}
	}
	for _, object := range objects {
		kind, _ := object["kind"].(string)
		meta, _ := object["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		namespace, _ := meta["namespace"].(string)
		switch kind {
		case "PersistentVolumeClaim":
			specification, _ := object["spec"].(map[string]any)
			if specification["volumeName"] != nil {
				if _, err := run("verify reattached volume", nil, "-n", namespace, "wait", "pvc/"+name, "--for=jsonpath={.status.phase}=Bound", "--timeout=5m"); err != nil {
					return err
				}
			}
		case "Deployment", "StatefulSet", "DaemonSet":
			if _, err := run("verify restored application", nil, "-n", namespace, "rollout", "status", strings.ToLower(kind)+"/"+name, "--timeout=5m"); err != nil {
				return err
			}
		case "Pod":
			if _, err := run("verify restored pod", nil, "-n", namespace, "wait", "pod/"+name, "--for=condition=Ready", "--timeout=5m"); err != nil {
				return err
			}
		}
	}
	return nil
}

func upgradeQualifiedKind(kind, apiVersion string) string {
	if group, _, found := strings.Cut(apiVersion, "/"); found {
		return kind + "." + group
	}
	return kind
}

func rewriteUpgradeNodeReferences(value any, renames map[string]string) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if (key == "nodeName" || key == "kubernetes.io/hostname") && renames[fmt.Sprint(child)] != "" {
				value[key] = renames[fmt.Sprint(child)]
			}
			if key == "values" && value["key"] == "kubernetes.io/hostname" {
				if entries, ok := child.([]any); ok {
					for index, entry := range entries {
						if replacement := renames[fmt.Sprint(entry)]; replacement != "" {
							entries[index] = replacement
						}
					}
				}
			}
			if key != "data" && key != "binaryData" && key != "stringData" {
				rewriteUpgradeNodeReferences(child, renames)
			}
		}
	case []any:
		for _, child := range value {
			rewriteUpgradeNodeReferences(child, renames)
		}
	}
}

func (p *ClusterProvisioner) RecoverUpgrade(ctx context.Context, name string, confirm bool) error {
	if !confirm || !clusterNamePattern.MatchString(name) {
		return errors.New("a valid cluster name and confirm=true are required for upgrade recovery")
	}
	spec, err := p.readClusterMetadata(name)
	if err != nil {
		return err
	}
	if (spec.Phase != "upgrade-failed" && spec.Phase != "upgrading") || spec.Upgrade == nil || spec.Upgrade.BackupPath == "" {
		return errors.New("cluster has no failed kind upgrade to recover; retry the target version for VM upgrades")
	}
	directory := spec.Upgrade.BackupPath
	if filepath.Dir(directory) != filepath.Clean(p.kubeconfigRoot) || !strings.HasPrefix(filepath.Base(directory), ".upgrade-"+name+"-") {
		return errors.New("upgrade recovery path is outside the cluster backup directory")
	}
	data, err := os.ReadFile(filepath.Join(directory, "recovery.json"))
	if err != nil {
		return fmt.Errorf("read upgrade recovery backup: %w", err)
	}
	var backup kindUpgradeBackup
	if err := json.Unmarshal(data, &backup); err != nil {
		return err
	}
	if backup.Original.Name != name || backup.Original.Provider != "kind" || backup.Replacement.Name != name ||
		!clusterNamePattern.MatchString(clusterRuntimeName(backup.Original)) || !clusterNamePattern.MatchString(clusterRuntimeName(backup.Replacement)) {
		return errors.New("upgrade backup does not match this managed cluster")
	}
	release, err := p.reserveRuntimeName(clusterRuntimeName(backup.Original), name)
	if err != nil {
		return err
	}
	defer release()
	output, err := p.runner.Run(ctx, runtimes.Command{Name: "kind", Args: []string{"get", "nodes", "--name", "porto-" + clusterRuntimeName(backup.Replacement)}, Env: p.portoDockerEnv()})
	if err != nil && !kindClusterMissing(output) {
		return runtimes.CommandError("inspect failed replacement", output, err)
	}
	if kindRuntimePresent(backup.Replacement, output) {
		if err := p.kindUpgradePower(ctx, backup.Replacement, false); err != nil {
			return fmt.Errorf("stop replacement before recovery: %w", err)
		}
	}
	if err := writeFileAtomic(p.clusterKubeconfigPath(name), backup.Kubeconfig); err != nil {
		return err
	}
	if err := p.kindUpgradePower(ctx, backup.Original, true); err != nil {
		return err
	}
	if err := p.waitForClusterAPI(ctx, p.clusterKubeconfigPath(name), clusterContextName(backup.Original)); err != nil {
		return err
	}
	if err := p.writeClusterMetadata(backup.Original); err != nil {
		return err
	}
	_, err = p.registerKubeconfig(ctx, name)
	return err
}
