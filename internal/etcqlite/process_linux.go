//go:build linux

package etcqlite

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func inspectPID(pid int) (*ProcessIdentity, error) {
	if pid <= 1 {
		return nil, errors.New("invalid Etcqlite process ID")
	}
	directory := filepath.Join("/proc", strconv.Itoa(pid))
	data, err := os.ReadFile(filepath.Join(directory, "stat"))
	if err != nil {
		return nil, err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return nil, errors.New("Linux returned invalid process identity")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return nil, errors.New("Linux returned incomplete process identity")
	}
	if fields[0] == "Z" {
		return nil, os.ErrNotExist
	}
	executable, err := os.Readlink(filepath.Join(directory, "exe"))
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(executable, " (deleted)") {
		slog.Warn("Native process executable was removed; ownership retains its kernel birth and executable path", "pid", pid, "executable", executable)
		executable = strings.TrimSuffix(executable, " (deleted)")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	return &ProcessIdentity{PID: pid, Birth: fmt.Sprintf("%s:%s", strings.TrimSpace(string(boot)), fields[19]), Executable: executable}, nil
}
