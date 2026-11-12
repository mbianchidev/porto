package kubernetes

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/filelock"
	"github.com/mbianchidev/porto/internal/providers"
	"github.com/mbianchidev/porto/internal/runtimes"
)

const (
	appleNativeFormat = 1
	appleClusterLabel = "dev.porto.cluster"
	appleNodeLabel    = "dev.porto.node"
	appleNodeImage    = "docker.io/kindest/node:v1.35.5@sha256:ce977ae6d65918d0b58a5f8b5e940429c2ce42fa3a5619ec2bbc60b949c0ac95"
	appleShutdown     = 6 * time.Minute
	appleRecovery     = 15 * time.Minute
)

type AppleClusterState struct {
	Format         int                  `json:"format"`
	ID             string               `json:"id"`
	AppRoot        string               `json:"appRoot"`
	RuntimeVersion string               `json:"runtimeVersion"`
	Nodes          []AppleNode          `json:"nodes"`
	CAHash         string               `json:"caHash,omitempty"`
	Operation      *AppleLifecycle      `json:"operation,omitempty"`
	Health         *AppleHealthSnapshot `json:"health,omitempty"`
	Warnings       []string             `json:"warnings,omitempty"`
}

type AppleNode struct {
	ID                      string           `json:"id"`
	Identity                string           `json:"identity"`
	Role                    string           `json:"role"`
	Group                   string           `json:"group,omitempty"`
	Machine                 MachineSpec      `json:"machine"`
	CreatedAt               string           `json:"createdAt,omitempty"`
	Configuration           json.RawMessage  `json:"configuration,omitempty"`
	Storage                 AppleNodeStorage `json:"storage"`
	IPv4                    string           `json:"ipv4,omitempty"`
	BootID                  string           `json:"bootID,omitempty"`
	KubernetesUID           string           `json:"kubernetesUID,omitempty"`
	PodCIDR                 string           `json:"podCIDR,omitempty"`
	ShutdownGraceSeconds    int64            `json:"shutdownGraceSeconds,omitempty"`
	CriticalShutdownSeconds int64            `json:"criticalShutdownSeconds,omitempty"`
	Deleting                bool             `json:"deleting,omitempty"`
	Deleted                 bool             `json:"deleted,omitempty"`
}

type AppleNodeStorage struct {
	Directory   string          `json:"directory"`
	DirectoryID string          `json:"directoryID"`
	Rootfs      string          `json:"rootfs"`
	RootfsID    string          `json:"rootfsID"`
	Description json.RawMessage `json:"description"`
}

type AppleLifecycle struct {
	ID              string            `json:"id"`
	Goal            string            `json:"goal"`
	Stage           string            `json:"stage"`
	StartedAt       string            `json:"startedAt"`
	CompletedAt     string            `json:"completedAt,omitempty"`
	Nodes           map[string]string `json:"nodes"`
	Error           string            `json:"error,omitempty"`
	BaselineUnknown bool              `json:"baselineUnknown,omitempty"`
}

type ClusterCapabilities struct {
	NativeNodes   bool   `json:"nativeNodes,omitempty"`
	Start         bool   `json:"start"`
	Stop          bool   `json:"stop"`
	Restart       bool   `json:"restart"`
	ImageLoading  bool   `json:"imageLoading"`
	NodeScaling   bool   `json:"nodeScaling"`
	Upgrade       bool   `json:"upgrade"`
	LifecycleNote string `json:"lifecycleNote,omitempty"`
}

type appleSystem struct {
	Status string `json:"status"`
	Client struct {
		Version string `json:"version"`
	} `json:"client"`
	Server struct {
		Version string `json:"version"`
	} `json:"server"`
	Paths struct {
		AppRoot     string `json:"appRoot"`
		InstallRoot string `json:"installRoot"`
	} `json:"paths"`
}

type appleNativeContainer struct {
	ID            string          `json:"id"`
	Configuration json.RawMessage `json:"configuration"`
	Status        struct {
		State       string `json:"state"`
		StartedDate string `json:"startedDate"`
		Networks    []struct {
			IPv4Address string `json:"ipv4Address"`
		} `json:"networks"`
	} `json:"status"`
}

type appleNativeConfiguration struct {
	ID           string            `json:"id"`
	CreationDate string            `json:"creationDate"`
	Labels       map[string]string `json:"labels"`
}

func appleIdentity() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate native ownership identity: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func canonicalAppleJSON(data []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, errors.New("native configuration is null")
	}
	return json.Marshal(value)
}

func appleDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func appleNativeNodeIDs(request ClusterRequest) []string {
	if request.Apple == nil {
		return appleNodeNames(request)
	}
	result := make([]string, 0, len(request.Apple.Nodes))
	for _, node := range request.Apple.Nodes {
		if !node.Deleted {
			result = append(result, node.ID)
		}
	}
	return result
}

func appleCapabilities(request ClusterRequest) *ClusterCapabilities {
	if request.Apple == nil {
		return &ClusterCapabilities{
			LifecycleNote: "This existing cluster is owned by Apple's cluster plugin, which blocks node start. Porto will not silently migrate or rebuild it.",
		}
	}
	return &ClusterCapabilities{NativeNodes: true, Start: true, Stop: true, Restart: true, ImageLoading: true,
		LifecycleNote: "Porto manages each native node VM directly. Node scaling and Kubernetes-version upgrades are not implemented."}
}

func (p *ClusterProvisioner) LifecycleSummary(name string) (bool, string, error) {
	if !clusterNamePattern.MatchString(name) {
		return false, "", fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	request, err := p.readClusterMetadata(name)
	if err != nil {
		return false, "", err
	}
	return request.Provider == "apple" && request.Apple != nil, appleLifecycleMessage(request.Apple), nil
}

func (p *ClusterProvisioner) appleSystem(ctx context.Context, start bool) (appleSystem, error) {
	if err := p.requireAppleHost(); err != nil {
		return appleSystem{}, err
	}
	read := func() (appleSystem, error) {
		probe, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		output, err := p.runApple(probe, runtimes.Command{
			Args: []string{"system", "status", "--format", "json"}, StdoutOnly: true,
		})
		if err != nil {
			return appleSystem{}, runtimes.CommandError("inspect native container services", output, err)
		}
		var system appleSystem
		if err := json.Unmarshal(output, &system); err != nil {
			return system, fmt.Errorf("decode native container services: %w", err)
		}
		if system.Status != "running" || !filepath.IsAbs(system.Paths.AppRoot) {
			return system, errors.New("native container services are unavailable or have no absolute state root")
		}
		for _, version := range []string{system.Client.Version, system.Server.Version} {
			if err := providers.ValidateAppleContainerVersion("container CLI version " + version); err != nil {
				return system, err
			}
		}
		if system.Client.Version != system.Server.Version {
			return system, errors.New("native client and server versions differ; arrange an explicit runtime update without stopping unrelated workloads")
		}
		return system, nil
	}
	system, err := read()
	if err == nil || !start {
		return system, err
	}
	startContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	output, startErr := p.runApple(startContext, runtimes.Command{
		Args: []string{"system", "start", "--disable-kernel-install"},
	})
	if startErr != nil {
		return system, errors.Join(err, runtimes.CommandError("start native services without stopping containers", output, startErr))
	}
	return read()
}

func (p *ClusterProvisioner) inspectAppleNodes(ctx context.Context, ids []string) (map[string]appleNativeContainer, error) {
	if len(ids) == 0 {
		return nil, errors.New("native ownership has no nodes")
	}
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	output, err := p.runApple(probe, runtimes.Command{
		Args: append([]string{"inspect"}, ids...), StdoutOnly: true,
	})
	if err != nil {
		return nil, runtimes.CommandError("inspect exact native node identities", output, err)
	}
	var containers []appleNativeContainer
	if err := json.Unmarshal(output, &containers); err != nil {
		return nil, fmt.Errorf("decode native node identities: %w", err)
	}
	if containers == nil || len(containers) != len(ids) {
		return nil, errors.New("native node inventory is incomplete; no replacement nodes will be created")
	}
	result := make(map[string]appleNativeContainer, len(containers))
	for _, container := range containers {
		if _, duplicate := result[container.ID]; duplicate || !slices.Contains(ids, container.ID) {
			return nil, errors.New("native inventory returned an unexpected or duplicate node identity")
		}
		result[container.ID] = container
	}
	return result, nil
}

func (p *ClusterProvisioner) ownedAppleNodes(ctx context.Context, request ClusterRequest) (map[string]appleNativeContainer, error) {
	return p.ownedAppleNodesForAction(ctx, request, false)
}

func (p *ClusterProvisioner) ownedAppleNodesForAction(ctx context.Context, request ClusterRequest, deleting bool) (map[string]appleNativeContainer, error) {
	if err := p.requireAppleHost(); err != nil {
		return nil, err
	}
	if request.Apple == nil || request.Apple.Format != appleNativeFormat || request.Apple.ID == "" || len(request.Apple.Nodes) == 0 {
		return nil, errors.New("native cluster ownership is missing or uses an unsupported format")
	}
	ids := appleNativeNodeIDs(request)
	if len(ids) == 0 && deleting {
		return map[string]appleNativeContainer{}, nil
	}
	containers, err := p.inspectAppleNodes(ctx, ids)
	if err != nil {
		return nil, err
	}
	controlPlanes := 0
	for _, node := range request.Apple.Nodes {
		if node.Deleted {
			continue
		}
		container := containers[node.ID]
		var configuration appleNativeConfiguration
		if err := json.Unmarshal(container.Configuration, &configuration); err != nil {
			return nil, fmt.Errorf("decode node %s ownership: %w", node.ID, err)
		}
		if configuration.ID != node.ID || configuration.Labels[appleClusterLabel] != request.Apple.ID ||
			configuration.Labels[appleNodeLabel] != node.Identity || node.CreatedAt == "" ||
			configuration.CreationDate != node.CreatedAt {
			return nil, fmt.Errorf("native node %s ownership changed; refusing to operate on a replacement", node.ID)
		}
		actual, err := canonicalAppleJSON(container.Configuration)
		if err != nil {
			return nil, err
		}
		expected, err := canonicalAppleJSON(node.Configuration)
		if err != nil || !bytes.Equal(actual, expected) {
			return nil, fmt.Errorf("native node %s configuration ownership changed", node.ID)
		}
		if node.Role == "control-plane" {
			controlPlanes++
		} else if node.Role != "worker" {
			return nil, fmt.Errorf("native node %s has an unknown topology role", node.ID)
		}
		var storageErr error
		if deleting && node.Storage.RootfsID == "" && container.Status.State == "stopped" {
			storageErr = p.verifyUnbootedAppleStorage(request.Apple.AppRoot, node)
		} else {
			storageErr = p.verifyAppleStorage(request.Apple.AppRoot, node)
		}
		if storageErr != nil {
			return nil, fmt.Errorf("native node %s storage ownership: %w", node.ID, storageErr)
		}
	}
	if controlPlanes != 1 {
		return nil, errors.New("native topology must contain exactly one control plane; HA membership changes are not supported")
	}
	return containers, nil
}

func (p *ClusterProvisioner) verifyUnbootedAppleStorage(appRoot string, node AppleNode) error {
	directory := filepath.Join(appRoot, "containers", node.ID)
	identity, err := p.appleFileIdentity(directory)
	if err != nil || directory != node.Storage.Directory || identity != node.Storage.DirectoryID || identity == "" {
		return errors.Join(errors.New("unbooted native creation directory ownership changed"), err)
	}
	for _, file := range []string{"rootfs.ext4", "rootfs.json"} {
		if _, err := os.Lstat(filepath.Join(directory, file)); !errors.Is(err, os.ErrNotExist) {
			return errors.New("native creation has unrecorded backing data; refusing to discard it")
		}
	}
	return nil
}

func (p *ClusterProvisioner) captureAppleStorage(appRoot, id string) (AppleNodeStorage, error) {
	directory := filepath.Join(appRoot, "containers", id)
	directoryID, err := p.appleFileIdentity(directory)
	if err != nil {
		return AppleNodeStorage{}, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return AppleNodeStorage{}, err
	}
	defer root.Close()
	info, err := root.Lstat("rootfs.json")
	if err != nil || !info.Mode().IsRegular() {
		return AppleNodeStorage{}, errors.New("native writable filesystem descriptor is missing or not a regular file")
	}
	description, err := root.ReadFile("rootfs.json")
	if err != nil {
		return AppleNodeStorage{}, err
	}
	var descriptor struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
		Type        struct {
			Block *struct {
				Format string `json:"format"`
			} `json:"block"`
		} `json:"type"`
	}
	if err := json.Unmarshal(description, &descriptor); err != nil {
		return AppleNodeStorage{}, err
	}
	expected := filepath.Join(directory, "rootfs.ext4")
	if descriptor.Source != expected || descriptor.Destination != "/" || descriptor.Type.Block == nil || descriptor.Type.Block.Format != "ext4" {
		return AppleNodeStorage{}, errors.New("unsupported native writable filesystem layout; refusing to infer storage ownership")
	}
	identity, err := p.appleFileIdentity(expected)
	if err != nil {
		return AppleNodeStorage{}, err
	}
	canonical, err := canonicalAppleJSON(description)
	if err != nil {
		return AppleNodeStorage{}, err
	}
	return AppleNodeStorage{Directory: directory, DirectoryID: directoryID, Rootfs: expected, RootfsID: identity, Description: canonical}, nil
}

func (p *ClusterProvisioner) verifyAppleStorage(appRoot string, node AppleNode) error {
	if node.Storage.RootfsID == "" || node.Storage.DirectoryID == "" {
		return errors.New("persistent filesystem identity is missing")
	}
	actual, err := p.captureAppleStorage(appRoot, node.ID)
	if err != nil {
		return err
	}
	expectedDescription, err := canonicalAppleJSON(node.Storage.Description)
	if err != nil {
		return fmt.Errorf("decode recorded native backing ownership: %w", err)
	}
	if actual.Directory != node.Storage.Directory || actual.DirectoryID != node.Storage.DirectoryID ||
		actual.Rootfs != node.Storage.Rootfs || actual.RootfsID != node.Storage.RootfsID ||
		!bytes.Equal(actual.Description, expectedDescription) {
		return errors.New("persistent filesystem was replaced or reconfigured; existing data will not be discarded")
	}
	return nil
}

func appleNodeIPv4(container appleNativeContainer) (string, error) {
	for _, network := range container.Status.Networks {
		prefix, err := netip.ParsePrefix(network.IPv4Address)
		if err == nil && prefix.Addr().Is4() && !prefix.Addr().IsUnspecified() && !prefix.Addr().IsLoopback() {
			return prefix.Addr().String(), nil
		}
	}
	return "", fmt.Errorf("native node %s has no usable IPv4 address", container.ID)
}

func (p *ClusterProvisioner) appleGuest(ctx context.Context, node string, timeout time.Duration, input []byte, args ...string) ([]byte, error) {
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	arguments := []string{"exec"}
	if input != nil {
		arguments = append(arguments, "--interactive")
	}
	arguments = append(append(arguments, node), args...)
	output, err := p.runApple(commandContext, runtimes.Command{
		Args: arguments, Stdin: input, StdoutOnly: true,
	})
	if err != nil || commandContext.Err() != nil {
		return nil, provisioningCommandError(commandContext, "native node "+node, output, err)
	}
	return output, nil
}

func (p *ClusterProvisioner) appleReadFile(ctx context.Context, node, file string) ([]byte, error) {
	return p.appleGuest(ctx, node, 15*time.Second, nil, "/bin/cat", file)
}

func (p *ClusterProvisioner) appleWriteFile(ctx context.Context, node, file string, contents []byte) error {
	const script = `set -eu
umask 077
directory="$(dirname -- "$1")"
mkdir -p "$directory"
temporary="$(mktemp "$directory/.porto-write-XXXXXX")"
trap 'rm -f -- "$temporary"' EXIT
cat > "$temporary"
chmod 0600 "$temporary"
mv -- "$temporary" "$1"
trap - EXIT`
	_, err := p.appleGuest(ctx, node, 30*time.Second, contents, "/bin/sh", "-c", script, "sh", file)
	return err
}

func waitApple(ctx context.Context, description string, probe func(context.Context) error) error {
	var last error
	for {
		if err := context.Cause(ctx); err != nil {
			return errors.Join(fmt.Errorf("%s: %w", description, err), last)
		}
		if last = probe(ctx); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("%s: %w", description, context.Cause(ctx)), last)
		case <-time.After(time.Second):
		}
	}
}

func appleLifecycleMessage(state *AppleClusterState) string {
	if state == nil {
		return ""
	}
	return strings.Join(state.Warnings, " ")
}

func (p *ClusterProvisioner) appleOperationLockPath(identity string) string {
	return filepath.Join(p.kubeconfigRoot, ".apple-operation-"+config.KubernetesClusterFileToken(identity)+".lock")
}

func (p *ClusterProvisioner) appleOperationActive(identity string) (bool, error) {
	lease, err := filelock.TryAcquireExisting(p.appleOperationLockPath(identity))
	if filelock.IsBusy(err) {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect native operation ownership: %w", err)
	}
	return false, lease.Close()
}

func (p *ClusterProvisioner) lockAppleOperation(ctx context.Context, request *ClusterRequest, action string) (func(), error) {
	if request.Provider != "apple" || request.Apple == nil {
		return func() {}, nil
	}
	identity := request.Apple.ID
	lease, err := filelock.Acquire(ctx, p.appleOperationLockPath(identity))
	if err != nil {
		return nil, fmt.Errorf("serialize native cluster %s operation %s: %w", request.Name, action, err)
	}
	var current ClusterRequest
	switch action {
	case "start", "stop", "restart":
		current, err = p.readAppleLifecycleMetadata(request.Name)
	case "delete":
		current, err = p.readClusterMetadata(request.Name)
	default:
		current, err = p.readMutableClusterMetadata(request.Name)
	}
	if err == nil && (current.Provider != "apple" || current.Apple == nil || current.Apple.ID != identity) {
		err = errors.New("native cluster ownership changed while waiting for its operation lock")
	}
	if err != nil {
		return nil, errors.Join(err, lease.Close())
	}
	*request = current
	return func() {
		if err := lease.Close(); err != nil {
			log.Printf("release native cluster %s operation lock: %v", request.Name, err)
		}
	}, nil
}
