package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/mbianchidev/porto/internal/runtimes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// nerdctl's attached start wires only stdout and stderr for non-terminal containers and never
// closes the task's stdin, so piped input (docker run -i) never reaches the container nor ends
// with EOF. Porto feeds the task's stdin FIFO itself and closes it through containerd.
type compatibilityInputForwarder interface {
	ForwardTaskInput(ctx context.Context, id string, input io.Reader) error
}

var (
	compatibilityInputTimeout      = time.Minute
	compatibilityInputPollInterval = 50 * time.Millisecond
)

const compatibilityInputShutdownTimeout = 10 * time.Second

func (r *grpcContainerRuntime) ForwardTaskInput(ctx context.Context, id string, input io.Reader) error {
	if r.tasks == nil {
		return fmt.Errorf("%w: containerd task service is unavailable", ErrUnavailable)
	}
	resolved, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	fifo, err := r.waitForTaskInput(ctx, resolved)
	if err != nil {
		return err
	}
	writeErr := r.writeTaskInput(ctx, fifo, input)
	closeContext, cancel := context.WithTimeout(withContainerdNamespace(context.WithoutCancel(ctx), r.namespace), compatibilityInputShutdownTimeout)
	defer cancel()
	_, closeErr := r.tasks.CloseIO(closeContext, &tasksapi.CloseIORequest{ContainerID: resolved, Stdin: true})
	if status.Code(closeErr) == codes.NotFound {
		closeErr = nil
	}
	if closeErr != nil {
		closeErr = containerdOperationError("close task input of", resolved, closeErr)
	}
	return errors.Join(writeErr, closeErr)
}

// waitForTaskInput skips a previous stopped task, which nerdctl deletes before creating the attached one.
func (r *grpcContainerRuntime) waitForTaskInput(ctx context.Context, id string) (string, error) {
	deadline := time.NewTimer(compatibilityInputTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(compatibilityInputPollInterval)
	defer ticker.Stop()
	for {
		process, err := r.getTask(ctx, id)
		if err != nil && status.Code(err) != codes.NotFound {
			return "", containerdOperationError("inspect task input of", id, err)
		}
		if err == nil && process.GetStdin() != "" &&
			(process.GetStatus() == tasktypes.Status_CREATED || process.GetStatus() == tasktypes.Status_RUNNING) {
			return process.GetStdin(), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", fmt.Errorf("container %q did not expose task input within %s", id, compatibilityInputTimeout)
		case <-ticker.C:
		}
	}
}

// taskInputWriterScript copies its input into the task's stdin FIFO, whose reported path is valid
// inside RootlessKit's mount namespace for rootless containerd.
const taskInputWriterScript = `set -- sh -c 'exec cat > "$1"' porto-task-input "$1"
` + rootlessKitExec

func (r *grpcContainerRuntime) writeTaskInput(ctx context.Context, fifo string, input io.Reader) error {
	runner, ok := r.runner.(runtimes.ProcessRunner)
	if !ok {
		return fmt.Errorf("%w: task input requires process streaming", ErrUnsupported)
	}
	command := runtimes.Command{Name: "sh", Args: []string{"-c", taskInputWriterScript, "porto-task-input", fifo}}
	if r.lima != "" {
		command = runtimes.Command{Name: "limactl", Args: append([]string{"shell", "--workdir=/", r.lima, "--", command.Name}, command.Args...)}
	}
	writer, err := runner.Start(ctx, command)
	if err != nil {
		return fmt.Errorf("start task input writer: %w", err)
	}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			_ = writer.Kill()
		case <-stopped:
		}
	}()
	var stderr bytes.Buffer
	var drained sync.WaitGroup
	drained.Add(2)
	go func() { defer drained.Done(); _, _ = io.Copy(io.Discard, writer.Stdout()) }()
	go func() {
		defer drained.Done()
		_, _ = io.Copy(&limitedWriter{buffer: &stderr, limit: 4096}, writer.Stderr())
	}()
	_, copyErr := io.Copy(writer.Stdin(), input)
	closeErr := writer.Stdin().Close()
	drained.Wait()
	if waitErr := writer.Wait(); waitErr != nil {
		detail := strings.TrimSpace(stderr.String())
		return errors.Join(copyErr, closeErr, fmt.Errorf("task input writer: %w: %s", waitErr, detail))
	}
	return errors.Join(copyErr, closeErr)
}

type limitedWriter struct {
	buffer *bytes.Buffer
	limit  int
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if remaining := w.limit - w.buffer.Len(); remaining > 0 {
		w.buffer.Write(data[:min(len(data), remaining)])
	}
	return len(data), nil
}

type compatibilityInputProcess struct {
	runtimes.Process
	input  *io.PipeWriter
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *compatibilityInputProcess) Stdin() io.WriteCloser {
	return p.input
}

func (p *compatibilityInputProcess) Kill() error {
	p.cancel()
	return p.Process.Kill()
}

func (p *compatibilityInputProcess) Wait() error {
	err := p.Process.Wait()
	p.cancel()
	_ = p.input.CloseWithError(io.ErrClosedPipe)
	timer := time.NewTimer(compatibilityInputShutdownTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		slog.Warn("Container input forwarding did not stop after the attached process exited")
	}
	return err
}

// execOperationsAs finds an optional capability behind the wrapper that lends the inventory's
// live runtime, whose embedded interface would otherwise hide every extra method.
func execOperationsAs[T any](operations execOperations) (T, bool) {
	if borrowed, ok := operations.(borrowedExecOperations); ok {
		operations = borrowed.execOperations
	}
	capability, ok := operations.(T)
	return capability, ok
}

// forwardCompatibilityInput keeps the attached start's output and exit status, and replaces its
// ignored stdin with a writer that reaches the task. Without a containerd connection it returns
// the original process, which keeps output working but cannot deliver input.
func (m *Manager) forwardCompatibilityInput(ctx context.Context, id string, process runtimes.Process) runtimes.Process {
	if m.execConnector == nil {
		return process
	}
	operations, err := m.execConnector(ctx)
	if err != nil {
		slog.Warn("Container input forwarding is unavailable", "container", id, "error", err)
		return process
	}
	forwarder, ok := execOperationsAs[compatibilityInputForwarder](operations)
	if !ok {
		_ = operations.Close()
		slog.Warn("Container input forwarding is unsupported by the containerd connection", "container", id)
		return process
	}
	_ = process.Stdin().Close()
	reader, writer := io.Pipe()
	forwardContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
	wrapped := &compatibilityInputProcess{Process: process, input: writer, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(wrapped.done)
		forwardErr := forwarder.ForwardTaskInput(forwardContext, id, reader)
		_ = reader.CloseWithError(io.ErrClosedPipe)
		err := errors.Join(forwardErr, operations.Close())
		if err != nil && forwardContext.Err() == nil {
			slog.Error("Container input forwarding failed", "container", id, "error", err)
		}
	}()
	return wrapped
}
