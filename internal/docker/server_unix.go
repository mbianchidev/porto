//go:build !windows

package docker

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type unixEndpointLease struct {
	lock     *os.File
	identity socketIdentity
}

type socketIdentity struct {
	device uint64
	inode  uint64
}

func listenDockerEndpoint(path string) (net.Listener, endpointLease, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create Docker API directory: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open Docker API lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, nil, fmt.Errorf("Docker API endpoint %s is already owned by another Porto daemon: %w", path, err)
	}
	if err := removeStaleSocket(path); err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, nil, endpointListenError(path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, nil, fmt.Errorf("protect Porto Docker API socket: %w", err)
	}
	identity, err := dockerSocketIdentity(path)
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, nil, err
	}
	return listener, &unixEndpointLease{lock: lock, identity: identity}, nil
}

func (l *unixEndpointLease) Release(path string) error {
	current, identityErr := dockerSocketIdentity(path)
	var removeErr error
	switch {
	case errors.Is(identityErr, os.ErrNotExist):
	case identityErr != nil:
		removeErr = identityErr
	case current != l.identity:
		removeErr = fmt.Errorf("refusing to remove Docker API socket replaced by another process: %s", path)
	default:
		removeErr = os.Remove(path)
	}
	unlockErr := syscall.Flock(int(l.lock.Fd()), syscall.LOCK_UN)
	closeErr := l.lock.Close()
	return errors.Join(removeErr, unlockErr, closeErr)
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Docker API socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket path %s", path)
	}
	connection, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return fmt.Errorf("refusing to replace active Docker API socket %s", path)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return fmt.Errorf("cannot verify whether Docker API socket %s is stale: %w", path, dialErr)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale Docker API socket: %w", err)
	}
	return nil
}

func dockerSocketIdentity(path string) (socketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return socketIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return socketIdentity{}, errors.New("Docker API socket identity is unavailable")
	}
	return socketIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}, nil
}
