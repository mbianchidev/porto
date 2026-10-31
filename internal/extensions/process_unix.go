//go:build !windows

package extensions

import (
	"errors"
	"os/exec"
	"syscall"

	"github.com/mbianchidev/porto/internal/process"
)

func cleanupProcess(cmd *exec.Cmd) error {
	err := process.Kill(cmd)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
