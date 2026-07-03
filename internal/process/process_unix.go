//go:build !windows

package process

import (
"os/exec"
"syscall"
)

func configure(cmd *exec.Cmd) {
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func Terminate(cmd *exec.Cmd) error {
if cmd == nil || cmd.Process == nil {
return nil
}
return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

func Kill(cmd *exec.Cmd) error {
if cmd == nil || cmd.Process == nil {
return nil
}
return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
