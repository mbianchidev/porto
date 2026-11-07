//go:build darwin

package etcqlite

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func inspectPID(pid int) (*ProcessIdentity, error) {
	if pid <= 1 {
		return nil, errors.New("invalid Etcqlite process ID")
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	// The typed helper reports EIO for an empty, already-reaped process record.
	if errors.Is(err, unix.EIO) {
		raw, rawErr := unix.SysctlRaw("kern.proc.pid", pid)
		if rawErr == nil && len(raw) == 0 {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("read macOS process birth: %w", errors.Join(err, rawErr))
	}
	if errors.Is(err, unix.ESRCH) || (err == nil && (info.Proc.P_pid != int32(pid) || info.Proc.P_stat == 5)) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("read macOS process birth: %w", err)
	}
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		current, currentErr := unix.SysctlKinfoProc("kern.proc.pid", pid)
		record, recordErr := unix.SysctlRaw("kern.proc.pid", pid)
		if (recordErr == nil && len(record) == 0) ||
			(currentErr == nil && (current.Proc.P_pid != int32(pid) || current.Proc.P_stat == 5)) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("read macOS process executable: %w", err)
	}
	if len(raw) <= 4 {
		return nil, errors.New("macOS returned incomplete process arguments")
	}
	end := bytes.IndexByte(raw[4:], 0)
	if end <= 0 {
		return nil, errors.New("macOS returned no process executable")
	}
	path := string(raw[4 : 4+end])
	executable, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) && filepath.IsAbs(path) {
		slog.Warn("Native process executable was removed; ownership retains its kernel birth and executable path", "pid", pid, "executable", path)
		executable, err = filepath.Clean(path), nil
	}
	if err != nil {
		return nil, err
	}
	return &ProcessIdentity{
		PID: pid, Birth: fmt.Sprintf("%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec), Executable: executable,
	}, nil
}
