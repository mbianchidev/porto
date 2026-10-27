//go:build windows

package docker

import (
	"errors"

	"golang.org/x/sys/windows"
)

func tryEngineInstallLock(lock *engineInstallLock) (bool, error) {
	err := windows.LockFileEx(windows.Handle(lock.file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &lock.overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}
