package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const kubeconfigStaleLockAge = 2 * time.Minute

type kubeconfigLockOwner struct {
	PID       int       `json:"pid"`
	CreatedAt time.Time `json:"createdAt"`
}

type kubeconfigFileLock struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	closed bool
}

func acquireKubeconfigFileLock(ctx context.Context, path string) (*kubeconfigFileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			owner, marshalErr := json.Marshal(kubeconfigLockOwner{
				PID:       os.Getpid(),
				CreatedAt: time.Now().UTC(),
			})
			if marshalErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, marshalErr
			}
			if _, writeErr := file.Write(owner); writeErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, writeErr
			}
			if syncErr := file.Sync(); syncErr != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, syncErr
			}
			return &kubeconfigFileLock{path: path, file: file}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		recovered, recoverErr := recoverStaleKubeconfigLock(path, time.Now())
		if recoverErr != nil {
			return nil, recoverErr
		}
		if recovered {
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *kubeconfigFileLock) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	lockedInfo, statErr := l.file.Stat()
	closeErr := l.file.Close()
	if statErr != nil {
		return errors.Join(statErr, closeErr)
	}
	currentInfo, currentErr := os.Stat(l.path)
	if errors.Is(currentErr, os.ErrNotExist) {
		return closeErr
	}
	if currentErr != nil {
		return errors.Join(closeErr, currentErr)
	}
	if !os.SameFile(lockedInfo, currentInfo) {
		return closeErr
	}
	removeErr := os.Remove(l.path)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

func recoverStaleKubeconfigLock(path string, now time.Time) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	var data []byte
	if info.Mode()&os.ModeSymlink == 0 {
		file, openErr := os.Open(path)
		if openErr == nil {
			var readErr error
			data, readErr = io.ReadAll(io.LimitReader(file, 4096))
			closeErr := file.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return false, err
			}
		} else if errors.Is(openErr, os.ErrNotExist) {
			return true, nil
		} else if !errors.Is(openErr, os.ErrPermission) {
			return false, openErr
		}
	}
	stale := now.Sub(info.ModTime()) >= kubeconfigStaleLockAge
	var owner kubeconfigLockOwner
	if json.Unmarshal(data, &owner) == nil && owner.PID > 0 {
		if !owner.CreatedAt.IsZero() {
			stale = now.Sub(owner.CreatedAt) >= kubeconfigStaleLockAge
		}
		alive, aliveErr := kubeconfigProcessAlive(owner.PID)
		if aliveErr != nil {
			return false, aliveErr
		}
		stale = stale || !alive
	}
	if !stale {
		return false, nil
	}
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !os.SameFile(info, current) {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return true, nil
}
