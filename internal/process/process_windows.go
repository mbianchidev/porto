//go:build windows

package process

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}

func Terminate(cmd *exec.Cmd) error {
	return killTree(cmd)
}

func Kill(cmd *exec.Cmd) error {
	return killTree(cmd)
}

func killTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	taskkill := exec.Command(
		"taskkill",
		"/PID", strconv.Itoa(cmd.Process.Pid),
		"/T",
		"/F",
	)
	configure(taskkill)
	output, treeErr := taskkill.CombinedOutput()
	if treeErr == nil {
		return nil
	}
	processErr := cmd.Process.Kill()
	message := strings.TrimSpace(string(output))
	if message != "" {
		treeErr = errors.New(message)
	}
	return errors.Join(treeErr, processErr)
}
