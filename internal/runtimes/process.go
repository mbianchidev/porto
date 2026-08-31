package runtimes

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
)

type Process interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait() error
	Kill() error
	PID() int
}

type ProcessRunner interface {
	Start(context.Context, Command) (Process, error)
}

func (ExecRunner) Start(ctx context.Context, command Command) (Process, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = append(os.Environ(), command.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}
	return &execProcess{
		command: cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
	}, nil
}

type execProcess struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	wait    sync.Once
	waitErr error
}

func (p *execProcess) Stdin() io.WriteCloser {
	return p.stdin
}

func (p *execProcess) Stdout() io.ReadCloser {
	return p.stdout
}

func (p *execProcess) Stderr() io.ReadCloser {
	return p.stderr
}

func (p *execProcess) Wait() error {
	p.wait.Do(func() {
		p.waitErr = p.command.Wait()
	})
	return p.waitErr
}

func (p *execProcess) Kill() error {
	if p.command.Process == nil {
		return nil
	}
	return p.command.Process.Kill()
}

func (p *execProcess) PID() int {
	if p.command.Process == nil {
		return 0
	}
	return p.command.Process.Pid
}
