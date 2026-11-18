package docker

import (
	"context"
	"errors"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

// startLogAttach attaches to a non-terminal compatibility container. nerdctl's attach cannot open
// output that goes to the log driver and would compete for FIFOs that another start still reads,
// so output follows the container's logs from now until it exits, and input goes to its stdin FIFO.
func (m *Manager) startLogAttach(ctx context.Context, details containerAttachDetails, stdin bool) (runtimes.Process, error) {
	var forwarder compatibilityInputForwarder
	closeForwarder := func() error { return nil }
	if stdin && details.OpenStdin {
		var err error
		forwarder, closeForwarder, err = m.taskInputForwarder(ctx)
		if err != nil {
			return nil, err
		}
		fifo, err := forwarder.TaskInput(ctx, details.ID)
		if err == nil && fifo == "" {
			err = errTaskInputClosed
		}
		if err != nil {
			return nil, errors.Join(err, closeForwarder())
		}
	}
	process, err := m.startProcess(ctx, "follow Porto container output", "logs", "--follow", "--since", time.Now().UTC().Format(time.RFC3339Nano), details.ID)
	if err != nil {
		return nil, errors.Join(err, closeForwarder())
	}
	if forwarder == nil {
		return process, nil
	}
	return wrapTaskInput(ctx, details.ID, process, forwarder, closeForwarder, taskInputOptions{closeOnEOF: details.StdinOnce}), nil
}
