//go:build !windows

package runtimes

import (
	"github.com/charmbracelet/x/xpty"
	"os/exec"
)

func PTYSupported() bool                                         { return true }
func startPTYCommand(terminal xpty.Pty, command *exec.Cmd) error { return terminal.Start(command) }
