package nativefiles

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func (m *Manager) webDAVCapability() Capability {
	if _, err := m.lookPath("mount_webdav"); err != nil {
		return Capability{
			Driver: "macOS WebDAV", Message: "The built-in macOS WebDAV filesystem is unavailable.",
			Fallback: "Use the in-app Files inspector, verified volume archives, or porto vm copy.",
		}
	}
	return Capability{
		Supported: true, Driver: "macOS WebDAV",
		Message:  "Driver-free private host folder over confined SFTP. Saved edits reach the workload directly. Unix links and host chmod/chown are unavailable; guest permissions are preserved.",
		Fallback: "Use the in-app Files inspector or verified archives for Unix link/ownership operations. Reopen or refresh for external changes; guest filesystem notifications are not guaranteed.",
	}
}

func (m *Manager) mountWebDAVLocked(ctx context.Context, live *liveAttachment, writable bool) (err error) {
	owned, cancel := context.WithCancel(context.Background())
	stopStartup := context.AfterFunc(ctx, cancel)
	startup := time.AfterFunc(30*time.Second, cancel)
	defer func() {
		stopStartup()
		startup.Stop()
		if err != nil && live.closeBridge == nil {
			cancel()
		}
	}()
	runner, ok := m.runner.(runtimes.ProcessRunner)
	if !ok {
		return datafiles.ErrUnsupported
	}
	client, process, closeConnection, err := connectNativeSFTP(owned, *live.target.SFTP, runner)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		cancel()
		return errors.Join(err, closeConnection())
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		cancel()
		return errors.Join(err, listener.Close(), closeConnection())
	}
	password := hex.EncodeToString(secret)
	filesystem := &sftpWebDAV{client: client, readOnly: !writable, verify: live.target.Verify}
	server := &http.Server{
		Handler:           nativeWebDAVHandler(webDAVMountPath(live.record.ID)[1:], password, filesystem),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 * 1024,
	}
	done := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
		close(done)
	}()
	go func() {
		_ = client.Wait()
		_ = server.Close()
	}()
	live.done = done
	live.closeBridge = func(ctx context.Context) error {
		serverErr := server.Close()
		closed := make(chan error, 1)
		go func() { closed <- closeConnection() }()
		select {
		case closeErr := <-closed:
			cancel()
			live.closeBridge, live.done = nil, nil
			return errors.Join(serverErr, closeErr)
		case <-ctx.Done():
			cancel()
			killErr := process.Kill()
			if errors.Is(killErr, os.ErrProcessDone) {
				killErr = nil
			}
			return errors.Join(serverErr, fmt.Errorf("native SFTP bridge did not close: %w", ctx.Err()), killErr)
		}
	}
	live.record.Endpoint = "http://" + listener.Addr().String() + webDAVMountPath(live.record.ID)
	if err := m.save(live.record); err != nil {
		return err
	}
	credentials, err := os.CreateTemp(m.root, ".webdav-credentials-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, credentials.Close()) }()
	if err := os.Remove(credentials.Name()); err != nil {
		return err
	}
	// macOS's credential descriptor avoids URLs, arguments, keychains and persistent secret files.
	for _, value := range []string{"porto", password, "", "", ""} {
		if err := binary.Write(credentials, binary.BigEndian, uint32(len(value))); err != nil {
			return err
		}
		if _, err := io.WriteString(credentials, value); err != nil {
			return err
		}
	}
	if _, err := credentials.Seek(0, io.SeekStart); err != nil {
		return err
	}
	options := "nobrowse,nodev,nosuid,noexec"
	if !writable {
		options += ",rdonly"
	}
	mount, cancelMount := context.WithTimeout(ctx, 30*time.Second)
	defer cancelMount()
	output, err := m.runner.Run(mount, runtimes.Command{
		Name:        "/sbin/mount_webdav",
		Args:        []string{"-S", "-a", "0", "-o", options, "-v", "Porto-" + live.record.Identity[:12], live.record.Endpoint, live.record.Path},
		StdinReader: credentials,
	})
	if err != nil {
		return fmt.Errorf("macOS native filesystem mount failed: %w: %.1024s", err, output)
	}
	return m.waitMounted(ctx, live.record, live.done)
}
