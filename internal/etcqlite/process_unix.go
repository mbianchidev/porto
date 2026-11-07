//go:build darwin || linux

package etcqlite

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func signalPID(pid int, signal unix.Signal) error {
	if pid <= 1 {
		return errors.New("invalid Etcqlite process ID")
	}
	err := unix.Kill(pid, signal)
	if errors.Is(err, unix.ESRCH) {
		return os.ErrNotExist
	}
	return err
}

func terminatePID(pid int) error { return signalPID(pid, unix.SIGTERM) }
func killPID(pid int) error      { return signalPID(pid, unix.SIGKILL) }
