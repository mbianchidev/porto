//go:build windows

package filelock

import (
	"errors"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

type Lock struct {
	file       *os.File
	overlapped windows.Overlapped
	once       sync.Once
	err        error
}

func (l *Lock) try() error {
	return windows.LockFileEx(windows.Handle(l.file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &l.overlapped)
}
func busy(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }
func (l *Lock) Close() error {
	l.once.Do(func() {
		l.err = errors.Join(windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped), l.file.Close())
	})
	return l.err
}
