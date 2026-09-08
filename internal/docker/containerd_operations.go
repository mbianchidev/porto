package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/api/types"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"
	"github.com/mbianchidev/porto/internal/runtimes"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const (
	defaultContainerStopTimeout     = 10 * time.Second
	containerdLinuxResourcesTypeURL = "types.containerd.io/opencontainers/runtime-spec/1/LinuxResources"
)

type containerOperations interface {
	Start(context.Context, string) error
	Stop(context.Context, string, int) error
	Kill(context.Context, string, uint32) error
	Pause(context.Context, string) error
	Resume(context.Context, string) error
	Restart(context.Context, string, int) error
	Wait(context.Context, string) (int, error)
	Rename(context.Context, string, string) error
	UpdateLabels(context.Context, string, map[string]string) error
	UpdateResources(context.Context, string, ContainerUpdate) error
	UpdateRestartPolicy(context.Context, string, string) error
	UpdateHealth(context.Context, string, *ContainerHealthcheck) error
	Checkpoint(context.Context, string, string) ([]string, error)
	Restore(context.Context, string, string) error
	Delete(context.Context, string, bool, bool) error
	Close() error
}

type containerOperationsConnector func(context.Context) (containerOperations, error)

type containerCreation interface {
	Create(context.Context, CreateContainerRequest) (string, error)
	Close() error
}

type containerCreationConnector func(context.Context) (containerCreation, error)

type execOperations interface {
	StartExec(context.Context, ExecRequest) (runtimes.Process, error)
	StartAttached(context.Context, string, bool) (runtimes.Process, error)
	Close() error
}

type execOperationsConnector func(context.Context) (execOperations, error)

type networkOperations interface {
	Connect(context.Context, string, string, []string) error
	Disconnect(context.Context, string, string, bool) error
	Close() error
}

type networkOperationsConnector func(context.Context) (networkOperations, error)

func (m *Manager) connectContainerCreation(ctx context.Context) (containerCreation, error) {
	runtimeClient, err := m.connectContainerRuntime(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnavailable) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: connect direct container creation: %v", ErrUnavailable, err)
	}
	creation, ok := runtimeClient.(containerCreation)
	if !ok {
		_ = runtimeClient.Close()
		return nil, fmt.Errorf("%w: connected containerd client does not support container creation", ErrUnsupported)
	}
	return creation, nil
}

func (m *Manager) createContainerDirect(
	ctx context.Context,
	request CreateContainerRequest,
) (string, bool, error) {
	connector := m.creationConnector
	if connector == nil {
		return "", false, nil
	}
	creation, err := connector(ctx)
	if err != nil {
		if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnavailable) {
			return "", false, nil
		}
		return "", true, err
	}
	id, createErr := creation.Create(ctx, request)
	closeErr := creation.Close()
	if errors.Is(createErr, ErrUnsupported) {
		return "", false, errors.Join(createErr, closeErr)
	}
	return id, true, errors.Join(createErr, closeErr)
}

func (m *Manager) connectContainerOperations(ctx context.Context) (containerOperations, error) {
	runtimeClient, err := m.connectContainerRuntime(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnavailable) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: connect direct container operations: %v", ErrUnavailable, err)
	}
	operations, ok := runtimeClient.(containerOperations)
	if !ok {
		_ = runtimeClient.Close()
		return nil, fmt.Errorf("%w: connected containerd client does not support lifecycle operations", ErrUnsupported)
	}
	return operations, nil
}

func (m *Manager) connectNetworkOperations(ctx context.Context) (networkOperations, error) {
	runtimeClient, err := m.connectContainerRuntime(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnavailable) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: connect direct network operations: %v", ErrUnavailable, err)
	}
	operations, ok := runtimeClient.(networkOperations)
	if !ok {
		_ = runtimeClient.Close()
		return nil, fmt.Errorf("%w: connected containerd client does not support network operations", ErrUnsupported)
	}
	return operations, nil
}

func (m *Manager) connectExecOperations(ctx context.Context) (execOperations, error) {
	runtimeClient, err := m.connectContainerRuntime(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnavailable) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: connect direct exec operations: %v", ErrUnavailable, err)
	}
	operations, ok := runtimeClient.(execOperations)
	if !ok {
		_ = runtimeClient.Close()
		return nil, fmt.Errorf("%w: connected containerd client does not support exec operations", ErrUnsupported)
	}
	return operations, nil
}

func (m *Manager) startExecDirect(
	ctx context.Context,
	request ExecRequest,
) (runtimes.Process, bool, error) {
	connector := m.execConnector
	if connector == nil {
		return nil, false, nil
	}
	operations, err := connector(ctx)
	if err != nil {
		if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnavailable) {
			return nil, false, nil
		}
		return nil, true, err
	}
	process, startErr := operations.StartExec(ctx, request)
	if errors.Is(startErr, ErrUnsupported) || errors.Is(startErr, ErrUnavailable) {
		_ = operations.Close()
		return nil, false, nil
	}
	if startErr != nil {
		return nil, true, errors.Join(startErr, operations.Close())
	}
	if process == nil {
		return nil, true, errors.Join(
			errors.New("direct exec returned an empty process"),
			operations.Close(),
		)
	}
	return &managedExecProcess{Process: process, close: operations.Close}, true, nil
}

func (m *Manager) startContainerAttachedDirect(
	ctx context.Context,
	id string,
	stdin bool,
) (runtimes.Process, bool, error) {
	connector := m.execConnector
	if connector == nil {
		return nil, false, nil
	}
	operations, err := connector(ctx)
	if err != nil {
		if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnavailable) {
			return nil, false, nil
		}
		return nil, true, err
	}
	process, startErr := operations.StartAttached(ctx, id, stdin)
	if errors.Is(startErr, ErrUnsupported) || errors.Is(startErr, ErrUnavailable) {
		_ = operations.Close()
		return nil, false, nil
	}
	if startErr != nil {
		return nil, true, errors.Join(startErr, operations.Close())
	}
	if process == nil {
		return nil, true, errors.Join(
			errors.New("direct attached start returned an empty process"),
			operations.Close(),
		)
	}
	return &managedExecProcess{Process: process, close: operations.Close}, true, nil
}

type managedExecProcess struct {
	runtimes.Process
	close    func() error
	waitOnce sync.Once
	waitErr  error
}

func (p *managedExecProcess) KillOnDisconnect() bool {
	policy, ok := p.Process.(interface{ KillOnDisconnect() bool })
	return !ok || policy.KillOnDisconnect()
}

func (p *managedExecProcess) Wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = errors.Join(p.Process.Wait(), p.close())
	})
	return p.waitErr
}

func (p *managedExecProcess) Resize(ctx context.Context, width, height uint32) error {
	resizable, ok := p.Process.(interface {
		Resize(context.Context, uint32, uint32) error
	})
	if !ok {
		return fmt.Errorf("%w: process terminal resize", ErrUnsupported)
	}
	return resizable.Resize(ctx, width, height)
}

func (m *Manager) withContainerOperations(
	ctx context.Context,
	operation func(containerOperations) error,
) (bool, error) {
	handled, operationErr, _ := m.attemptContainerOperation(ctx, operation)
	return handled, operationErr
}

func (m *Manager) attemptContainerOperation(
	ctx context.Context,
	operation func(containerOperations) error,
) (bool, error, error) {
	connector := m.operationsConnector
	if connector == nil {
		return false, nil, nil
	}
	operations, err := connector(ctx)
	if err != nil {
		if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnavailable) {
			return false, nil, err
		}
		return true, err, nil
	}
	operationErr := operation(operations)
	closeErr := operations.Close()
	if errors.Is(operationErr, ErrUnsupported) || errors.Is(operationErr, ErrUnavailable) {
		return false, nil, errors.Join(operationErr, closeErr)
	}
	return true, errors.Join(operationErr, closeErr), nil
}

func (m *Manager) withNetworkOperations(
	ctx context.Context,
	operation func(networkOperations) error,
) (bool, error) {
	connector := m.networkConnector
	if connector == nil {
		return false, nil
	}
	operations, err := connector(ctx)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return false, nil
		}
		return true, err
	}
	operationErr := operation(operations)
	closeErr := operations.Close()
	if errors.Is(operationErr, ErrUnsupported) || errors.Is(operationErr, ErrUnavailable) {
		if closeErr != nil {
			return true, closeErr
		}
		return false, nil
	}
	return true, errors.Join(operationErr, closeErr)
}

func (r *grpcContainerRuntime) Connect(
	ctx context.Context,
	network string,
	containerID string,
	aliases []string,
) error {
	resolvedID, err := r.resolveContainerID(ctx, containerID)
	if err != nil {
		return err
	}
	containerID = resolvedID
	unlock := r.networkLocks.lock(containerID)
	defer unlock()
	if network == directNetworkNone || network == directNetworkHost {
		return fmt.Errorf("%w: built-in network %q cannot be connected as an additional CNI endpoint", ErrUnsupported, network)
	}
	if len(aliases) > 1 {
		return fmt.Errorf("%w: direct CNI supports one endpoint alias", ErrUnsupported)
	}
	record, networks, aliasMap, process, err := r.directNetworkState(ctx, containerID)
	if err != nil {
		return err
	}
	containerID = record.GetID()
	if slices.Contains(networks, network) {
		return fmt.Errorf("%w: container %q is already connected to network %q", ErrConflict, containerID, network)
	}
	if process == nil || process.GetPid() == 0 {
		return fmt.Errorf("%w: container %q has no running network namespace", ErrConflict, containerID)
	}
	netns := fmt.Sprintf("/proc/%d/ns/net", process.GetPid())
	stateMap := directNetworkStateMap(record.GetLabels()[portoNetworkStateLabel])
	interfacePrefix := nextNetworkInterfacePrefix(stateMap)
	output, err := r.runRuntimeHelper(ctx,
		"cni-connect",
		"--network", network,
		"--container", containerID,
		"--netns", netns,
		"--aliases", strings.Join(aliases, ","),
		"--interface-prefix", interfacePrefix,
	)
	if err != nil {
		return err
	}
	networks = append(networks, network)
	sort.Strings(networks)
	if len(aliases) > 0 {
		aliasMap[network] = append([]string(nil), aliases...)
	}
	var endpoint struct {
		Interface string   `json:"interface"`
		MAC       string   `json:"mac"`
		Addresses []string `json:"addresses"`
		Gateways  []string `json:"gateways"`
	}
	if err := json.Unmarshal(output, &endpoint); err != nil {
		_, rollbackErr := r.runRuntimeHelper(
			context.Background(),
			"cni-disconnect",
			"--network", network,
			"--container", containerID,
			"--netns", netns,
			"--aliases", strings.Join(aliases, ","),
			"--interface-prefix", interfacePrefix,
		)
		return errors.Join(
			fmt.Errorf("decode CNI endpoint result for network %q: %w", network, err),
			rollbackErr,
		)
	}
	state := ContainerNetworkState{
		Name:      network,
		Interface: endpoint.Interface,
		MAC:       endpoint.MAC,
		Aliases:   append([]string(nil), aliases...),
	}
	if len(endpoint.Addresses) > 0 {
		state.IPAddress = endpoint.Addresses[0]
	}
	if len(endpoint.Gateways) > 0 {
		state.Gateway = endpoint.Gateways[0]
	}
	stateMap[network] = state
	networkDocument, _ := json.Marshal(networks)
	aliasDocument, _ := json.Marshal(aliasMap)
	stateDocument, _ := json.Marshal(stateMap)
	if err := r.updateLabelsResolved(ctx, containerID, map[string]string{
		nerdctlNetworksLabel:   string(networkDocument),
		portoNetworkAliasLabel: string(aliasDocument),
		portoNetworkStateLabel: string(stateDocument),
		portoNetworkPIDLabel:   strconv.FormatUint(uint64(process.GetPid()), 10),
	}); err != nil {
		_, rollbackErr := r.runRuntimeHelper(
			context.Background(),
			"cni-disconnect",
			"--network", network,
			"--container", containerID,
			"--netns", netns,
			"--aliases", strings.Join(aliases, ","),
			"--interface-prefix", interfacePrefix,
		)
		return errors.Join(err, rollbackErr)
	}
	return nil
}

func (r *grpcContainerRuntime) Disconnect(
	ctx context.Context,
	network string,
	containerID string,
	force bool,
) error {
	resolvedID, err := r.resolveContainerID(ctx, containerID)
	if err != nil {
		return err
	}
	containerID = resolvedID
	unlock := r.networkLocks.lock(containerID)
	defer unlock()
	if network == directNetworkNone || network == directNetworkHost {
		return fmt.Errorf("%w: built-in network %q cannot be disconnected", ErrUnsupported, network)
	}
	record, networks, aliasMap, process, err := r.directNetworkState(ctx, containerID)
	if err != nil {
		return err
	}
	containerID = record.GetID()
	index := slices.Index(networks, network)
	if index < 0 {
		if force {
			return nil
		}
		return fmt.Errorf("%w: container %q is not connected to network %q", ErrNotFound, containerID, network)
	}
	netns := ""
	if process != nil && process.GetPid() > 0 {
		netns = fmt.Sprintf("/proc/%d/ns/net", process.GetPid())
	}
	aliases := aliasMap[network]
	stateMap := directNetworkStateMap(record.GetLabels()[portoNetworkStateLabel])
	interfacePrefix := networkInterfacePrefix(stateMap[network].Interface)
	if _, err := r.runRuntimeHelper(ctx,
		"cni-disconnect",
		"--network", network,
		"--container", containerID,
		"--netns", netns,
		"--aliases", strings.Join(aliases, ","),
		"--interface-prefix", interfacePrefix,
	); err != nil && !force {
		return err
	}
	networks = append(networks[:index], networks[index+1:]...)
	delete(aliasMap, network)
	delete(stateMap, network)
	networkDocument, _ := json.Marshal(networks)
	aliasDocument, _ := json.Marshal(aliasMap)
	stateDocument, _ := json.Marshal(stateMap)
	networkPID := ""
	if len(stateMap) > 0 && process != nil && process.GetPid() > 0 {
		networkPID = strconv.FormatUint(uint64(process.GetPid()), 10)
	}
	if err := r.updateLabelsResolved(ctx, containerID, map[string]string{
		nerdctlNetworksLabel:   string(networkDocument),
		portoNetworkAliasLabel: string(aliasDocument),
		portoNetworkStateLabel: string(stateDocument),
		portoNetworkPIDLabel:   networkPID,
	}); err != nil {
		return fmt.Errorf("CNI endpoint was removed but container metadata reconciliation failed: %w", err)
	}
	return nil
}

func directNetworkStateMap(encoded string) map[string]ContainerNetworkState {
	result := make(map[string]ContainerNetworkState)
	if encoded != "" {
		_ = json.Unmarshal([]byte(encoded), &result)
	}
	return result
}

func (r *grpcContainerRuntime) ReconcileNetworks(
	ctx context.Context,
	containerID string,
	force bool,
) error {
	resolvedID, err := r.resolveContainerID(ctx, containerID)
	if err != nil {
		return err
	}
	containerID = resolvedID
	unlock := r.networkLocks.lock(containerID)
	defer unlock()
	record, _, _, process, err := r.directNetworkState(ctx, containerID)
	if err != nil {
		return err
	}
	netns := ""
	if process != nil && containerdTaskActive(process.GetStatus()) && process.GetPid() > 0 {
		netns = fmt.Sprintf("/proc/%d/ns/net", process.GetPid())
	}
	if netns == "" {
		return r.cleanupNetworkRecord(ctx, record, "")
	}
	return r.reconcileNetworkRecord(ctx, record, netns, force)
}

func (r *grpcContainerRuntime) CleanupNetworks(ctx context.Context, containerID string) error {
	resolvedID, err := r.resolveContainerID(ctx, containerID)
	if err != nil {
		return err
	}
	containerID = resolvedID
	unlock := r.networkLocks.lock(containerID)
	defer unlock()
	response, err := r.containers.Get(
		withContainerdNamespace(ctx, r.namespace),
		&containersapi.GetContainerRequest{ID: containerID},
	)
	if err != nil {
		return containerdOperationError("inspect network cleanup metadata for", containerID, err)
	}
	return r.cleanupNetworkRecord(ctx, response.GetContainer(), "")
}

func (r *grpcContainerRuntime) reconcileNetworkRecord(
	ctx context.Context,
	record *containersapi.Container,
	netns string,
	force bool,
) error {
	if record == nil || netns == "" {
		return nil
	}
	states := directNetworkStateMap(record.GetLabels()[portoNetworkStateLabel])
	if len(states) == 0 {
		return nil
	}
	aliases := map[string][]string{}
	if encoded := record.GetLabels()[portoNetworkAliasLabel]; encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &aliases); err != nil {
			return fmt.Errorf("decode network aliases for container %q: %w", record.GetID(), err)
		}
	}
	currentPID := ""
	if fields := strings.Split(strings.Trim(netns, "/"), "/"); len(fields) >= 2 {
		currentPID = fields[1]
	}
	changed := record.GetLabels()[portoNetworkPIDLabel] != currentPID
	for network, state := range states {
		if network == directNetworkNone || network == directNetworkHost {
			continue
		}
		prefix := networkInterfacePrefix(state.Interface)
		arguments := []string{
			"--network", network,
			"--container", record.GetID(),
			"--netns", netns,
			"--aliases", strings.Join(aliases[network], ","),
			"--interface-prefix", prefix,
		}
		if !force {
			output, err := r.runRuntimeHelper(ctx, append([]string{"cni-check"}, arguments...)...)
			if err == nil {
				var check struct {
					Valid     bool `json:"valid"`
					Supported bool `json:"supported"`
				}
				if json.Unmarshal(output, &check) == nil && (check.Valid || !check.Supported) {
					continue
				}
			}
		}
		output, err := r.runRuntimeHelper(ctx, append([]string{"cni-connect"}, arguments...)...)
		if err != nil {
			return fmt.Errorf("restore CNI endpoint %q for container %q: %w", network, record.GetID(), err)
		}
		var endpoint struct {
			Interface string   `json:"interface"`
			MAC       string   `json:"mac"`
			Addresses []string `json:"addresses"`
			Gateways  []string `json:"gateways"`
		}
		if err := json.Unmarshal(output, &endpoint); err != nil {
			return fmt.Errorf("decode restored CNI endpoint %q: %w", network, err)
		}
		state.Interface = endpoint.Interface
		state.MAC = endpoint.MAC
		state.Aliases = append([]string(nil), aliases[network]...)
		if len(endpoint.Addresses) > 0 {
			state.IPAddress = endpoint.Addresses[0]
		}
		if len(endpoint.Gateways) > 0 {
			state.Gateway = endpoint.Gateways[0]
		}
		states[network] = state
		changed = true
	}
	if !changed {
		return nil
	}
	encoded, err := json.Marshal(states)
	if err != nil {
		return fmt.Errorf("encode reconciled networks for container %q: %w", record.GetID(), err)
	}
	return r.updateLabelsResolved(ctx, record.GetID(), map[string]string{
		portoNetworkStateLabel: string(encoded),
		portoNetworkPIDLabel:   currentPID,
	})
}

func (r *grpcContainerRuntime) cleanupNetworkRecord(
	ctx context.Context,
	record *containersapi.Container,
	netns string,
) error {
	if record == nil {
		return nil
	}
	states := directNetworkStateMap(record.GetLabels()[portoNetworkStateLabel])
	aliases := map[string][]string{}
	if encoded := record.GetLabels()[portoNetworkAliasLabel]; encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &aliases); err != nil {
			return fmt.Errorf("decode network aliases for container %q: %w", record.GetID(), err)
		}
	}
	var cleanupErr error
	for network, state := range states {
		if network == directNetworkNone || network == directNetworkHost {
			continue
		}
		_, err := r.runRuntimeHelper(
			ctx,
			"cni-disconnect",
			"--network", network,
			"--container", record.GetID(),
			"--netns", netns,
			"--aliases", strings.Join(aliases[network], ","),
			"--interface-prefix", networkInterfacePrefix(state.Interface),
		)
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if cleanupErr == nil && len(states) > 0 {
		cleanupErr = r.updateLabelsResolved(ctx, record.GetID(), map[string]string{
			portoNetworkPIDLabel: "",
		})
	}
	return cleanupErr
}

func nextNetworkInterfacePrefix(states map[string]ContainerNetworkState) string {
	used := make(map[string]struct{}, len(states))
	for _, state := range states {
		used[networkInterfacePrefix(state.Interface)] = struct{}{}
	}
	for index := 1; ; index++ {
		prefix := fmt.Sprintf("porto%d", index)
		if _, ok := used[prefix]; !ok {
			return prefix
		}
	}
}

func networkInterfacePrefix(name string) string {
	if strings.HasSuffix(name, "0") {
		return strings.TrimSuffix(name, "0")
	}
	if name != "" {
		return name
	}
	return "eth"
}

func (r *grpcContainerRuntime) directNetworkState(
	ctx context.Context,
	containerID string,
) (*containersapi.Container, []string, map[string][]string, *tasktypes.Process, error) {
	if r.containers == nil || r.tasks == nil {
		return nil, nil, nil, nil, fmt.Errorf("%w: container and task services are required for CNI", ErrUnavailable)
	}
	resolvedID, err := r.resolveContainerID(ctx, containerID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	containerID = resolvedID
	response, err := r.containers.Get(
		withContainerdNamespace(ctx, r.namespace),
		&containersapi.GetContainerRequest{ID: containerID},
	)
	if err != nil {
		return nil, nil, nil, nil, containerdOperationError("inspect network metadata for", containerID, err)
	}
	record := response.GetContainer()
	var networks []string
	if encoded := record.GetLabels()[nerdctlNetworksLabel]; encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &networks); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("decode networks for container %q: %w", containerID, err)
		}
	}
	aliasMap := make(map[string][]string)
	if encoded := record.GetLabels()[portoNetworkAliasLabel]; encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &aliasMap); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("decode network aliases for container %q: %w", containerID, err)
		}
	}
	process, err := r.getTask(ctx, containerID)
	if err != nil {
		if status.Code(err) != codes.NotFound {
			return nil, nil, nil, nil, containerdOperationError("inspect network namespace for", containerID, err)
		}
		process = nil
	}
	return record, networks, aliasMap, process, nil
}

func (r *grpcContainerRuntime) runRuntimeHelper(ctx context.Context, args ...string) ([]byte, error) {
	command := runtimes.Command{}
	if r.lima != "" {
		command.Name = "limactl"
		command.Args = []string{
			"shell", r.lima, "--", "sh", "-c",
			`exec "$HOME/.local/bin/porto-runtime-helper" "$@"`,
			"porto-runtime-helper",
		}
		command.Args = append(command.Args, args...)
	} else {
		if r.helperPath == "" {
			return nil, fmt.Errorf("%w: Porto runtime helper is unavailable", ErrUnsupported)
		}
		command.Name = r.helperPath
		command.Args = append([]string(nil), args...)
	}
	output, err := r.runner.Run(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("run Porto runtime helper %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func (r *grpcContainerRuntime) Start(ctx context.Context, id string) error {
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	process, err := r.getTask(ctx, id)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return r.recreateAndStartTask(ctx, id)
		}
		return containerdOperationError("inspect task for", id, err)
	}
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	switch process.GetStatus() {
	case tasktypes.Status_CREATED:
		if err := r.setRestartDesired(ctx, id, true); err != nil {
			return err
		}
		_, err = r.tasks.Start(namespacedContext, &tasksapi.StartRequest{ContainerID: id})
	case tasktypes.Status_PAUSED:
		if err := r.setRestartDesired(ctx, id, true); err != nil {
			return err
		}
		_, err = r.tasks.Resume(namespacedContext, &tasksapi.ResumeTaskRequest{ContainerID: id})
	case tasktypes.Status_RUNNING:
		return fmt.Errorf("%w: preserve existing handling for an already-running container", ErrUnsupported)
	case tasktypes.Status_STOPPED:
		return r.recreateAndStartTask(ctx, id)
	default:
		return fmt.Errorf("%w: container %q task is %s", ErrConflict, id, process.GetStatus())
	}
	if operationErr := containerdOperationError("start", id, err); operationErr != nil {
		return errors.Join(operationErr, r.setRestartDesired(ctx, id, false))
	}
	return nil
}

func (r *grpcContainerRuntime) Stop(ctx context.Context, id string, timeoutSeconds int) error {
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	process, err := r.getTask(ctx, id)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			if err := r.requireContainer(ctx, id); err != nil {
				return err
			}
			return r.setRestartDesired(ctx, id, false)
		}
		return containerdOperationError("inspect task for", id, err)
	}
	if err := r.setRestartDesired(ctx, id, false); err != nil {
		return err
	}
	switch process.GetStatus() {
	case tasktypes.Status_CREATED, tasktypes.Status_STOPPED:
		return nil
	case tasktypes.Status_PAUSED:
		if _, err := r.tasks.Resume(
			withContainerdNamespace(ctx, r.namespace),
			&tasksapi.ResumeTaskRequest{ContainerID: id},
		); err != nil {
			return containerdOperationError("resume before stopping", id, err)
		}
	case tasktypes.Status_RUNNING:
	default:
		return fmt.Errorf("%w: container %q task is %s", ErrConflict, id, process.GetStatus())
	}

	stopSignal, stopTimeout, err := r.stopOptions(ctx, id, timeoutSeconds)
	if err != nil {
		return err
	}
	if err := r.Kill(ctx, id, stopSignal); err != nil {
		return err
	}
	waitContext, cancel := context.WithTimeout(ctx, stopTimeout)
	_, waitErr := r.tasks.Wait(
		withContainerdNamespace(waitContext, r.namespace),
		&tasksapi.WaitRequest{ContainerID: id},
	)
	cancel()
	if waitErr == nil {
		return r.CleanupNetworks(ctx, id)
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if status.Code(waitErr) != codes.DeadlineExceeded {
		return containerdOperationError("wait for stopped", id, waitErr)
	}
	if err := r.Kill(ctx, id, 9); err != nil {
		return fmt.Errorf("force stop container %q: %w", id, err)
	}
	_, err = r.tasks.Wait(
		withContainerdNamespace(ctx, r.namespace),
		&tasksapi.WaitRequest{ContainerID: id},
	)
	if err := containerdOperationError("wait for force-stopped", id, err); err != nil {
		return err
	}
	return r.CleanupNetworks(ctx, id)
}

func (r *grpcContainerRuntime) Kill(ctx context.Context, id string, signal uint32) error {
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	_, err = r.tasks.Kill(
		withContainerdNamespace(ctx, r.namespace),
		&tasksapi.KillRequest{ContainerID: id, Signal: signal},
	)
	return containerdOperationError("kill", id, err)
}

func (r *grpcContainerRuntime) Pause(ctx context.Context, id string) error {
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	_, err = r.tasks.Pause(
		withContainerdNamespace(ctx, r.namespace),
		&tasksapi.PauseTaskRequest{ContainerID: id},
	)
	return containerdOperationError("pause", id, err)
}

func (r *grpcContainerRuntime) Resume(ctx context.Context, id string) error {
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	_, err = r.tasks.Resume(
		withContainerdNamespace(ctx, r.namespace),
		&tasksapi.ResumeTaskRequest{ContainerID: id},
	)
	return containerdOperationError("resume", id, err)
}

func (r *grpcContainerRuntime) Restart(ctx context.Context, id string, timeoutSeconds int) error {
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	process, err := r.getTask(ctx, id)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return r.recreateAndStartTask(ctx, id)
		}
		return containerdOperationError("inspect task for", id, err)
	}
	if containerdTaskActive(process.GetStatus()) {
		if err := r.Stop(ctx, id, timeoutSeconds); err != nil {
			return errors.Join(err, r.setRestartDesired(context.Background(), id, true))
		}
	}
	return r.recreateAndStartTask(ctx, id)
}

func (r *grpcContainerRuntime) recreateAndStartTask(ctx context.Context, id string) error {
	if r.containers == nil || r.tasks == nil {
		return taskRecreationUnsupportedError(id, "containerd task recreation services are unavailable")
	}
	unlock := r.networkLocks.lock(id)
	defer unlock()
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	response, err := r.containers.Get(
		namespacedContext,
		&containersapi.GetContainerRequest{ID: id},
	)
	if err != nil {
		return containerdOperationError("inspect metadata for", id, err)
	}
	record := response.GetContainer()
	if record == nil || record.GetSpec() == nil {
		return taskRecreationUnsupportedError(id, "container metadata exists without a task and does not include an OCI spec")
	}

	var rootfs []*types.Mount
	if record.GetSnapshotKey() != "" || record.GetSnapshotter() != "" {
		if record.GetSnapshotKey() == "" || record.GetSnapshotter() == "" || r.snapshots == nil {
			return fmt.Errorf("%w: container %q has incomplete snapshot metadata", ErrUnsupported, id)
		}
		mounts, mountErr := r.snapshots.Mounts(namespacedContext, &snapshotsapi.MountsRequest{
			Snapshotter: record.GetSnapshotter(),
			Key:         record.GetSnapshotKey(),
		})
		if mountErr != nil {
			return containerdOperationError("resolve rootfs for", id, mountErr)
		}
		rootfs = mounts.GetMounts()
	}

	taskFound := false
	if process, taskErr := r.getTask(ctx, id); taskErr == nil {
		if process.GetStatus() != tasktypes.Status_STOPPED &&
			process.GetStatus() != tasktypes.Status_CREATED {
			return fmt.Errorf("%w: container %q task is %s", ErrConflict, id, process.GetStatus())
		}
		if err := r.cleanupNetworkRecord(ctx, record, ""); err != nil {
			return err
		}
		taskFound = true
		if _, deleteErr := r.tasks.Delete(namespacedContext, &tasksapi.DeleteTaskRequest{ContainerID: id}); deleteErr != nil {
			return containerdOperationError("delete stopped task for", id, deleteErr)
		}
	} else if status.Code(taskErr) != codes.NotFound {
		return containerdOperationError("inspect task for", id, taskErr)
	}
	if !taskFound {
		if err := r.cleanupNetworkRecord(ctx, record, ""); err != nil {
			return err
		}
	}
	var runtimeOptions *anypb.Any
	if runtime := record.GetRuntime(); runtime != nil {
		if runtime.GetOptions() != nil {
			runtimeOptions = proto.Clone(runtime.GetOptions()).(*anypb.Any)
		}
	}
	createRequest := &tasksapi.CreateTaskRequest{
		ContainerID: id,
		Rootfs:      rootfs,
		Terminal:    ociSpecTerminal(record.GetSpec().GetValue()),
		Options:     runtimeOptions,
	}
	if record.GetLabels()[portoManagedLabel] == portoRuntimeVersion {
		logPath := record.GetLabels()[portoLogPathLabel]
		if logPath == "" {
			return fmt.Errorf("%w: Porto-managed container %q has no persistent log path", ErrConflict, id)
		}
		if err := r.prepareDirectLogPath(ctx, logPath); err != nil {
			return err
		}
		logURI := (&url.URL{Scheme: "file", Path: filepath.ToSlash(logPath)}).String()
		createRequest.Stdout = logURI
		createRequest.Stderr = logURI
	}
	createResponse, err := r.tasks.Create(namespacedContext, createRequest)
	if err != nil {
		return containerdOperationError("recreate task for", id, err)
	}
	networkNetNS := ""
	if record.GetLabels()[portoManagedLabel] == portoRuntimeVersion {
		if createResponse.GetPid() > 0 {
			networkNetNS = fmt.Sprintf("/proc/%d/ns/net", createResponse.GetPid())
		}
		if err := r.reconcileNetworkRecord(ctx, record, networkNetNS, true); err != nil {
			cleanupErr := r.cleanupNetworkRecord(context.Background(), record, networkNetNS)
			_, deleteErr := r.tasks.Delete(namespacedContext, &tasksapi.DeleteTaskRequest{ContainerID: id})
			return errors.Join(
				err,
				cleanupErr,
				containerdOperationError("delete task after CNI failure for", id, deleteErr),
			)
		}
	}
	if err := r.setRestartDesired(ctx, id, true); err != nil {
		networkCleanupErr := r.cleanupNetworkRecord(context.Background(), record, networkNetNS)
		_, deleteErr := r.tasks.Delete(namespacedContext, &tasksapi.DeleteTaskRequest{ContainerID: id})
		return errors.Join(
			err,
			networkCleanupErr,
			containerdOperationError("delete task after restart-policy failure for", id, deleteErr),
		)
	}
	if _, err = r.tasks.Start(namespacedContext, &tasksapi.StartRequest{ContainerID: id}); err != nil {
		restartRollbackErr := r.setRestartDesired(context.Background(), id, false)
		networkCleanupErr := r.cleanupNetworkRecord(context.Background(), record, networkNetNS)
		_, deleteErr := r.tasks.Delete(namespacedContext, &tasksapi.DeleteTaskRequest{ContainerID: id})
		return errors.Join(
			containerdOperationError("start recreated task for", id, err),
			restartRollbackErr,
			networkCleanupErr,
			containerdOperationError("delete failed recreated task for", id, deleteErr),
		)
	}
	return nil
}

func (r *grpcContainerRuntime) prepareDirectLogPath(ctx context.Context, logPath string) error {
	if r.lima != "" {
		_, err := r.runner.Run(ctx, runtimes.Command{
			Name: "limactl",
			Args: []string{
				"shell", r.lima, "--", "sh", "-c",
				`set -eu; umask 077; mkdir -p -- "$1"; touch -- "$2"`,
				"porto-container-log",
				filepath.Dir(logPath),
				logPath,
			},
		})
		if err != nil {
			return fmt.Errorf("prepare Lima container log %q: %w", logPath, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return fmt.Errorf("create direct container log directory: %w", err)
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("prepare direct container log %q: %w", logPath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close direct container log %q: %w", logPath, err)
	}
	return nil
}

func ociSpecTerminal(encoded []byte) bool {
	var spec specs.Spec
	if err := json.Unmarshal(encoded, &spec); err != nil || spec.Process == nil {
		return false
	}
	return spec.Process.Terminal
}

func (r *grpcContainerRuntime) Wait(ctx context.Context, id string) (int, error) {
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return 0, err
	}
	id = resolvedID
	process, err := r.getTask(ctx, id)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			if containerErr := r.requireContainer(ctx, id); containerErr != nil {
				return 0, containerErr
			}
			return 0, nil
		}
		return 0, containerdOperationError("inspect task for", id, err)
	}
	if !containerdTaskActive(process.GetStatus()) {
		return int(process.GetExitStatus()), nil
	}
	response, err := r.tasks.Wait(
		withContainerdNamespace(ctx, r.namespace),
		&tasksapi.WaitRequest{ContainerID: id},
	)
	if err != nil {
		return 0, containerdOperationError("wait for", id, err)
	}
	return int(response.GetExitStatus()), nil
}

func (r *grpcContainerRuntime) Rename(ctx context.Context, id, name string) error {
	return r.UpdateLabels(ctx, id, map[string]string{nerdctlNameLabel: name})
}

func (r *grpcContainerRuntime) UpdateLabels(ctx context.Context, id string, updates map[string]string) error {
	if r.containers == nil {
		return fmt.Errorf("%w: container metadata service is unavailable", ErrUnavailable)
	}
	if len(updates) == 0 {
		return nil
	}
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	return r.updateLabelsResolved(ctx, id, updates)
}

func (r *grpcContainerRuntime) updateLabelsResolved(
	ctx context.Context,
	id string,
	updates map[string]string,
) error {
	labels := cloneStringMap(updates)
	paths := make([]string, 0, len(labels))
	for key := range labels {
		paths = append(paths, "labels."+key)
	}
	sort.Strings(paths)
	_, err := r.containers.Update(
		withContainerdNamespace(ctx, r.namespace),
		&containersapi.UpdateContainerRequest{
			Container: &containersapi.Container{
				ID:     id,
				Labels: labels,
			},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths},
		})
	return containerdOperationError("update metadata for", id, err)
}

func (r *grpcContainerRuntime) UpdateRestartPolicy(ctx context.Context, id, policy string) error {
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	if err := validateRestartPolicy(policy); err != nil {
		return err
	}
	updates := map[string]string{restartPolicyLabel: policy}
	if policy == "" || policy == "no" {
		updates[restartPolicyLabel] = ""
		updates[restartCountLabel] = ""
		updates[restartStatusLabel] = ""
		updates[restartLogURILabel] = ""
		updates[restartStoppedLabel] = ""
	} else {
		response, err := r.containers.Get(
			withContainerdNamespace(ctx, r.namespace),
			&containersapi.GetContainerRequest{ID: id},
		)
		if err != nil {
			return containerdOperationError("inspect restart metadata for", id, err)
		}
		logPath := response.GetContainer().GetLabels()[portoLogPathLabel]
		if logPath == "" {
			return fmt.Errorf("%w: container %q has no persistent restart log", ErrUnsupported, id)
		}
		running := false
		if process, taskErr := r.getTask(ctx, id); taskErr == nil {
			running = containerdTaskActive(process.GetStatus())
		} else if status.Code(taskErr) != codes.NotFound {
			return containerdOperationError("inspect task for restart update", id, taskErr)
		}
		updates[restartStatusLabel] = map[bool]string{true: "running", false: "stopped"}[running]
		updates[restartStoppedLabel] = strconv.FormatBool(!running)
		updates[restartLogURILabel] = (&url.URL{
			Scheme: "file",
			Path:   filepath.ToSlash(logPath),
		}).String()
	}
	return r.updateLabelsResolved(ctx, id, updates)
}

func (r *grpcContainerRuntime) setRestartDesired(ctx context.Context, id string, running bool) error {
	if r.containers == nil {
		return nil
	}
	response, err := r.containers.Get(
		withContainerdNamespace(ctx, r.namespace),
		&containersapi.GetContainerRequest{ID: id},
	)
	if err != nil {
		return containerdOperationError("inspect restart policy for", id, err)
	}
	if response.GetContainer().GetLabels()[restartPolicyLabel] == "" {
		return nil
	}
	statusValue := "stopped"
	if running {
		statusValue = "running"
	}
	return r.updateLabelsResolved(ctx, id, map[string]string{
		restartStatusLabel:  statusValue,
		restartStoppedLabel: strconv.FormatBool(!running),
	})
}

func (r *grpcContainerRuntime) UpdateResources(ctx context.Context, id string, update ContainerUpdate) error {
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	response, err := r.containers.Get(
		namespacedContext,
		&containersapi.GetContainerRequest{ID: id},
	)
	if err != nil {
		return containerdOperationError("inspect resources for", id, err)
	}
	record := response.GetContainer()
	if record == nil || record.GetSpec() == nil || len(record.GetSpec().GetValue()) == 0 {
		return fmt.Errorf("%w: container %q does not have a directly updatable OCI spec", ErrUnsupported, id)
	}

	var spec specs.Spec
	if err := json.Unmarshal(record.GetSpec().GetValue(), &spec); err != nil {
		return fmt.Errorf("%w: decode OCI resources for container %q: %v", ErrUnsupported, id, err)
	}
	if spec.Windows != nil {
		return fmt.Errorf("%w: direct Windows resource updates are not supported", ErrUnsupported)
	}
	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}
	if spec.Linux.Resources == nil {
		spec.Linux.Resources = &specs.LinuxResources{}
	}
	applyContainerUpdate(spec.Linux.Resources, update)

	encodedSpec, err := json.Marshal(&spec)
	if err != nil {
		return fmt.Errorf("encode OCI resources for container %q: %w", id, err)
	}
	encodedResources, err := json.Marshal(spec.Linux.Resources)
	if err != nil {
		return fmt.Errorf("encode task resources for container %q: %w", id, err)
	}

	updatedRecord := proto.Clone(record).(*containersapi.Container)
	updatedRecord.Spec = &anypb.Any{
		TypeUrl: record.GetSpec().GetTypeUrl(),
		Value:   encodedSpec,
	}
	_, err = r.containers.Update(namespacedContext, &containersapi.UpdateContainerRequest{
		Container:  updatedRecord,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec"}},
	})
	if err != nil {
		return containerdOperationError("update resource metadata for", id, err)
	}

	process, err := r.getTask(ctx, id)
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		return r.rollbackContainerSpec(
			namespacedContext,
			id,
			record,
			containerdOperationError("inspect task resources for", id, err),
		)
	}
	if process == nil || !containerdTaskActive(process.GetStatus()) {
		return nil
	}
	_, err = r.tasks.Update(namespacedContext, &tasksapi.UpdateTaskRequest{
		ContainerID: id,
		Resources: &anypb.Any{
			TypeUrl: containerdLinuxResourcesTypeURL,
			Value:   encodedResources,
		},
	})
	if err == nil || status.Code(err) == codes.NotFound {
		return nil
	}
	return r.rollbackContainerSpec(
		namespacedContext,
		id,
		record,
		containerdOperationError("update task resources for", id, err),
	)
}

func (r *grpcContainerRuntime) UpdateHealth(
	ctx context.Context,
	id string,
	healthcheck *ContainerHealthcheck,
) error {
	if healthcheck == nil {
		return errors.New("healthcheck is required")
	}
	if r.containers == nil {
		return fmt.Errorf("%w: container metadata service is unavailable", ErrUnavailable)
	}
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	if err := validateHealthcheck(healthcheck); err != nil {
		return err
	}
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	response, err := r.containers.Get(
		namespacedContext,
		&containersapi.GetContainerRequest{ID: id},
	)
	if err != nil {
		return containerdOperationError("inspect health metadata for", id, err)
	}
	record := response.GetContainer()
	if record.GetLabels()[portoManagedLabel] != portoRuntimeVersion {
		return fmt.Errorf(
			"%w: container %q health scheduling is owned by its compatibility runtime",
			ErrUnsupported,
			id,
		)
	}
	encoded, err := json.Marshal(healthcheck)
	if err != nil {
		return fmt.Errorf("encode healthcheck for container %q: %w", id, err)
	}
	status := "starting"
	if len(healthcheck.Test) == 0 || healthcheck.Test[0] == "" || healthcheck.Test[0] == "NONE" {
		status = "none"
	}
	state, err := json.Marshal(containerHealthState{Status: status})
	if err != nil {
		return fmt.Errorf("encode health state for container %q: %w", id, err)
	}
	return r.updateLabelsResolved(ctx, id, map[string]string{
		nerdctlHealthcheckLabel: string(encoded),
		nerdctlHealthStateLabel: string(state),
	})
}

func (r *grpcContainerRuntime) Checkpoint(ctx context.Context, id, parent string) ([]string, error) {
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return nil, err
	}
	id = resolvedID
	if r.client != nil {
		return r.checkpointContainer(ctx, id, parent)
	}
	if r.tasks == nil {
		return nil, fmt.Errorf("%w: task service is unavailable", ErrUnavailable)
	}
	response, err := r.tasks.Checkpoint(
		withContainerdNamespace(ctx, r.namespace),
		&tasksapi.CheckpointTaskRequest{
			ContainerID:      id,
			ParentCheckpoint: parent,
		},
	)
	if err != nil {
		return nil, containerdOperationError("checkpoint", id, err)
	}
	descriptors := response.GetDescriptors()
	result := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if descriptor.GetMediaType() != "" {
			result = append(result, descriptor.GetMediaType())
		}
	}
	return result, nil
}

func (r *grpcContainerRuntime) Restore(ctx context.Context, id, checkpoint string) error {
	if r.client == nil {
		return fmt.Errorf("%w: high-level containerd restore client is unavailable", ErrUnsupported)
	}
	unlock := r.networkLocks.lock(id)
	defer unlock()
	capability := r.Capabilities(ctx).CheckpointRestore
	if !capability.Supported {
		return fmt.Errorf("%w: container restore: %s", ErrUnsupported, capability.Reason)
	}
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	if _, err := r.client.LoadContainer(namespacedContext, id); err == nil {
		return fmt.Errorf("%w: restore target container %q already exists", ErrConflict, id)
	} else if !errdefs.IsNotFound(err) {
		return containerdOperationError("inspect restore target", id, err)
	}
	checkpointImage, err := r.client.GetImage(namespacedContext, checkpoint)
	if err != nil {
		return fmt.Errorf("load checkpoint %q: %w", checkpoint, err)
	}
	if runtimeName := checkpointImage.Labels()[checkpointRuntimeLabel]; runtimeName != portoRuntimeName {
		return fmt.Errorf(
			"%w: checkpoint %q uses runtime %q instead of %q",
			ErrUnsupported,
			checkpoint,
			runtimeName,
			portoRuntimeName,
		)
	}
	restored, err := r.client.Restore(
		namespacedContext,
		id,
		checkpointImage,
		containerd.WithRestoreImage,
		containerd.WithRestoreSpec,
		containerd.WithRestoreRuntime,
		containerd.WithRestoreRW,
	)
	if err != nil {
		return fmt.Errorf("restore container %q metadata from checkpoint %q: %w", id, checkpoint, err)
	}
	cleanup := func(operationErr error) error {
		cleanupErr := restored.Delete(
			withContainerdNamespace(context.Background(), r.namespace),
			containerd.WithSnapshotCleanup,
		)
		return errors.Join(operationErr, cleanupErr)
	}
	labels, err := checkpointContainerLabels(checkpointImage.Labels())
	if err != nil {
		return cleanup(err)
	}
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[portoManagedLabel] = portoRuntimeVersion
	labels[nerdctlNameLabel] = id
	logPath, err := r.directContainerLogPath(id)
	if err != nil {
		return cleanup(err)
	}
	labels[portoLogPathLabel] = logPath
	if labels[restartPolicyLabel] != "" {
		labels[restartStatusLabel] = "stopped"
		labels[restartStoppedLabel] = "true"
		labels[restartLogURILabel] = (&url.URL{
			Scheme: "file",
			Path:   filepath.ToSlash(logPath),
		}).String()
	}
	if _, err := restored.SetLabels(namespacedContext, labels); err != nil {
		return cleanup(fmt.Errorf("restore container %q labels: %w", id, err))
	}
	spec, err := restored.Spec(namespacedContext)
	if err != nil {
		return cleanup(fmt.Errorf("read restored container %q spec: %w", id, err))
	}
	if err := r.prepareDirectLogPath(ctx, logPath); err != nil {
		return cleanup(err)
	}
	logURI := &url.URL{Scheme: "file", Path: filepath.ToSlash(logPath)}
	ioCreator := cio.LogURI(logURI)
	if spec.Process != nil && spec.Process.Terminal {
		ioCreator = cio.TerminalLogURI(logURI)
	}
	task, err := restored.NewTask(
		namespacedContext,
		ioCreator,
		containerd.WithTaskCheckpoint(checkpointImage),
	)
	if err != nil {
		return cleanup(fmt.Errorf("create restored task for container %q: %w", id, err))
	}
	networkResponse, err := r.containers.Get(
		namespacedContext,
		&containersapi.GetContainerRequest{ID: id},
	)
	if err != nil {
		_, _ = task.Delete(withContainerdNamespace(context.Background(), r.namespace))
		return cleanup(containerdOperationError("read restored network metadata for", id, err))
	}
	netns := ""
	if task.Pid() > 0 {
		netns = fmt.Sprintf("/proc/%d/ns/net", task.Pid())
	}
	if err := r.reconcileNetworkRecord(ctx, networkResponse.GetContainer(), netns, true); err != nil {
		networkCleanupErr := r.cleanupNetworkRecord(context.Background(), networkResponse.GetContainer(), netns)
		_, _ = task.Delete(withContainerdNamespace(context.Background(), r.namespace))
		return cleanup(errors.Join(err, networkCleanupErr))
	}
	if err := r.setRestartDesired(ctx, id, true); err != nil {
		restartRollbackErr := r.setRestartDesired(context.Background(), id, false)
		networkCleanupErr := r.cleanupNetworkRecord(context.Background(), networkResponse.GetContainer(), netns)
		_, deleteErr := task.Delete(withContainerdNamespace(context.Background(), r.namespace))
		return cleanup(errors.Join(err, restartRollbackErr, networkCleanupErr, deleteErr))
	}
	if err := task.Start(namespacedContext); err != nil {
		restartRollbackErr := r.setRestartDesired(context.Background(), id, false)
		networkCleanupErr := r.cleanupNetworkRecord(context.Background(), networkResponse.GetContainer(), netns)
		_, deleteErr := task.Delete(withContainerdNamespace(context.Background(), r.namespace))
		return cleanup(errors.Join(
			fmt.Errorf("start restored task for container %q: %w", id, err),
			restartRollbackErr,
			networkCleanupErr,
			deleteErr,
		))
	}
	return nil
}

func applyContainerUpdate(resources *specs.LinuxResources, update ContainerUpdate) {
	if update.NanoCPUs > 0 {
		if resources.CPU == nil {
			resources.CPU = &specs.LinuxCPU{}
		}
		period := uint64(100_000)
		quota := update.NanoCPUs / 10_000
		resources.CPU.Period = &period
		resources.CPU.Quota = &quota
	}
	if update.Memory > 0 || update.MemorySwap > 0 {
		if resources.Memory == nil {
			resources.Memory = &specs.LinuxMemory{}
		}
	}
	if update.Memory > 0 {
		memory := update.Memory
		resources.Memory.Limit = &memory
		if update.MemorySwap == 0 {
			memorySwap := update.Memory * 2
			resources.Memory.Swap = &memorySwap
		}
	}
	if update.MemorySwap > 0 {
		memorySwap := update.MemorySwap
		resources.Memory.Swap = &memorySwap
	}
}

func (r *grpcContainerRuntime) rollbackContainerSpec(
	ctx context.Context,
	id string,
	record *containersapi.Container,
	operationErr error,
) error {
	_, rollbackErr := r.containers.Update(ctx, &containersapi.UpdateContainerRequest{
		Container:  record,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec"}},
	})
	if rollbackErr == nil {
		return operationErr
	}
	return fmt.Errorf(
		"direct resource update failed after changing container %q metadata: %v; rollback failed: %v",
		id,
		operationErr,
		rollbackErr,
	)
}

func (r *grpcContainerRuntime) Delete(ctx context.Context, id string, force, volumes bool) error {
	if volumes {
		return fmt.Errorf("%w: direct removal does not clean up container volumes", ErrUnsupported)
	}
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	unlock := r.networkLocks.lock(id)
	defer unlock()
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	response, err := r.containers.Get(
		namespacedContext,
		&containersapi.GetContainerRequest{ID: id},
	)
	if err != nil {
		return containerdOperationError("inspect metadata for", id, err)
	}
	record := response.GetContainer()
	if record == nil {
		return fmt.Errorf("inspect metadata for container %q returned an empty record", id)
	}
	managed := record.GetLabels()[portoManagedLabel] == portoRuntimeVersion
	if !managed && (record.GetSnapshotKey() != "" || record.GetSnapshotter() != "" ||
		record.GetLabels()[nerdctlNetworksLabel] != "" ||
		record.GetLabels()[nerdctlPortsLabel] != "") {
		return fmt.Errorf("%w: direct removal cannot safely clean up container snapshots or networking", ErrUnsupported)
	}

	process, err := r.getTask(ctx, id)
	if err != nil && status.Code(err) != codes.NotFound {
		return containerdOperationError("inspect task for", id, err)
	}
	networksCleaned := false
	if err == nil {
		switch process.GetStatus() {
		case tasktypes.Status_CREATED, tasktypes.Status_STOPPED:
		case tasktypes.Status_PAUSED:
			if !force {
				return fmt.Errorf("%w: container %q task is paused", ErrConflict, id)
			}
			if _, err := r.tasks.Resume(
				namespacedContext,
				&tasksapi.ResumeTaskRequest{ContainerID: id},
			); err != nil {
				return containerdOperationError("resume before removing", id, err)
			}
			fallthrough
		case tasktypes.Status_RUNNING, tasktypes.Status_PAUSING:
			if !force {
				return fmt.Errorf("%w: container %q task is running", ErrConflict, id)
			}
			if _, err := r.tasks.Kill(
				namespacedContext,
				&tasksapi.KillRequest{ContainerID: id, Signal: 9},
			); err != nil {
				return containerdOperationError("kill before removing", id, err)
			}
			if _, err := r.tasks.Wait(
				namespacedContext,
				&tasksapi.WaitRequest{ContainerID: id},
			); err != nil {
				return containerdOperationError("wait before removing", id, err)
			}
		default:
			return fmt.Errorf("%w: container %q task is %s", ErrConflict, id, process.GetStatus())
		}
		if managed {
			if err := r.cleanupNetworkRecord(ctx, record, ""); err != nil {
				return err
			}
			networksCleaned = true
		}
		if _, err := r.tasks.Delete(
			namespacedContext,
			&tasksapi.DeleteTaskRequest{ContainerID: id},
		); err != nil {
			return containerdOperationError("delete task for", id, err)
		}
	}
	if managed && !networksCleaned {
		if err := r.cleanupNetworkRecord(ctx, record, ""); err != nil {
			return err
		}
	}
	_, err = r.containers.Delete(
		namespacedContext,
		&containersapi.DeleteContainerRequest{ID: id},
	)
	if err != nil {
		return containerdOperationError("delete metadata for", id, err)
	}
	if managed && record.GetSnapshotKey() != "" {
		if r.client == nil {
			return fmt.Errorf(
				"container %q metadata was removed but snapshot %q remains: %w",
				id,
				record.GetSnapshotKey(),
				ErrUnavailable,
			)
		}
		snapshotter := record.GetSnapshotter()
		if snapshotter == "" {
			return fmt.Errorf("container %q metadata was removed but its snapshotter is unknown", id)
		}
		if err := r.client.SnapshotService(snapshotter).Remove(namespacedContext, record.GetSnapshotKey()); err != nil &&
			!errdefs.IsNotFound(err) {
			return fmt.Errorf(
				"container %q metadata was removed but snapshot %q cleanup failed: %w",
				id,
				record.GetSnapshotKey(),
				err,
			)
		}
	}
	if managed {
		if err := r.removeDirectLog(ctx, record.GetLabels()[portoLogPathLabel]); err != nil {
			return fmt.Errorf("container %q was removed but log cleanup failed: %w", id, err)
		}
	}
	return nil
}

func (r *grpcContainerRuntime) removeDirectLog(ctx context.Context, logPath string) error {
	if logPath == "" {
		return nil
	}
	if r.lima != "" {
		_, err := r.runner.Run(ctx, runtimes.Command{
			Name: "limactl",
			Args: []string{
				"shell", r.lima, "--", "rm", "-f", "--", logPath,
			},
		})
		if err != nil {
			return fmt.Errorf("remove Lima container log %q: %w", logPath, err)
		}
		return nil
	}
	if err := os.Remove(logPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove direct container log %q: %w", logPath, err)
	}
	return nil
}

func (r *grpcContainerRuntime) getTask(ctx context.Context, id string) (*tasktypes.Process, error) {
	response, err := r.tasks.Get(
		withContainerdNamespace(ctx, r.namespace),
		&tasksapi.GetRequest{ContainerID: id},
	)
	if err != nil {
		return nil, err
	}
	return response.GetProcess(), nil
}

func (r *grpcContainerRuntime) resolveContainerID(ctx context.Context, identifier string) (string, error) {
	if r.containers == nil {
		return identifier, nil
	}
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	response, err := r.containers.Get(
		namespacedContext,
		&containersapi.GetContainerRequest{ID: identifier},
	)
	if err == nil && response.GetContainer().GetID() != "" {
		return response.GetContainer().GetID(), nil
	}
	if err != nil && status.Code(err) != codes.NotFound {
		return "", containerdOperationError("resolve", identifier, err)
	}
	list, err := r.containers.List(namespacedContext, &containersapi.ListContainersRequest{})
	if err != nil {
		return "", containerdOperationError("list while resolving", identifier, err)
	}
	name := strings.TrimPrefix(identifier, "/")
	matches := make([]string, 0, 2)
	for _, record := range list.GetContainers() {
		if record.GetLabels()[nerdctlNameLabel] == name || strings.HasPrefix(record.GetID(), identifier) {
			matches = append(matches, record.GetID())
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: container %q", ErrNotFound, identifier)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%w: container identifier %q is ambiguous", ErrConflict, identifier)
	}
}

func (r *grpcContainerRuntime) requireContainer(ctx context.Context, id string) error {
	_, err := r.containers.Get(
		withContainerdNamespace(ctx, r.namespace),
		&containersapi.GetContainerRequest{ID: id},
	)
	return containerdOperationError("inspect", id, err)
}

func (r *grpcContainerRuntime) stopOptions(
	ctx context.Context,
	id string,
	timeoutSeconds int,
) (uint32, time.Duration, error) {
	response, err := r.containers.Get(
		withContainerdNamespace(ctx, r.namespace),
		&containersapi.GetContainerRequest{ID: id},
	)
	if err != nil {
		return 0, 0, containerdOperationError("inspect stop settings for", id, err)
	}
	container := containerFromContainerd(response.GetContainer(), nil)
	stopSignal := container.StopSignal
	if stopSignal == "" {
		stopSignal = "SIGTERM"
	}
	signal, err := parseContainerSignal(stopSignal)
	if err != nil {
		return 0, 0, fmt.Errorf("decode stop signal for container %q: %w", id, err)
	}
	stopTimeout := defaultContainerStopTimeout
	if container.StopTimeout > 0 {
		stopTimeout = time.Duration(container.StopTimeout) * time.Second
	}
	if timeoutSeconds > 0 {
		stopTimeout = time.Duration(timeoutSeconds) * time.Second
	}
	return signal, stopTimeout, nil
}

func containerdTaskActive(state tasktypes.Status) bool {
	switch state {
	case tasktypes.Status_RUNNING, tasktypes.Status_PAUSED, tasktypes.Status_PAUSING:
		return true
	default:
		return false
	}
}

func taskRecreationUnsupportedError(id, state string) error {
	return fmt.Errorf(
		"%w: %w for container %q because %s; containerd task creation also requires daemon-local nerdctl logging and FIFO setup",
		ErrUnsupported,
		ErrTaskRecreationRequired,
		id,
		state,
	)
}

func containerdOperationError(operation, id string, err error) error {
	if err == nil {
		return nil
	}
	switch status.Code(err) {
	case codes.Canceled:
		return fmt.Errorf("%s container %q: %w", operation, id, errors.Join(context.Canceled, err))
	case codes.DeadlineExceeded:
		return fmt.Errorf("%s container %q: %w", operation, id, errors.Join(context.DeadlineExceeded, err))
	case codes.NotFound:
		return fmt.Errorf("%w: container %q: %v", ErrNotFound, id, err)
	case codes.AlreadyExists, codes.FailedPrecondition, codes.Aborted:
		return fmt.Errorf("%w: %s container %q: %v", ErrConflict, operation, id, err)
	case codes.Unavailable:
		return fmt.Errorf("%w: %s container %q: %v", ErrUnavailable, operation, id, err)
	case codes.Unimplemented:
		return fmt.Errorf("%w: %s container %q: %v", ErrUnsupported, operation, id, err)
	default:
		return fmt.Errorf("%s container %q: %w", operation, id, err)
	}
}

func parseContainerSignal(value string) (uint32, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "" {
		return 9, nil
	}
	if number, err := strconv.ParseUint(value, 10, 32); err == nil {
		return uint32(number), nil
	}
	value = strings.TrimPrefix(value, "SIG")
	signals := map[string]uint32{
		"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6,
		"BUS": 7, "FPE": 8, "KILL": 9, "USR1": 10, "SEGV": 11, "USR2": 12,
		"PIPE": 13, "ALRM": 14, "TERM": 15, "CHLD": 17, "CONT": 18, "STOP": 19,
		"TSTP": 20, "TTIN": 21, "TTOU": 22, "URG": 23, "XCPU": 24, "XFSZ": 25,
		"VTALRM": 26, "PROF": 27, "WINCH": 28, "IO": 29, "PWR": 30, "SYS": 31,
	}
	signal, ok := signals[value]
	if !ok {
		encoded, _ := json.Marshal(value)
		return 0, fmt.Errorf("invalid container signal %s", encoded)
	}
	return signal, nil
}
