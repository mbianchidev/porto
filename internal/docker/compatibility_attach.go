package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type taskOutputInspector interface {
	TaskOutput(ctx context.Context, id string) (string, error)
}

func (r *grpcContainerRuntime) TaskOutput(ctx context.Context, id string) (string, error) {
	if r.tasks == nil {
		return "", fmt.Errorf("%w: containerd task service is unavailable", ErrUnavailable)
	}
	resolved, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return "", err
	}
	process, err := r.getTask(ctx, resolved)
	if err != nil {
		return "", containerdOperationError("inspect task output of", resolved, err)
	}
	return process.GetStdout(), nil
}

// taskOutputFollowsLogs reports a task whose output goes to a log URI instead of FIFOs. nerdctl
// starts detached non-terminal containers that way, and its attach cannot open their output.
func (m *Manager) taskOutputFollowsLogs(ctx context.Context, id string) bool {
	if m.execConnector == nil {
		return false
	}
	operations, err := m.execConnector(ctx)
	if err != nil {
		return false
	}
	defer operations.Close()
	inspector, ok := operations.(taskOutputInspector)
	if !ok {
		return false
	}
	output, err := inspector.TaskOutput(ctx, id)
	return err == nil && strings.Contains(output, "://")
}

// startLogAttach streams the container's output from now until it exits, like a Docker attach
// without logs. A log-URI task has no input FIFO, so open input cannot be attached.
func (m *Manager) startLogAttach(ctx context.Context, id string, stdin bool) (runtimes.Process, error) {
	if stdin {
		details, err := m.containerAttachDetails(ctx, id)
		if err != nil {
			return nil, err
		}
		if details.OpenStdin {
			return nil, fmt.Errorf("%w: attaching input to a non-terminal container that was started detached; use 'docker exec -i' or start it attached with 'docker start -ai'", ErrUnsupported)
		}
	}
	return m.startProcess(ctx, "follow Porto container output", "logs", "--follow", "--since", time.Now().UTC().Format(time.RFC3339Nano), id)
}
