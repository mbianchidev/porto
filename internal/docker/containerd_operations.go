package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultContainerStopTimeout = 10 * time.Second

type containerOperations interface {
	Start(context.Context, string) error
	Stop(context.Context, string, int) error
	Kill(context.Context, string, uint32) error
	Pause(context.Context, string) error
	Resume(context.Context, string) error
	Restart(context.Context, string, int) error
	Wait(context.Context, string) (int, error)
	Close() error
}

type containerOperationsConnector func(context.Context) (containerOperations, error)

func (m *Manager) connectContainerOperations(ctx context.Context) (containerOperations, error) {
	runtimeClient, err := m.connectContainerRuntime(ctx)
	if err != nil {
		return nil, err
	}
	operations, ok := runtimeClient.(containerOperations)
	if !ok {
		_ = runtimeClient.Close()
		return nil, fmt.Errorf("%w: connected containerd client does not support lifecycle operations", ErrUnsupported)
	}
	return operations, nil
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
		return false, nil
	}
	operationErr := operation(operations)
	closeErr := operations.Close()
	if errors.Is(operationErr, ErrUnsupported) {
		return false, nil
	}
	return true, errors.Join(operationErr, closeErr)
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
