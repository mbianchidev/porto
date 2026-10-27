package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

func (m *Manager) lockEngineInstallation(ctx context.Context) error {
	if ctx.Done() == nil {
		m.installMu.Lock()
		return nil
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if m.installMu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
		}
	}
}

func acquireEngineInstallLockContext(ctx context.Context, path string) (*engineInstallLock, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if ctx.Done() == nil {
		return acquireEngineInstallLock(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &engineInstallLock{file: file}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err = context.Cause(ctx); err != nil {
			break
		}
		var acquired bool
		acquired, err = tryEngineInstallLock(lock)
		if err != nil {
			break
		}
		if acquired {
			return lock, nil
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return nil, errors.Join(err, file.Close())
}
