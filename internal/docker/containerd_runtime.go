package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	eventsapi "github.com/containerd/containerd/api/services/events/v1"
	namespacesapi "github.com/containerd/containerd/api/services/namespaces/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	containerdNamespaceHeader = "containerd-namespace"

	nerdctlNameLabel           = "nerdctl/name"
	nerdctlNetworksLabel       = "nerdctl/networks"
	nerdctlPortsLabel          = "nerdctl/ports"
	nerdctlImageDigestLabel    = "nerdctl/image-digest"
	nerdctlStopTimeoutLabel    = "nerdctl/stop-timeout"
	nerdctlHealthcheckLabel    = "nerdctl/healthcheck"
	nerdctlHealthStateLabel    = "nerdctl/healthstate"
	nerdctlErrorLabel          = "nerdctl/error"
	restartPolicyLabel         = "containerd.io/restart.policy"
	restartCountLabel          = "containerd.io/restart.count"
	restartStatusLabel         = "containerd.io/restart.status"
	defaultContainerdNamespace = "default"
)

const limaContainerdDiscoveryCommand = `
namespace="${CONTAINERD_NAMESPACE:-default}"
for socket in \
  "${CONTAINERD_ADDRESS:-}" \
  "$XDG_RUNTIME_DIR/containerd-rootless/containerd.sock" \
  "$XDG_RUNTIME_DIR/containerd/containerd.sock" \
  "/run/user/$(id -u)/containerd-rootless/containerd.sock" \
  "/run/user/$(id -u)/containerd/containerd.sock" \
  "/run/containerd/containerd.sock"
do
  if [ -n "$socket" ] && [ -S "$socket" ]; then
    printf '%s\n%s\n' "$socket" "$namespace"
    exit 0
  fi
done
echo "containerd socket is unavailable" >&2
exit 1
`

type grpcContainerRuntime struct {
	connection *grpc.ClientConn
	namespace  string
	backend    string
	containers containersapi.ContainersClient
	tasks      tasksapi.TasksClient
	events     eventsapi.EventsClient
	enrich     func(context.Context) ([]Container, error)

	enrichMu          sync.Mutex
	enrichmentReady   bool
	enrichmentVersion map[string]string
	enrichment        map[string]Container
	enrichmentError   error
}

func (m *Manager) connectContainerRuntime(ctx context.Context) (containerRuntime, error) {
	backend, err := m.backend(ctx)
	if err != nil {
		return nil, err
	}
	namespace := configuredContainerdNamespace()
	if backend.name == "limactl" {
		if len(backend.prefix) < 2 {
			return nil, errors.New("Porto Lima backend configuration is incomplete")
		}
		instance := backend.prefix[1]
		socket, discoveredNamespace, err := m.discoverLimaContainerd(ctx, instance)
		if err != nil {
			return nil, err
		}
		if discoveredNamespace != "" {
			namespace = discoveredNamespace
		}
		sshConfig, err := m.discoverLimaSSHConfig(ctx, instance)
		if err != nil {
			return nil, err
		}
		return newGRPCContainerRuntime(
			ctx,
			namespace,
			backend.description,
			func(dialContext context.Context, _ string) (net.Conn, error) {
				return dialLimaContainerd(dialContext, instance, sshConfig, socket)
			},
			m.containerInventoryEnricher(backend),
		)
	}

	var dialErrors []error
	for _, address := range localContainerdAddresses() {
		runtimeClient, err := newGRPCContainerRuntime(
			ctx,
			namespace,
			backend.description,
			func(dialContext context.Context, _ string) (net.Conn, error) {
				return dialLocalContainerd(dialContext, address)
			},
			m.containerInventoryEnricher(backend),
		)
		if err == nil {
			return runtimeClient, nil
		}
		dialErrors = append(dialErrors, fmt.Errorf("%s: %w", address, err))
	}
	return nil, fmt.Errorf("%w; containerd socket discovery failed: %w", ErrUnavailable, errors.Join(dialErrors...))
}

func newGRPCContainerRuntime(
	ctx context.Context,
	namespace string,
	backend string,
	dialer func(context.Context, string) (net.Conn, error),
	enrich func(context.Context) ([]Container, error),
) (*grpcContainerRuntime, error) {
	connection, err := grpc.NewClient(
		"passthrough:///porto-containerd",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		return nil, fmt.Errorf("create containerd client: %w", err)
	}
	runtimeClient := &grpcContainerRuntime{
		connection:        connection,
		namespace:         namespace,
		backend:           backend,
		containers:        containersapi.NewContainersClient(connection),
		tasks:             tasksapi.NewTasksClient(connection),
		events:            eventsapi.NewEventsClient(connection),
		enrich:            enrich,
		enrichmentVersion: map[string]string{},
		enrichment:        map[string]Container{},
	}
	validationContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	health, err := grpc_health_v1.NewHealthClient(connection).Check(
		validationContext,
		&grpc_health_v1.HealthCheckRequest{},
		grpc.WaitForReady(true),
	)
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("check containerd health: %w", err)
	}
	if health.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
		_ = connection.Close()
		return nil, fmt.Errorf("%w; containerd health is %s", ErrUnavailable, health.GetStatus())
	}
	namespaces, err := namespacesapi.NewNamespacesClient(connection).List(
		validationContext,
		&namespacesapi.ListNamespacesRequest{},
	)
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("list containerd namespaces: %w", err)
	}
	for _, candidate := range namespaces.GetNamespaces() {
		if candidate.GetName() == namespace {
			return runtimeClient, nil
		}
	}
	_ = connection.Close()
	return nil, fmt.Errorf("%w; containerd namespace %q does not exist", ErrUnavailable, namespace)
}

func configuredContainerdNamespace() string {
	for _, name := range []string{"CONTAINERD_NAMESPACE", "NERDCTL_NAMESPACE"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return defaultContainerdNamespace
}

func normalizeContainerdAddress(address string) string {
	address = strings.TrimSpace(address)
	address = strings.TrimPrefix(address, "unix://")
	address = strings.TrimPrefix(address, "npipe://")
	return address
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func (m *Manager) discoverLimaContainerd(ctx context.Context, instance string) (string, string, error) {
	output, err := m.runCommand(
		ctx,
		20*time.Second,
		"discover Lima containerd socket",
		nil,
		"limactl",
		"shell",
		instance,
		"--",
		"sh",
		"-lc",
		limaContainerdDiscoveryCommand,
	)
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) == "" || strings.TrimSpace(lines[1]) == "" {
		return "", "", errors.New("Lima containerd discovery returned an incomplete socket and namespace")
	}
	return strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), nil
}

func (m *Manager) discoverLimaSSHConfig(ctx context.Context, instance string) (string, error) {
	output, err := m.runCommand(
		ctx,
		10*time.Second,
		"discover Lima SSH configuration",
		nil,
		"limactl",
		"list",
		"--format",
		"{{.SSHConfigFile}}",
		instance,
	)
	if err != nil {
		return "", err
	}
	configPath := strings.TrimSpace(string(output))
	if configPath == "" {
		return "", errors.New("Lima SSH configuration path is unavailable")
	}
	return configPath, nil
}

func dialLimaContainerd(ctx context.Context, instance, sshConfig, socket string) (net.Conn, error) {
	directory, err := os.MkdirTemp("", "porto-containerd-tunnel-*")
	if err != nil {
		return nil, fmt.Errorf("create containerd tunnel directory: %w", err)
	}
	localSocket := filepath.Join(directory, "containerd.sock")
	command := exec.Command(
		"ssh",
		"-F",
		sshConfig,
		"-N",
		"-T",
		"-o",
		"ExitOnForwardFailure=yes",
		"-o",
		"StreamLocalBindUnlink=yes",
		"-L",
		localSocket+":"+socket,
		"lima-"+instance,
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("start containerd SSH tunnel: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
	}()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			_ = os.RemoveAll(directory)
			message := strings.TrimSpace(stderr.String())
			if message == "" && err != nil {
				message = err.Error()
			}
			return nil, fmt.Errorf("containerd SSH tunnel exited before connecting: %s", message)
		case <-ctx.Done():
			_ = command.Process.Kill()
			<-done
			_ = os.RemoveAll(directory)
			return nil, context.Cause(ctx)
		case <-timeout.C:
			_ = command.Process.Kill()
			<-done
			_ = os.RemoveAll(directory)
			return nil, errors.New("timed out opening containerd SSH tunnel")
		case <-ticker.C:
			connection, err := (&net.Dialer{}).DialContext(ctx, "unix", localSocket)
			if err == nil {
				return &limaContainerdConn{
					Conn:      connection,
					command:   command,
					done:      done,
					directory: directory,
				}, nil
			}
		}
	}
}

func (m *Manager) containerInventoryEnricher(backend commandBackend) func(context.Context) ([]Container, error) {
	return func(ctx context.Context) ([]Container, error) {
		output, err := m.runBackend(
			ctx,
			backend,
			defaultInventoryOperationTimeout,
			"read Porto container compatibility metadata",
			nil,
			"ps",
			"-a",
			"--no-trunc",
			"--format",
			"{{json .}}",
		)
		if err != nil {
			return nil, err
		}
		return decodeNerdctlContainers(output)
	}
}

type limaContainerdConn struct {
	net.Conn
	command   *exec.Cmd
	done      chan error
	directory string
	once      sync.Once
}

func (connection *limaContainerdConn) Close() error {
	var closeErr error
	connection.once.Do(func() {
		closeErr = connection.Conn.Close()
		if connection.command.Process != nil {
			if err := connection.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				closeErr = errors.Join(closeErr, err)
			}
		}
		select {
		case err := <-connection.done:
			if err != nil && !strings.Contains(strings.ToLower(err.Error()), "signal: killed") {
				closeErr = errors.Join(closeErr, fmt.Errorf("containerd SSH tunnel: %w", err))
			}
		case <-time.After(5 * time.Second):
			closeErr = errors.Join(closeErr, errors.New("timed out stopping containerd SSH tunnel"))
		}
		closeErr = errors.Join(closeErr, os.RemoveAll(connection.directory))
	})
	return closeErr
}

func (r *grpcContainerRuntime) Namespace() string { return r.namespace }
func (r *grpcContainerRuntime) Backend() string   { return r.backend }

func (r *grpcContainerRuntime) Snapshot(ctx context.Context) ([]Container, error) {
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	response, err := r.containers.List(namespacedContext, &containersapi.ListContainersRequest{})
	if err != nil {
		return nil, fmt.Errorf("list containerd containers: %w", err)
	}
	records := response.GetContainers()
	enrichment, enrichmentErr := r.compatibilityMetadata(ctx, records)
	containers := make([]Container, 0, len(records))
	for _, record := range records {
		taskResponse, taskErr := r.tasks.Get(namespacedContext, &tasksapi.GetRequest{ContainerID: record.GetID()})
		var process *tasktypes.Process
		if taskErr == nil {
			process = taskResponse.GetProcess()
		} else if grpcstatus.Code(taskErr) != codes.NotFound {
			mapped := containerFromContainerd(record, nil)
			mapped.InventoryError = fmt.Sprintf("read task state: %v", taskErr)
			mergeContainerCompatibilityMetadata(&mapped, enrichment[record.GetID()])
			mapped.InventoryError = combineInventoryError(mapped.InventoryError, enrichmentErr)
			containers = append(containers, mapped)
			continue
		}
		mapped := containerFromContainerd(record, process)
		mergeContainerCompatibilityMetadata(&mapped, enrichment[record.GetID()])
		mapped.InventoryError = combineInventoryError(mapped.InventoryError, enrichmentErr)
		containers = append(containers, mapped)
	}
	return containers, nil
}

func (r *grpcContainerRuntime) compatibilityMetadata(
	ctx context.Context,
	records []*containersapi.Container,
) (map[string]Container, error) {
	r.enrichMu.Lock()
	defer r.enrichMu.Unlock()
	currentVersions := make(map[string]string, len(records))
	needsRefresh := !r.enrichmentReady
	for _, record := range records {
		version := containerMetadataVersion(record)
		currentVersions[record.GetID()] = version
		if r.enrichmentVersion[record.GetID()] != version {
			needsRefresh = true
		}
	}
	if len(currentVersions) != len(r.enrichmentVersion) {
		needsRefresh = true
	}
	if needsRefresh && r.enrich != nil {
		enriched, err := r.enrich(ctx)
		if err != nil {
			r.enrichmentError = fmt.Errorf("read nerdctl compatibility metadata: %w", err)
		} else {
			r.enrichment = make(map[string]Container, len(enriched))
			for _, container := range enriched {
				r.enrichment[container.ID] = container
			}
			r.enrichmentVersion = currentVersions
			r.enrichmentReady = true
			r.enrichmentError = nil
		}
	}
	result := make(map[string]Container, len(r.enrichment))
	for id, container := range r.enrichment {
		if _, exists := currentVersions[id]; exists {
			result[id] = cloneContainer(container)
		}
	}
	return result, r.enrichmentError
}

func containerMetadataVersion(record *containersapi.Container) string {
	updatedAt := timestampString(record.GetUpdatedAt())
	return strings.Join([]string{
		updatedAt,
		record.GetLabels()[nerdctlNetworksLabel],
		record.GetLabels()[nerdctlPortsLabel],
		record.GetLabels()[nerdctlNameLabel],
	}, "\x00")
}

func mergeContainerCompatibilityMetadata(container *Container, compatibility Container) {
	if compatibility.ID == "" {
		return
	}
	if !container.TaskPresent && compatibility.State != "" {
		container.State = compatibility.State
		container.Status = compatibility.Status
		if code, ok := parseCompatibilityExitCode(compatibility.Status); ok {
			container.ExitCode = &code
			container.ExitReason = exitReason(code)
			container.ExitSignal = exitSignal(code)
		}
	}
	if compatibility.Ports != "" {
		container.Ports = compatibility.Ports
		hasStructuredPorts := false
		for _, network := range container.NetworkDetails {
			if network.ContainerPort > 0 {
				hasStructuredPorts = true
				break
			}
		}
		if !hasStructuredPorts {
			container.NetworkDetails = append(container.NetworkDetails, parseCompatibilityPorts(compatibility.Ports)...)
		}
	}
	if compatibility.Networks != "" {
		container.Networks = compatibility.Networks
	}
	if compatibility.Mounts != "" {
		container.Mounts = compatibility.Mounts
	}
}

func parseCompatibilityExitCode(status string) (uint32, bool) {
	const prefix = "Exited ("
	start := strings.Index(status, prefix)
	if start < 0 {
		return 0, false
	}
	value := status[start+len(prefix):]
	end := strings.IndexByte(value, ')')
	if end < 0 {
		return 0, false
	}
	parsed, err := strconv.ParseUint(value[:end], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(parsed), true
}

func parseCompatibilityPorts(value string) []ContainerNetworkState {
	var result []ContainerNetworkState
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		left, right, mapped := strings.Cut(entry, "->")
		target := right
		if !mapped {
			target = left
		}
		portText, protocol, _ := strings.Cut(target, "/")
		containerPort, err := strconv.ParseInt(strings.TrimSpace(portText), 10, 32)
		if err != nil {
			continue
		}
		state := ContainerNetworkState{
			ContainerPort: int32(containerPort),
			Protocol:      firstNonEmpty(protocol, "tcp"),
		}
		if mapped {
			host := strings.TrimSpace(left)
			if index := strings.LastIndexByte(host, ':'); index >= 0 {
				state.HostIP = strings.Trim(host[:index], "[]")
				host = host[index+1:]
			}
			hostPort, err := strconv.ParseInt(host, 10, 32)
			if err != nil {
				continue
			}
			state.HostPort = int32(hostPort)
		}
		result = append(result, state)
	}
	return result
}

func combineInventoryError(existing string, err error) string {
	if err == nil {
		return existing
	}
	if existing == "" {
		return err.Error()
	}
	return errors.Join(errors.New(existing), err).Error()
}

func (r *grpcContainerRuntime) Subscribe(ctx context.Context) (<-chan ContainerLifecycleEvent, <-chan error) {
	events := make(chan ContainerLifecycleEvent, 128)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		stream, err := r.events.Subscribe(
			withContainerdNamespace(ctx, r.namespace),
			&eventsapi.SubscribeRequest{},
			grpc.WaitForReady(true),
		)
		if err != nil {
			errs <- err
			return
		}
		for {
			envelope, err := stream.Recv()
			if err != nil {
				if ctx.Err() == nil {
					errs <- err
				}
				return
			}
			event, relevant := containerLifecycleEvent(envelope)
			if !relevant {
				continue
			}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, errs
}

func (r *grpcContainerRuntime) Close() error {
	return r.connection.Close()
}

func withContainerdNamespace(ctx context.Context, namespace string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, containerdNamespaceHeader, namespace)
}

func containerFromContainerd(record *containersapi.Container, process *tasktypes.Process) Container {
	labels := cloneStringMap(record.GetLabels())
	container := Container{
		ID:             record.GetID(),
		Name:           firstNonEmpty(labels[nerdctlNameLabel], record.GetID()),
		Image:          record.GetImage(),
		ImageID:        labels[nerdctlImageDigestLabel],
		State:          "created",
		Status:         "Created",
		CreatedAt:      timestampString(record.GetCreatedAt()),
		UpdatedAt:      timestampString(record.GetUpdatedAt()),
		Labels:         labels,
		ComposeProject: labels["com.docker.compose.project"],
		ComposeService: labels["com.docker.compose.service"],
		RestartPolicy:  labels[restartPolicyLabel],
		Health:         ContainerHealth{Status: "disabled"},
		StopTimeout:    parsePositiveInt(labels[nerdctlStopTimeoutLabel]),
		ExitReason:     labels[nerdctlErrorLabel],
	}
	container.RestartCount, _ = strconv.Atoi(labels[restartCountLabel])
	if labels[restartStatusLabel] == "running" && container.RestartPolicy != "" {
		container.State = "restarting"
		container.Status = "Restarting"
	}

	var mappingErrors []error
	var spec specs.Spec
	if record.GetSpec() != nil && len(record.GetSpec().GetValue()) > 0 {
		if err := json.Unmarshal(record.GetSpec().GetValue(), &spec); err != nil {
			mappingErrors = append(mappingErrors, fmt.Errorf("decode OCI spec: %w", err))
		} else {
			mapContainerSpec(&container, &spec)
		}
	}
	if err := mapContainerNetworks(&container, labels); err != nil {
		mappingErrors = append(mappingErrors, err)
	}
	if err := mapContainerHealth(&container, labels); err != nil {
		mappingErrors = append(mappingErrors, err)
	}
	mapContainerProcess(&container, process)
	if len(mappingErrors) > 0 {
		container.InventoryError = errors.Join(mappingErrors...).Error()
	}
	return container
}

func mapContainerSpec(container *Container, spec *specs.Spec) {
	container.Annotations = cloneStringMap(spec.Annotations)
	if spec.Process != nil {
		container.Command = strings.Join(spec.Process.Args, " ")
	}
	for _, key := range []string{
		"org.opencontainers.image.stopSignal",
		"org.opencontainers.image.stop-signal",
		"io.containerd.image.config.stop-signal",
	} {
		if value := spec.Annotations[key]; value != "" {
			container.StopSignal = value
			break
		}
	}
	container.MountDetails = make([]ContainerMount, 0, len(spec.Mounts))
	var mountNames []string
	for _, mount := range spec.Mounts {
		container.MountDetails = append(container.MountDetails, ContainerMount{
			Type:        mount.Type,
			Source:      mount.Source,
			Destination: mount.Destination,
			Options:     append([]string(nil), mount.Options...),
		})
		mountNames = append(mountNames, mount.Destination)
	}
	container.Mounts = strings.Join(mountNames, ", ")
	if spec.Linux == nil || spec.Linux.Resources == nil {
		return
	}
	resources := spec.Linux.Resources
	if resources.CPU != nil {
		if resources.CPU.Quota != nil {
			container.Resources.CPUQuota = *resources.CPU.Quota
		}
		if resources.CPU.Period != nil {
			container.Resources.CPUPeriod = *resources.CPU.Period
		}
		if resources.CPU.Shares != nil {
			container.Resources.CPUShares = *resources.CPU.Shares
		}
		container.Resources.CPUSet = resources.CPU.Cpus
	}
	if resources.Memory != nil {
		if resources.Memory.Limit != nil {
			container.Resources.MemoryLimit = *resources.Memory.Limit
		}
		if resources.Memory.Swap != nil {
			container.Resources.MemorySwap = *resources.Memory.Swap
		}
	}
	if resources.Pids != nil && resources.Pids.Limit != nil {
		container.Resources.PIDsLimit = *resources.Pids.Limit
	}
}

func mapContainerNetworks(container *Container, labels map[string]string) error {
	var networks []string
	if encoded := labels[nerdctlNetworksLabel]; encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &networks); err != nil {
			return fmt.Errorf("decode container networks: %w", err)
		}
	}
	sort.Strings(networks)
	container.Networks = strings.Join(networks, ", ")
	for _, network := range networks {
		container.NetworkDetails = append(container.NetworkDetails, ContainerNetworkState{Name: network})
	}
	if encoded := labels[nerdctlPortsLabel]; encoded != "" {
		var ports []struct {
			HostIP        string `json:"hostIP"`
			HostPort      int32  `json:"hostPort"`
			ContainerPort int32  `json:"containerPort"`
			Protocol      string `json:"protocol"`
		}
		if err := json.Unmarshal([]byte(encoded), &ports); err != nil {
			return fmt.Errorf("decode container ports: %w", err)
		}
		var formatted []string
		for _, port := range ports {
			protocol := firstNonEmpty(port.Protocol, "tcp")
			host := port.HostIP
			if port.HostPort > 0 {
				if host != "" {
					formatted = append(formatted, fmt.Sprintf("%s:%d->%d/%s", host, port.HostPort, port.ContainerPort, protocol))
				} else {
					formatted = append(formatted, fmt.Sprintf("%d->%d/%s", port.HostPort, port.ContainerPort, protocol))
				}
			} else {
				formatted = append(formatted, fmt.Sprintf("%d/%s", port.ContainerPort, protocol))
			}
			container.NetworkDetails = append(container.NetworkDetails, ContainerNetworkState{
				HostIP:        host,
				HostPort:      port.HostPort,
				ContainerPort: port.ContainerPort,
				Protocol:      protocol,
			})
		}
		sort.Strings(formatted)
		container.Ports = strings.Join(formatted, ", ")
	}
	return nil
}

func mapContainerHealth(container *Container, labels map[string]string) error {
	if labels[nerdctlHealthcheckLabel] == "" {
		container.Health.Status = "disabled"
		return nil
	}
	container.Health.Status = "starting"
	encoded := labels[nerdctlHealthStateLabel]
	if encoded == "" {
		return nil
	}
	var state struct {
		Status        string
		FailingStreak int
	}
	if err := json.Unmarshal([]byte(encoded), &state); err != nil {
		return fmt.Errorf("decode container health state: %w", err)
	}
	container.Health.Status = firstNonEmpty(state.Status, "starting")
	container.Health.FailingStreak = state.FailingStreak
	container.Health.UpdatedAt = container.UpdatedAt
	return nil
}

func mapContainerProcess(container *Container, process *tasktypes.Process) {
	if process == nil {
		return
	}
	container.TaskPresent = true
	container.PID = process.GetPid()
	state := strings.ToLower(process.GetStatus().String())
	switch state {
	case "running":
		container.State = "running"
		container.Status = "Up"
	case "paused", "pausing":
		container.State = state
		container.Status = "Up (" + strings.ToUpper(state[:1]) + state[1:] + ")"
	case "stopped":
		container.State = "exited"
		exitCode := process.GetExitStatus()
		container.ExitCode = &exitCode
		container.ExitAt = timestampString(process.GetExitedAt())
		container.ExitReason = exitReason(exitCode)
		if signal := exitSignal(exitCode); signal != nil {
			container.ExitSignal = signal
		}
		container.Status = fmt.Sprintf("Exited (%d)", exitCode)
		if container.ExitAt != "" {
			container.Status += " " + container.ExitAt
		}
	case "created":
		container.State = "created"
		container.Status = "Created"
	default:
		container.State = "unknown"
		container.Status = "Unknown"
	}
	if container.Health.Status != "disabled" && container.State == "running" {
		container.Status += " (" + container.Health.Status + ")"
	}
}

func containerLifecycleEvent(envelope *eventsapi.Envelope) (ContainerLifecycleEvent, bool) {
	if envelope == nil {
		return ContainerLifecycleEvent{}, false
	}
	topic := envelope.GetTopic()
	event := ContainerLifecycleEvent{
		Topic:     topic,
		Type:      lifecycleEventType(topic),
		Timestamp: timestampValue(envelope.GetTimestamp()),
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	if !relevantContainerdTopic(topic) {
		return event, false
	}
	if envelope.GetEvent() == nil {
		event.Reason = "event payload unavailable"
		return event, true
	}
	decode := func(value proto.Message) bool {
		if err := proto.Unmarshal(envelope.GetEvent().GetValue(), value); err != nil {
			event.Reason = "partial event payload: " + err.Error()
			return false
		}
		return true
	}
	switch topic {
	case "/containers/create":
		var value eventtypes.ContainerCreate
		if decode(&value) {
			event.ContainerID = value.GetID()
			event.Reason = "metadata-created"
		}
	case "/containers/update":
		var value eventtypes.ContainerUpdate
		if decode(&value) {
			event.ContainerID = value.GetID()
			event.Reason = "metadata-updated"
		}
	case "/containers/delete":
		var value eventtypes.ContainerDelete
		if decode(&value) {
			event.ContainerID = value.GetID()
			event.Reason = "metadata-deleted"
		}
	case "/tasks/create":
		var value eventtypes.TaskCreate
		if decode(&value) {
			event.ContainerID = value.GetContainerID()
			event.Reason = "task-created"
		}
	case "/tasks/start":
		var value eventtypes.TaskStart
		if decode(&value) {
			event.ContainerID = value.GetContainerID()
			event.Reason = "started"
		}
	case "/tasks/delete":
		var value eventtypes.TaskDelete
		if decode(&value) {
			event.ContainerID = value.GetContainerID()
			event.ExecID = value.GetID()
			event.ExitCode = uint32Pointer(value.GetExitStatus())
			event.Timestamp = firstNonZeroTime(timestampValue(value.GetExitedAt()), event.Timestamp)
			event.Reason = exitReason(value.GetExitStatus())
			if event.ExecID != "" && event.ExecID != event.ContainerID {
				event.Type = "exec-delete"
			}
		}
	case "/tasks/exit":
		var value eventtypes.TaskExit
		if decode(&value) {
			event.ContainerID = value.GetContainerID()
			event.ExecID = value.GetID()
			event.ExitCode = uint32Pointer(value.GetExitStatus())
			event.ExitSignal = exitSignal(value.GetExitStatus())
			event.Timestamp = firstNonZeroTime(timestampValue(value.GetExitedAt()), event.Timestamp)
			event.Reason = exitReason(value.GetExitStatus())
			if event.ExecID != "" && event.ExecID != event.ContainerID {
				event.Type = "exec-exit"
			}
		}
	case "/tasks/oom":
		var value eventtypes.TaskOOM
		if decode(&value) {
			event.ContainerID = value.GetContainerID()
			event.OOM = true
			event.Reason = "oom"
		}
	case "/tasks/exec-added":
		var value eventtypes.TaskExecAdded
		if decode(&value) {
			event.ContainerID = value.GetContainerID()
			event.ExecID = value.GetExecID()
			event.Reason = "exec-created"
		}
	case "/tasks/exec-started":
		var value eventtypes.TaskExecStarted
		if decode(&value) {
			event.ContainerID = value.GetContainerID()
			event.ExecID = value.GetExecID()
			event.Reason = "exec-started"
		}
	case "/tasks/paused":
		var value eventtypes.TaskPaused
		if decode(&value) {
			event.ContainerID = value.GetContainerID()
			event.Reason = "paused"
		}
	case "/tasks/resumed":
		var value eventtypes.TaskResumed
		if decode(&value) {
			event.ContainerID = value.GetContainerID()
			event.Reason = "resumed"
		}
	case "/tasks/checkpointed":
		var value eventtypes.TaskCheckpointed
		if decode(&value) {
			event.ContainerID = value.GetContainerID()
			event.Reason = "checkpointed"
		}
	default:
		event.Reason = strings.Trim(strings.ReplaceAll(topic, "/", " "), " ")
	}
	return event, true
}

func relevantContainerdTopic(topic string) bool {
	for _, prefix := range []string{
		"/containers/",
		"/tasks/",
		"/images/",
		"/namespaces/",
		"/snapshots/",
		"/sandboxes/",
	} {
		if strings.HasPrefix(topic, prefix) {
			return true
		}
	}
	return false
}

func lifecycleEventType(topic string) string {
	parts := strings.Split(strings.Trim(topic, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "runtime-event"
	}
	category := map[string]string{
		"containers": "container",
		"tasks":      "task",
		"images":     "image",
		"namespaces": "namespace",
		"snapshots":  "snapshot",
		"sandboxes":  "sandbox",
	}[parts[0]]
	if category == "" {
		category = strings.TrimSuffix(parts[0], "s")
	}
	if len(parts) == 1 {
		return category
	}
	return category + "-" + strings.Join(parts[1:], "-")
}

func exitReason(code uint32) string {
	switch {
	case code == 0:
		return "completed"
	case code >= 129 && code <= 255:
		return "signal"
	default:
		return "error"
	}
}

func exitSignal(code uint32) *uint32 {
	if code < 129 || code > 255 {
		return nil
	}
	return uint32Pointer(code - 128)
}

func uint32Pointer(value uint32) *uint32 {
	return &value
}

func parsePositiveInt(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func timestampString(value *timestamppb.Timestamp) string {
	if value == nil {
		return ""
	}
	timestamp := value.AsTime()
	if timestamp.IsZero() {
		return ""
	}
	return timestamp.UTC().Format(time.RFC3339Nano)
}

func timestampValue(value *timestamppb.Timestamp) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.AsTime().UTC()
}

func firstNonZeroTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}
