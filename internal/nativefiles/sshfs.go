package nativefiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func (m *Manager) mountSSHFSLocked(ctx context.Context, live *liveAttachment, writable bool) (err error) {
	runner, ok := m.runner.(runtimes.ProcessRunner)
	if !ok {
		return datafiles.ErrUnsupported
	}
	binary, err := m.lookPath("sshfs")
	if err != nil {
		return err
	}
	owned, cancel := context.WithCancel(context.Background())
	stopStartup := context.AfterFunc(ctx, cancel)
	defer stopStartup()
	command := live.target.SFTP
	remote, err := runner.Start(owned, runtimes.Command{Name: command.Name, Args: command.Args, Env: command.Env})
	if err != nil {
		cancel()
		return err
	}
	options := fmt.Sprintf("fsname=porto-%s,contain_symlinks,default_permissions,uid=%d,gid=%d,direct_io,dir_cache=no,nodev,nosuid,noexec", live.record.ID, os.Geteuid(), os.Getegid())
	if !writable {
		options += ",ro"
	}
	host, err := runner.Start(owned, runtimes.Command{Name: binary, Args: []string{"-f", "-o", "passive", "-o", options, "porto:/", live.record.Path}})
	if err != nil {
		cancel()
		killErr := remote.Kill()
		_, stderrErr := io.Copy(io.Discard, remote.Stderr())
		return errors.Join(err, killErr, stderrErr, remote.Wait())
	}
	var remoteErrors, hostErrors limitedNativeErrors
	closeRemoteInput := sync.OnceValue(remote.Stdin().Close)
	closeHostInput := sync.OnceValue(host.Stdin().Close)
	remoteStderr, hostStderr := make(chan error, 1), make(chan error, 1)
	go func() { _, err := io.Copy(&remoteErrors, remote.Stderr()); remoteStderr <- err }()
	go func() { _, err := io.Copy(&hostErrors, host.Stderr()); hostStderr <- err }()
	up, down := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := io.Copy(remote.Stdin(), host.Stdout())
		up <- errors.Join(err, closeRemoteInput())
	}()
	go func() {
		_, err := io.Copy(host.Stdin(), remote.Stdout())
		down <- errors.Join(err, closeHostInput())
	}()
	done := make(chan error, 1)
	live.done = done
	go func() {
		upErr, downErr := <-up, <-down
		remoteErr, hostErr := <-remoteStderr, <-hostStderr
		remoteWait, hostWait := remote.Wait(), host.Wait()
		if remoteWait != nil {
			remoteWait = fmt.Errorf("confined native SFTP failed: %w: %s", remoteWait, remoteErrors.String())
		}
		if hostWait != nil {
			hostWait = fmt.Errorf("SSHFS bridge failed: %w: %s", hostWait, hostErrors.String())
		}
		done <- errors.Join(upErr, downErr, remoteErr, hostErr, remoteWait, hostWait)
		close(done)
	}()
	live.closeBridge = func(ctx context.Context) error {
		inputErr := errors.Join(closeHostInput(), closeRemoteInput())
		select {
		case processErr := <-done:
			cancel()
			live.closeBridge, live.done = nil, nil
			return errors.Join(inputErr, processErr)
		case <-ctx.Done():
			cancel()
			return errors.Join(inputErr, fmt.Errorf("wait for owned native SSHFS/SFTP cleanup: %w", ctx.Err()))
		}
	}
	if err := m.waitMounted(ctx, live.record, live.done); err != nil {
		return fmt.Errorf("native SSHFS startup failed: %w: SSHFS: %s; SFTP: %s", err, hostErrors.String(), remoteErrors.String())
	}
	return nil
}
