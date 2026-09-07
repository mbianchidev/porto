package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/mbianchidev/porto/internal/runtimes"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	UpdateHealth(context.Context, string, *ContainerHealthcheck) error
	Delete(context.Context, string, bool, bool) error
	Close() error
}

type containerOperationsConnector func(context.Context) (containerOperations, error)

type execOperations interface {
	StartExec(context.Context, ExecRequest) (runtimes.Process, error)
	Close() error
}

type execOperationsConnector func(context.Context) (execOperations, error)

type networkOperations interface {
	Connect(context.Context, string, string, []string) error
	Disconnect(context.Context, string, string, bool) error
	Close() error
}

type networkOperationsConnector func(context.Context) (networkOperations, error)

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

type managedExecProcess struct {
	runtimes.Process
	close    func() error
	waitOnce sync.Once
	waitErr  error
}

func (p *managedExecProcess) Wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = errors.Join(p.Process.Wait(), p.close())
	})
	return p.waitErr
}

func (m *Manager) withContainerOperations(
	ctx context.Context,
	operation func(containerOperations) error,
) (bool, error) {
	connector := m.operationsConnector
	if connector == nil {
		return false, nil
	}
	operations, err := connector(ctx)
	if err != nil {
		if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrUnavailable) {
			return false, nil
		}
		return true, err
	}
	operationErr := operation(operations)
	closeErr := operations.Close()
	if errors.Is(operationErr, ErrUnsupported) || errors.Is(operationErr, ErrUnavailable) {
		return false, nil
	}
	return true, errors.Join(operationErr, closeErr)
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
	if errors.Is(operationErr, ErrUnavailable) {
		return false, nil
	}
	return true, errors.Join(operationErr, closeErr)
}

func (r *grpcContainerRuntime) StartExec(
	context.Context,
	ExecRequest,
) (runtimes.Process, error) {
	return nil, fmt.Errorf(
		"%w: direct containerd exec cannot preserve attached I/O because the task service requires daemon-local FIFO paths",
		ErrUnsupported,
	)
}

func (r *grpcContainerRuntime) Connect(
	context.Context,
	string,
	string,
	[]string,
) error {
	return fmt.Errorf(
		"%w: containerd cannot safely connect nerdctl CNI endpoints or represent Docker network aliases",
		ErrUnsupported,
	)
}

func (r *grpcContainerRuntime) Disconnect(
	context.Context,
	string,
	string,
	bool,
) error {
	return fmt.Errorf(
		"%w: containerd cannot safely disconnect nerdctl CNI endpoints by changing container metadata alone",
		ErrUnsupported,
	)
}

func (r *grpcContainerRuntime) Start(ctx context.Context, id string) error {
	process, err := r.getTask(ctx, id)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			if containerErr := r.requireContainer(ctx, id); containerErr != nil {
				return containerErr
			}
			return fmt.Errorf("%w: direct start requires creating the container task", ErrUnsupported)
		}
		return containerdOperationError("inspect task for", id, err)
	}
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	switch process.GetStatus() {
	case tasktypes.Status_CREATED:
		_, err = r.tasks.Start(namespacedContext, &tasksapi.StartRequest{ContainerID: id})
	case tasktypes.Status_PAUSED:
		_, err = r.tasks.Resume(namespacedContext, &tasksapi.ResumeTaskRequest{ContainerID: id})
	case tasktypes.Status_RUNNING:
		return fmt.Errorf("%w: preserve existing handling for an already-running container", ErrUnsupported)
	case tasktypes.Status_STOPPED:
		return fmt.Errorf("%w: direct start requires recreating the stopped container task", ErrUnsupported)
	default:
		return fmt.Errorf("%w: container %q task is %s", ErrConflict, id, process.GetStatus())
	}
	return containerdOperationError("start", id, err)
}

func (r *grpcContainerRuntime) Stop(ctx context.Context, id string, timeoutSeconds int) error {
	process, err := r.getTask(ctx, id)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return r.requireContainer(ctx, id)
		}
		return containerdOperationError("inspect task for", id, err)
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
		return nil
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
	return containerdOperationError("wait for force-stopped", id, err)
}

func (r *grpcContainerRuntime) Kill(ctx context.Context, id string, signal uint32) error {
	_, err := r.tasks.Kill(
		withContainerdNamespace(ctx, r.namespace),
		&tasksapi.KillRequest{ContainerID: id, Signal: signal},
	)
	return containerdOperationError("kill", id, err)
}

func (r *grpcContainerRuntime) Pause(ctx context.Context, id string) error {
	_, err := r.tasks.Pause(
		withContainerdNamespace(ctx, r.namespace),
		&tasksapi.PauseTaskRequest{ContainerID: id},
	)
	return containerdOperationError("pause", id, err)
}

func (r *grpcContainerRuntime) Resume(ctx context.Context, id string) error {
	_, err := r.tasks.Resume(
		withContainerdNamespace(ctx, r.namespace),
		&tasksapi.ResumeTaskRequest{ContainerID: id},
	)
	return containerdOperationError("resume", id, err)
}

func (r *grpcContainerRuntime) Restart(context.Context, string, int) error {
	return fmt.Errorf("%w: direct restart requires recreating the container task", ErrUnsupported)
}

func (r *grpcContainerRuntime) Wait(ctx context.Context, id string) (int, error) {
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
	labels := cloneStringMap(record.GetLabels())
	if labels == nil {
		labels = map[string]string{}
	}
	for key, value := range updates {
		labels[key] = value
	}
	record.Labels = labels
	_, err = r.containers.Update(namespacedContext, &containersapi.UpdateContainerRequest{
		Container:  record,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
	})
	return containerdOperationError("update metadata for", id, err)
}

func (r *grpcContainerRuntime) UpdateResources(ctx context.Context, id string, update ContainerUpdate) error {
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

	updatedRecord := *record
	updatedRecord.Spec = &anypb.Any{
		TypeUrl: record.GetSpec().GetTypeUrl(),
		Value:   encodedSpec,
	}
	_, err = r.containers.Update(namespacedContext, &containersapi.UpdateContainerRequest{
		Container:  &updatedRecord,
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

func (r *grpcContainerRuntime) UpdateHealth(context.Context, string, *ContainerHealthcheck) error {
	return fmt.Errorf(
		"%w: direct healthcheck updates cannot safely manage nerdctl scheduling and result logs",
		ErrUnsupported,
	)
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
	if record.GetSnapshotKey() != "" || record.GetSnapshotter() != "" ||
		record.GetLabels()[nerdctlNetworksLabel] != "" ||
		record.GetLabels()[nerdctlPortsLabel] != "" {
		return fmt.Errorf("%w: direct removal cannot safely clean up container snapshots or networking", ErrUnsupported)
	}

	process, err := r.getTask(ctx, id)
	if err != nil && status.Code(err) != codes.NotFound {
		return containerdOperationError("inspect task for", id, err)
	}
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
		if _, err := r.tasks.Delete(
			namespacedContext,
			&tasksapi.DeleteTaskRequest{ContainerID: id},
		); err != nil {
			return containerdOperationError("delete task for", id, err)
		}
	}
	_, err = r.containers.Delete(
		namespacedContext,
		&containersapi.DeleteContainerRequest{ID: id},
	)
	return containerdOperationError("delete metadata for", id, err)
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
