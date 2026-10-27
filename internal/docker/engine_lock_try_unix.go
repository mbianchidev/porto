//go:build unix

package docker

import (
	"errors"

	"golang.org/x/sys/unix"
)

func tryEngineInstallLock(lock *engineInstallLock) (bool, error) {
	err := unix.Flock(int(lock.file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}
