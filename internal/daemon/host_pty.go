package daemon

import (
	"context"
	"os/exec"

	"github.com/charmbracelet/x/xpty"
)

type hostPTY struct {
	xpty.Pty
	ctx     context.Context
	command *exec.Cmd
}

func startHostPTY(ctx context.Context, command *exec.Cmd) (*hostPTY, error) {
	terminal, err := xpty.NewPty(120, 32)
	if err != nil {
		return nil, err
	}
	if err := terminal.Start(command); err != nil {
		_ = terminal.Close()
		return nil, err
	}
	return &hostPTY{Pty: terminal, ctx: ctx, command: command}, nil
}

func (p *hostPTY) Wait() error {
	return xpty.WaitProcess(p.ctx, p.command)
}
