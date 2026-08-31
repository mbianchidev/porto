package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func (m *Manager) StartExec(ctx context.Context, request ExecRequest) (runtimes.Process, error) {
	if err := validateObjectID(request.ContainerID); err != nil {
		return nil, fmt.Errorf("container: %w", err)
	}
	if len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
		return nil, errors.New("exec command is required")
	}
	args := []string{"exec"}
	if request.Privileged {
		args = append(args, "--privileged")
	}
	for _, value := range request.Environment {
		if strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("invalid exec environment")
		}
		args = append(args, "--env", value)
	}
	args = appendStringFlag(args, "--workdir", request.WorkingDir)
	args = appendStringFlag(args, "--user", request.User)
	if request.AttachStdin {
		args = append(args, "--interactive")
	}
	if request.TTY {
		args = append(args, "--tty")
	}
	args = append(args, request.ContainerID)
	args = append(args, request.Command...)
	return m.startProcess(ctx, "start Porto container exec", args...)
}

func (m *Manager) StartAttach(ctx context.Context, id string, stdin bool) (runtimes.Process, error) {
	if err := validateObjectID(id); err != nil {
		return nil, err
	}
	args := []string{"attach"}
	if !stdin {
		args = append(args, "--no-stdin")
	}
	args = append(args, id)
	return m.startProcess(ctx, "attach Porto container", args...)
}

func (m *Manager) startProcess(ctx context.Context, action string, args ...string) (runtimes.Process, error) {
	backend, err := m.backend(ctx)
	if err != nil {
		return nil, err
	}
	runner, ok := m.runner.(runtimes.ProcessRunner)
	if !ok {
		return nil, fmt.Errorf("%w: process streaming", ErrUnsupported)
	}
	process, err := runner.Start(ctx, runtimes.Command{
		Name: backend.name,
		Args: append(append([]string(nil), backend.prefix...), args...),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return process, nil
}
