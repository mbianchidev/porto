//go:build windows

package process

import "os/exec"

func configure(cmd *exec.Cmd) {}

func Terminate(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func Kill(cmd *exec.Cmd) error { return Terminate(cmd) }
