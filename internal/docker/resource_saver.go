package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"github.com/mbianchidev/porto/internal/resourcesaver"
	"github.com/mbianchidev/porto/internal/runtimes"
)

const (
	ResourceSaverLabel         = "dev.porto.resource-saver"
	ResourceSaverStatefulLabel = "dev.porto.resource-saver.stateful"
	ManagedWorkloadLabel       = "dev.porto.managed"
	resourceSaverOwnerLabel    = "io.porto.resource-saver.owner"
)

type resourceSaverContextKey struct{}

type resourceSaverOwner struct {
	Token    string `json:"token"`
	Identity string `json:"identity"`
	PID      uint32 `json:"pid"`
	Phase    string `json:"phase"`
	Mode     string `json:"mode"`
}

type ResourceSaverActivity struct {
	PID        uint32
	RxBytes    uint64
	TxBytes    uint64
	ReadBytes  uint64
	WriteBytes uint64
}

func (m *Manager) ResourceSaverActivity(ctx context.Context, id string) (ResourceSaverActivity, error) {
	snapshot := m.ContainerSnapshot()
	if !snapshot.Available || snapshot.Stale {
		return ResourceSaverActivity{}, fmt.Errorf("%w: a fresh inventory is required for idle counters", ErrUnavailable)
	}
	container, err := findSnapshotContainer(snapshot, id)
	if err != nil || container.PID == 0 {
		return ResourceSaverActivity{}, fmt.Errorf("%w: the running task identity is unavailable for idle counters", ErrUnavailable)
	}
	backend, err := m.backendForCommand(ctx)
	if err != nil {
		return ResourceSaverActivity{}, err
	}
	const script = `set -eu
root="/proc/$1"
awk 'NR>2 { gsub(/:/," "); rx+=$2; tx+=$10 } END { printf "%.0f %.0f\n",rx,tx }' "$root/net/dev"
awk '/^read_bytes:/ {read=$2; have_read=1} /^write_bytes:/ {write=$2; have_write=1}
END {if (!have_read || !have_write) exit 1; printf "%.0f %.0f\n",read,write}' "$root/io"
`
	arguments := []string{"-c", script, "porto-idle-counters", strconv.FormatUint(uint64(container.PID), 10)}
	command := runtimes.Command{Name: "sh", Args: arguments}
	if backend.limaInstance != "" {
		command.Name = backend.name
		command.Args = append([]string{"shell", "--workdir=/", backend.limaInstance, "--", "sudo", "-n", "sh"}, arguments...)
	}
	output, err := m.runner.Run(ctx, command)
	if err != nil {
		return ResourceSaverActivity{}, runtimes.CommandError("read real task network and I/O counters", output, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 4 {
		return ResourceSaverActivity{}, errors.New("backend returned incomplete task network/I/O counters")
	}
	var values [4]uint64
	for index, field := range fields {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return ResourceSaverActivity{}, fmt.Errorf("decode real task activity: %w", err)
		}
		values[index] = value
	}
	return ResourceSaverActivity{PID: container.PID, RxBytes: values[0], TxBytes: values[1], ReadBytes: values[2], WriteBytes: values[3]}, nil
}

func (m *Manager) SetResourceSaver(manager *resourcesaver.Manager) { m.resourceSaver.Store(manager) }

func (m *Manager) ResourceSaver() *resourcesaver.Manager { return m.resourceSaver.Load() }

func ResourceSaverIdentity(container Container) string {
	created, err := time.Parse(time.RFC3339Nano, container.CreatedAt)
	if err != nil {
		return ""
	}
	return container.ID + "@" + created.UTC().Format(time.RFC3339Nano)
}

func (m *Manager) resourceSaverRuntime(ctx context.Context) (*grpcContainerRuntime, func() error, error) {
	client, err := m.runtimeConnector(ctx)
	if err != nil {
		return nil, nil, err
	}
	backend, ok := client.(*grpcContainerRuntime)
	if !ok || backend.client == nil {
		return nil, nil, errors.Join(fmt.Errorf("%w: Resource Saver requires direct containerd ownership checks", ErrUnsupported), client.Close())
	}
	return backend, client.Close, nil
}

func (m *Manager) SetResourceSaverPolicy(ctx context.Context, id, mode string, stateful bool) (err error) {
	if mode != "" && mode != "pause" && mode != "stop" && mode != "exclude" {
		return errors.New("container Resource Saver policy must be pause, stop, exclude, or empty")
	}
	ctx, release, err := m.dataReadGate(ctx)
	if err != nil {
		return err
	}
	defer release()
	id, err = m.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	backend, close, err := m.resourceSaverRuntime(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, close()) }()
	container, err := backend.client.LoadContainer(withContainerdNamespace(ctx, backend.namespace), id)
	if err != nil {
		return err
	}
	_, err = container.SetLabels(withContainerdNamespace(ctx, backend.namespace), map[string]string{
		ResourceSaverLabel: mode, ResourceSaverStatefulLabel: strconv.FormatBool(stateful), ManagedWorkloadLabel: "true",
	})
	if err == nil {
		m.invalidateContainerInventory()
	}
	return err
}

func (m *Manager) ResourceSaverSleep(ctx context.Context, entry resourcesaver.Entry) (err error) {
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return err
	}
	defer release()
	ctx = context.WithValue(ctx, resourceSaverContextKey{}, true)
	backend, close, err := m.resourceSaverRuntime(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, close()) }()
	for _, member := range entry.Application.Members {
		if _, _, err := m.resourceSaverContainer(ctx, backend, member, true); err != nil {
			return err
		}
	}
	completed := make([]resourcesaver.Member, 0, len(entry.Application.Members))
	defer func() {
		if err == nil {
			return
		}
		rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		rollback := entry
		rollback.Application.Members = completed
		if len(completed) > 0 {
			if rollbackErr := m.ResourceSaverResume(rollbackContext, rollback); rollbackErr != nil {
				err = resourcesaver.OwnershipUncertainError{Err: errors.Join(err, rollbackErr)}
			}
		}
	}()
	for _, member := range entry.Application.Members {
		container, _, err := m.resourceSaverContainer(ctx, backend, member, true)
		if err != nil {
			return err
		}
		action := "pause"
		if entry.Mode == resourcesaver.ScaleZero {
			action = "stop"
		}
		if err := m.ContainerAction(ctx, member.ID, action); err != nil {
			return err
		}
		owner := resourceSaverOwner{Token: entry.Token, Identity: member.Identity, PID: member.PID, Phase: "sleeping", Mode: entry.Mode}
		if entry.Mode == resourcesaver.ScaleZero {
			owner.PID = 0
		}
		completed = append(completed, member)
		if err := writeResourceSaverOwner(withContainerdNamespace(ctx, backend.namespace), container, owner); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) resourceSaverContainer(ctx context.Context, backend *grpcContainerRuntime, member resourcesaver.Member, checkBusy bool) (containerd.Container, containerd.Task, error) {
	namespaceContext := withContainerdNamespace(ctx, backend.namespace)
	container, err := backend.client.LoadContainer(namespaceContext, member.ID)
	if err != nil {
		return nil, nil, err
	}
	info, err := container.Info(namespaceContext)
	if err != nil {
		return nil, nil, err
	}
	if member.Identity == "" || member.Identity != member.ID+"@"+info.CreatedAt.UTC().Format(time.RFC3339Nano) {
		return nil, nil, fmt.Errorf("%w: Resource Saver container identity changed", ErrConflict)
	}
	task, err := container.Task(namespaceContext, nil)
	if err != nil {
		if !checkBusy && errdefs.IsNotFound(err) {
			return container, nil, nil
		}
		return nil, nil, err
	}
	if !checkBusy {
		return container, task, nil
	}
	status, err := task.Status(namespaceContext)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect Resource Saver task status: %w", err)
	}
	if status.Status != containerd.Running || task.Pid() != member.PID {
		return nil, nil, fmt.Errorf("%w: container is not the same running task; manual pauses and replacement tasks are never claimed", ErrConflict)
	}
	processes, err := task.Pids(namespaceContext)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect active execs and processes: %w", err)
	}
	if len(processes) != 1 {
		return nil, nil, fmt.Errorf("%w: active execs or an unobservable multi-process workload prevent automatic suspension", ErrConflict)
	}
	document, err := m.InspectContainer(ctx, member.ID)
	if err != nil {
		return nil, nil, err
	}
	var inspected struct {
		Config     struct{ Tty, OpenStdin bool }
		HostConfig struct{ NetworkMode string }
	}
	if err := json.Unmarshal(document, &inspected); err != nil {
		return nil, nil, err
	}
	mode := inspected.HostConfig.NetworkMode
	if inspected.Config.Tty || inspected.Config.OpenStdin || mode == "" || mode == "host" || strings.HasPrefix(mode, "container:") || mode == "none" {
		return nil, nil, fmt.Errorf("%w: interactive or unsupported-network containers cannot be suspended automatically", ErrUnsupported)
	}
	if err := m.resourceSaverConnections(ctx, backend, task.Pid()); err != nil {
		return nil, nil, err
	}
	return container, task, nil
}

func (m *Manager) resourceSaverConnections(ctx context.Context, backend *grpcContainerRuntime, pid uint32) error {
	const script = `set -eu
root="/proc/$1/net"
test -r "$root/tcp"
awk 'FNR>1 && $4=="01" { active++ } END { print active+0 }' "$root/tcp" "$root/tcp6"
`
	command := runtimes.Command{Name: "sh", Args: []string{"-c", script, "porto-active-connections", strconv.FormatUint(uint64(pid), 10)}}
	if backend.lima != "" {
		command.Name = "limactl"
		command.Args = append([]string{"shell", "--workdir=/", backend.lima, "--", "sudo", "-n", "sh"}, command.Args...)
	}
	output, err := m.runner.Run(ctx, command)
	if err != nil {
		return runtimes.CommandError("inspect active container connections", output, err)
	}
	active, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return fmt.Errorf("%w: decode the complete socket inventory: %w", ErrConflict, err)
	}
	if active != 0 {
		return fmt.Errorf("%w: %d established connection(s) prevent suspension", ErrConflict, active)
	}
	return nil
}

func (m *Manager) ResourceSaverResume(ctx context.Context, entry resourcesaver.Entry) (err error) {
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return err
	}
	defer release()
	ctx = context.WithValue(ctx, resourceSaverContextKey{}, true)
	backend, close, err := m.resourceSaverRuntime(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, close()) }()
	for _, member := range entry.Application.Members {
		container, task, err := m.resourceSaverContainer(ctx, backend, member, false)
		if err != nil {
			return err
		}
		labels, err := container.Labels(withContainerdNamespace(ctx, backend.namespace))
		if err != nil {
			return err
		}
		var owner resourceSaverOwner
		if err := json.Unmarshal([]byte(labels[resourceSaverOwnerLabel]), &owner); err != nil || owner.Token != entry.Token || owner.Identity != member.Identity || owner.Mode != entry.Mode {
			return fmt.Errorf("%w: Resource Saver ownership is missing or changed; manually paused containers are not resumed", ErrConflict)
		}
		status := containerd.Stopped
		if task != nil {
			state, err := task.Status(withContainerdNamespace(ctx, backend.namespace))
			if err != nil {
				return err
			}
			status = state.Status
			if owner.Phase == "resumed" && status == containerd.Running && task.Pid() == owner.PID {
				continue
			}
		}
		action := "start"
		if entry.Mode == resourcesaver.Pause {
			if task == nil || status != containerd.Paused || task.Pid() != owner.PID {
				return fmt.Errorf("%w: paused task ownership changed", ErrConflict)
			}
			action = "unpause"
		} else if status != containerd.Stopped && status != containerd.Created {
			return fmt.Errorf("%w: stopped task ownership changed; a manual pause is not Resource Saver ownership", ErrConflict)
		}
		if err := m.ContainerAction(ctx, member.ID, action); err != nil {
			return err
		}
		task, err = container.Task(withContainerdNamespace(ctx, backend.namespace), nil)
		if err != nil {
			return err
		}
		owner.PID, owner.Phase = task.Pid(), "resumed"
		if err := writeResourceSaverOwner(withContainerdNamespace(ctx, backend.namespace), container, owner); err != nil {
			return err
		}
	}
	return nil
}

func writeResourceSaverOwner(ctx context.Context, container containerd.Container, owner resourceSaverOwner) error {
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	_, err = container.SetLabels(ctx, map[string]string{resourceSaverOwnerLabel: string(data)})
	return err
}

func (m *Manager) SuspendResourceSaverEngine(ctx context.Context, token string) (err error) {
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return err
	}
	defer release()
	state, err := m.readEngineState()
	if err != nil {
		return err
	}
	if state.Mode != "lima" || state.Instance != engineInstanceName || state.OwnerID == "" {
		return fmt.Errorf("%w: external engines cannot be suspended by Resource Saver", ErrUnsupported)
	}
	if err := m.verifyLimaOwnership(ctx, state.OwnerID); err != nil {
		return err
	}
	if err := m.CheckEngineIdle(ctx); err != nil {
		return err
	}
	state.ResourceSaverToken = token
	if err := m.writeEngineState(state); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, resourceSaverContextKey{}, true)
	if err := m.StopEngine(ctx); err != nil {
		state.ResourceSaverToken = ""
		return errors.Join(err, m.writeEngineState(state))
	}
	return nil
}

func (m *Manager) WakeResourceSaverEngine(ctx context.Context, token string) error {
	state, err := m.readEngineState()
	if err != nil {
		return err
	}
	exists, running, err := m.limaInstanceStatus(ctx)
	if err != nil {
		return fmt.Errorf("Resource Saver engine is unavailable: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: the Resource Saver engine no longer exists", ErrNotFound)
	}
	if !running && (state.ResourceSaverToken == "" || state.ResourceSaverToken != token) {
		return fmt.Errorf("%w: the engine was stopped manually or its ownership changed; automatic wake was refused", ErrConflict)
	}
	ctx = context.WithValue(ctx, resourceSaverContextKey{}, true)
	if err := m.StartEngine(ctx); err != nil {
		return err
	}
	if err := m.StartContainerInventory(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if snapshot := m.ContainerSnapshot(); snapshot.Available && !snapshot.Stale {
			state.ResourceSaverToken = ""
			return m.writeEngineState(state)
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
		}
	}
}

func (m *Manager) ResourceSaverEngineOwned(token string) (bool, error) {
	state, err := m.readEngineState()
	return err == nil && state.ResourceSaverToken == token && token != "", err
}
