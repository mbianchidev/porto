//go:build unix

package docker

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type engineInstallLock struct {
	file *os.File
}

func acquireEngineInstallLock(path string) (*engineInstallLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &engineInstallLock{file: file}, nil
}

func (l *engineInstallLock) Close() error {
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	return errors.Join(unlockErr, closeErr)
}
