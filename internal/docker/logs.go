package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type LogOptions struct {
	Stdout     bool
	Stderr     bool
	Timestamps bool
	Tail       string
	Since      string
	Until      string
	Follow     bool
}

const allLogLines = -1

func (m *Manager) ContainerLogs(ctx context.Context, id string, tail int) ([]byte, error) {
	if err := validateObjectID(id); err != nil {
		return nil, err
	}
	if tail <= 0 || tail > 10000 {
		tail = 500
	}
	var output []byte
	err := m.StreamDockerContainerLogs(ctx, id, LogOptions{
		Stdout: true,
		Stderr: true,
		Tail:   strconv.Itoa(tail),
	}, func(chunk runtimes.OutputChunk) error {
		output = append(output, chunk.Data...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return output, nil
}

func (m *Manager) StreamDockerContainerLogs(
	ctx context.Context,
	id string,
	options LogOptions,
	emit func(runtimes.OutputChunk) error,
) error {
	if err := validateObjectID(id); err != nil {
		return err
	}
	if options.Stdout != options.Stderr {
		return fmt.Errorf("%w: selecting only stdout or stderr logs", ErrUnsupported)
	}
	if handled, err := m.streamContainerLogsDirect(ctx, id, options, emit); handled {
		return err
	}
	args := []string{"logs"}
	if options.Follow {
		args = append(args, "--follow")
	}
	if options.Timestamps {
		args = append(args, "--timestamps")
	}
	if options.Tail != "" {
		args = append(args, "--tail", options.Tail)
	}
	args = appendStringFlag(args, "--since", options.Since)
	args = appendStringFlag(args, "--until", options.Until)
	args = append(args, id)
	timeout := m.timeout
	if options.Follow {
		timeout = 24 * time.Hour
	}
	return m.runStreaming(ctx, timeout, "read Porto container logs", nil, emit, args...)
}

type containerLogOperations interface {
	StreamLogs(context.Context, string, LogOptions, func(runtimes.OutputChunk) error) error
}

func (m *Manager) streamContainerLogsDirect(
	ctx context.Context,
	id string,
	options LogOptions,
	emit func(runtimes.OutputChunk) error,
) (bool, error) {
	if m.directCLI || m.runtimeConnector == nil {
		return false, nil
	}
	runtimeClient, err := m.runtimeConnector(ctx)
	if err != nil {
		if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnsupported) {
			return false, nil
		}
		return true, err
	}
	operations, ok := runtimeClient.(containerLogOperations)
	if !ok {
		return false, runtimeClient.Close()
	}
	streamErr := operations.StreamLogs(ctx, id, options, emit)
	closeErr := runtimeClient.Close()
	if errors.Is(streamErr, ErrUnsupported) || errors.Is(streamErr, ErrUnavailable) {
		return false, errors.Join(streamErr, closeErr)
	}
	return true, errors.Join(streamErr, closeErr)
}

func (r *grpcContainerRuntime) StreamLogs(
	ctx context.Context,
	id string,
	options LogOptions,
	emit func(runtimes.OutputChunk) error,
) error {
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	id = resolvedID
	response, err := r.containers.Get(
		withContainerdNamespace(ctx, r.namespace),
		&containersapi.GetContainerRequest{ID: id},
	)
	if err != nil {
		return containerdOperationError("inspect logs for", id, err)
	}
	logPath := response.GetContainer().GetLabels()[portoLogPathLabel]
	if logPath == "" {
		return fmt.Errorf("%w: container %q does not use Porto-owned logs", ErrUnsupported, id)
	}
	if options.Since != "" || options.Until != "" || options.Timestamps {
		return fmt.Errorf(
			"%w: Porto-owned raw container logs do not retain per-line timestamps",
			ErrUnsupported,
		)
	}
	tail, err := parseLogTail(options.Tail)
	if err != nil {
		return err
	}
	if r.lima != "" {
		return r.streamLimaLog(ctx, logPath, tail, options.Follow, emit)
	}
	return streamLocalLog(ctx, logPath, tail, options.Follow, emit)
}

func parseLogTail(value string) (int, error) {
	switch strings.TrimSpace(value) {
	case "", "all":
		return allLogLines, nil
	default:
		tail, err := strconv.Atoi(value)
		if err != nil || tail < 0 {
			return 0, fmt.Errorf("invalid log tail %q", value)
		}
		return tail, nil
	}
}

func (r *grpcContainerRuntime) streamLimaLog(
	ctx context.Context,
	path string,
	tail int,
	follow bool,
	emit func(runtimes.OutputChunk) error,
) error {
	runner, ok := r.runner.(streamingRunner)
	if !ok {
		return fmt.Errorf("%w: Lima log streaming", ErrUnsupported)
	}
	args := []string{"shell", r.lima, "--", "tail"}
	if follow {
		args = append(args, "--follow=name", "--retry")
	}
	if tail > 0 {
		args = append(args, "-n", strconv.Itoa(tail))
	} else if tail == 0 {
		args = append(args, "-n", "0")
	} else {
		args = append(args, "-n", "+1")
	}
	args = append(args, "--", path)
	_, err := runner.RunStreaming(ctx, runtimes.Command{Name: "limactl", Args: args}, emit)
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("stream Lima container log %q: %w", path, err)
	}
	return context.Cause(ctx)
}

func streamLocalLog(
	ctx context.Context,
	path string,
	tail int,
	follow bool,
	emit func(runtimes.OutputChunk) error,
) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open direct container log %q: %w", path, err)
	}
	defer file.Close()
	offset, err := logTailOffset(file, tail)
	if err != nil {
		return err
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek direct container log %q: %w", path, err)
	}
	reader := bufio.NewReader(file)
	for {
		data, readErr := reader.ReadBytes('\n')
		if len(data) > 0 {
			if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: data}); err != nil {
				return err
			}
		}
		if readErr == nil {
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("read direct container log %q: %w", path, readErr)
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func logTailOffset(file *os.File, tail int) (int64, error) {
	if tail < 0 {
		return 0, nil
	}
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("inspect direct container log: %w", err)
	}
	if tail == 0 {
		return info.Size(), nil
	}
	position := info.Size()
	buffer := make([]byte, 4096)
	lines := 0
	for position > 0 {
		start := max(int64(0), position-int64(len(buffer)))
		read := int(position - start)
		if _, err := file.ReadAt(buffer[:read], start); err != nil && !errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("scan direct container log: %w", err)
		}
		for index := read - 1; index >= 0; index-- {
			if buffer[index] == '\n' && start+int64(index)+1 < info.Size() {
				lines++
				if lines == tail {
					return start + int64(index) + 1, nil
				}
			}
		}
		position = start
	}
	return 0, nil
}
