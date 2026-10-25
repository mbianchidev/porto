//go:build !windows

package filelock

import (
	"errors"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type Lock struct {
	file *os.File
	once sync.Once
	err  error
}

func (l *Lock) try() error { return unix.Flock(int(l.file.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func busy(err error) bool  { return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) }
func (l *Lock) Close() error {
	l.once.Do(func() { l.err = errors.Join(unix.Flock(int(l.file.Fd()), unix.LOCK_UN), l.file.Close()) })
	return l.err
}
