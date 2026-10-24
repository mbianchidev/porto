//go:build !windows

package daemon

import (
	"github.com/charmbracelet/x/xpty"
	"os/exec"
)

func hostPTYSupported() bool { return true }

func startPTYCommand(terminal xpty.Pty, command *exec.Cmd) error { return terminal.Start(command) }
