//go:build windows

package runtimes

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/charmbracelet/x/xpty"
	"golang.org/x/sys/windows"
)

func PTYSupported() bool { return windows.RtlGetVersion().BuildNumber >= 17763 }

func startPTYCommand(terminal xpty.Pty, command *exec.Cmd) error {
	console, ok := terminal.(*xpty.ConPty)
	if !ok {
		return errors.New("Windows terminal did not provide a ConPTY")
	}
	pid, handle, err := console.Spawn(command.Path, command.Args, &syscall.ProcAttr{Dir: command.Dir, Env: command.Env, Sys: command.SysProcAttr})
	if err != nil {
		return err
	}
	// ConPTY's creation handle and os.FindProcess's wait handle have different owners.
	command.Process, err = os.FindProcess(pid)
	if err != nil {
		return errors.Join(err, windows.TerminateProcess(windows.Handle(handle), 1), windows.CloseHandle(windows.Handle(handle)))
	}
	if err := windows.CloseHandle(windows.Handle(handle)); err != nil {
		killErr := command.Process.Kill()
		_, waitErr := command.Process.Wait()
		return errors.Join(err, killErr, waitErr)
	}
	return nil
}
