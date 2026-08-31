//go:build windows

package process

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

func configure(cmd *exec.Cmd) {}

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
	output, treeErr := exec.Command(
		"taskkill",
		"/PID", strconv.Itoa(cmd.Process.Pid),
		"/T",
		"/F",
	).CombinedOutput()
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
