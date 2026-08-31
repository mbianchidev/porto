package docker

import (
	"context"
	"fmt"
	"strconv"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type LogOptions struct {
	Stdout     bool
	Stderr     bool
	Timestamps bool
	Tail       string
	Since      string
	Until      string
}

func (m *Manager) ContainerLogs(ctx context.Context, id string, tail int) ([]byte, error) {
	if err := validateObjectID(id); err != nil {
		return nil, err
	}
	if tail <= 0 || tail > 10000 {
		tail = 500
	}
	var output []byte
	err := m.StreamDockerContainerLogs(ctx, id, LogOptions{
		Stdout:     true,
		Stderr:     true,
		Timestamps: true,
		Tail:       strconv.Itoa(tail),
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
	args := []string{"logs"}
	if options.Timestamps {
		args = append(args, "--timestamps")
	}
	if options.Tail != "" {
		args = append(args, "--tail", options.Tail)
	}
	args = appendStringFlag(args, "--since", options.Since)
	args = appendStringFlag(args, "--until", options.Until)
	args = append(args, id)
	return m.runStreaming(ctx, m.timeout, "read Porto container logs", nil, emit, args...)
}
