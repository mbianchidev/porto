//go:build linux || darwin

package localhttps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/mbianchidev/porto/internal/process"
)

func nativeDaemonIDs() (int, int, error) {
	if os.Geteuid() != 0 {
		return os.Geteuid(), os.Getegid(), nil
	}
	uid, first := strconv.Atoi(os.Getenv("SUDO_UID"))
	gid, second := strconv.Atoi(os.Getenv("SUDO_GID"))
	if first != nil || second != nil || uid == 0 {
		return 0, 0, errors.New("native HTTPS CI must retain SUDO_UID/GID to run the actual daemon unprivileged")
	}
	return uid, gid, nil
}

func prepareNativeDaemonOwner(home string) error {
	uid, gid, err := nativeDaemonIDs()
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return nil
	}
	return filepath.Walk(home, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, uid, gid)
	})
}

func nativeUnprivilegedDaemon(_ *testing.T, ctx context.Context, binary, home, ui string) (*exec.Cmd, error) {
	uid, gid, err := nativeDaemonIDs()
	if err != nil {
		return nil, err
	}
	command := process.NewCommand(ctx, "", binary, "daemon", "start")
	command.Env = process.WithEnvironment(os.Environ(), "PORTO_HOME="+home, "PORTO_UI_DIR="+ui, "HOME="+home)
	if os.Geteuid() == 0 {
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	}
	return command, nil
}

func startNativeDaemon(_ *testing.T, command *exec.Cmd) error { return command.Start() }
