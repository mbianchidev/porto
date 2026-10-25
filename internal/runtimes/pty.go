package runtimes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/charmbracelet/x/xpty"
	"github.com/mbianchidev/porto/internal/process"
)

const MaxPTYDimension = 32767

type PTY struct {
	xpty.Pty
	ctx     context.Context
	command *exec.Cmd
	wait    sync.Once
	waitErr error
}

func ValidatePTYSize(size [2]uint) error {
	if size[0] > MaxPTYDimension || size[1] > MaxPTYDimension {
		return errors.New("invalid terminal dimensions: height and width must not exceed 32767")
	}
	return nil
}

func StartPTY(ctx context.Context, command *exec.Cmd, width, height int) (*PTY, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if width < 0 || height < 0 || width > MaxPTYDimension || height > MaxPTYDimension {
		return nil, errors.New("invalid terminal dimensions")
	}
	if !PTYSupported() {
		return nil, errors.New("native terminals require Windows 10 1809 or newer with ConPTY, or a Unix PTY")
	}
	terminal, err := xpty.NewPty(width, height)
	if err != nil {
		return nil, err
	}
	if err := startPTYCommand(terminal, command); err != nil {
		_ = terminal.Close()
		return nil, err
	}
	return &PTY{Pty: terminal, ctx: ctx, command: command}, nil
}

func (p *PTY) Wait() error {
	p.wait.Do(func() { p.waitErr = xpty.WaitProcess(p.ctx, p.command) })
	return p.waitErr
}

type PTYRunner interface {
	StartPTY(context.Context, Command, [2]uint) (Process, error)
}

func (ExecRunner) StartPTY(ctx context.Context, command Command, size [2]uint) (Process, error) {
	if err := ValidatePTYSize(size); err != nil {
		return nil, err
	}
	if command.Stdin != nil || command.StdinReader != nil {
		return nil, errors.New("PTY input must use the streaming stdin channel")
	}
	var err error
	command, err = withNetworkEnvironment(command)
	if err != nil {
		return nil, err
	}
	width, height := int(size[1]), int(size[0])
	if width == 0 {
		width = 120
	}
	if height == 0 {
		height = 32
	}
	lifetime, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(lifetime, command.Name, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = process.WithEnvironment(os.Environ(), command.Env...)
	terminal, err := StartPTY(lifetime, cmd, width, height)
	if err != nil {
		cancel()
		return nil, err
	}
	reader, writer := io.Pipe()
	p := &ptyProcess{terminal: terminal, command: cmd, cancel: cancel, output: reader, done: make(chan struct{}), readDone: make(chan struct{})}
	go func() {
		_, readErr := io.Copy(writer, terminal)
		if readErr != nil {
			// A disconnected consumer must not block ConPTY's synchronous close
			// while it drains its remaining output.
			_, _ = io.Copy(io.Discard, terminal)
		}
		_ = writer.Close()
		close(p.readDone)
	}()
	go func() {
		p.waitErr = terminal.Wait()
		if lifetime.Err() != nil {
			p.waitErr = errors.Join(lifetime.Err(), p.waitErr)
		}
		_ = terminal.Close()
		<-p.readDone
		close(p.done)
		cancel()
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = reader.CloseWithError(ctx.Err())
			cancel()
		case <-p.done:
		}
	}()
	return p, nil
}

type ptyProcess struct {
	terminal *PTY
	command  *exec.Cmd
	cancel   context.CancelFunc
	output   *io.PipeReader
	done     chan struct{}
	readDone chan struct{}
	waitErr  error
	kill     sync.Once
}

type ptyInput struct{ io.Writer }

func (ptyInput) Close() error { return nil }

func (p *ptyProcess) Stdin() io.WriteCloser { return ptyInput{p.terminal} }
func (p *ptyProcess) Stdout() io.ReadCloser { return p.output }
func (p *ptyProcess) Stderr() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }
func (p *ptyProcess) Wait() error           { <-p.done; return p.waitErr }
func (p *ptyProcess) PID() int              { return p.command.Process.Pid }
func (p *ptyProcess) Kill() error {
	p.kill.Do(func() { _ = p.output.CloseWithError(context.Canceled); p.cancel(); _ = p.command.Process.Kill() })
	return nil
}
func (p *ptyProcess) Resize(ctx context.Context, width, height uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if width == 0 || height == 0 || width > MaxPTYDimension || height > MaxPTYDimension {
		return errors.New("invalid terminal dimensions")
	}
	select {
	case <-p.done:
		return fmt.Errorf("terminal process has exited")
	default:
	}
	return p.terminal.Resize(int(width), int(height))
}

func (p *ptyProcess) InputEOF() { _ = p.Kill() }
