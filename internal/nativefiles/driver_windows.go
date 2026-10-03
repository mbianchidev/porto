//go:build windows

package nativefiles

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimes"
	"golang.org/x/sys/windows"
)

func (m *Manager) mountHelper() (string, error) {
	if override := os.Getenv("PORTO_TEST_NATIVE_MOUNT_HELPER"); override != "" {
		return override, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	bundled := filepath.Join(filepath.Dir(executable), "runtime", "bin", "porto-files-mount.exe")
	if info, err := os.Stat(bundled); err == nil && info.Mode().IsRegular() {
		return bundled, nil
	}
	return m.lookPath("porto-files-mount.exe")
}

func (m *Manager) windowsCapability(ctx context.Context) Capability {
	helper, err := m.mountHelper()
	if err != nil {
		return Capability{Driver: "WinFsp", Message: "The native mount helper is missing. Install the full Porto desktop package.", Fallback: "Use Files or local volume archives."}
	}
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := m.runner.Run(probe, runtimes.Command{Name: helper, Args: []string{"--probe"}})
	if err != nil {
		return Capability{Driver: "WinFsp", Message: "Native driver probe failed: " + err.Error(), Fallback: "Use Files or local volume archives."}
	}
	var capability Capability
	if err := json.Unmarshal(output, &capability); err != nil {
		return Capability{Driver: "WinFsp", Message: "Native driver probe returned invalid metadata.", Fallback: "Use Files or local volume archives."}
	}
	return capability
}

func (m *Manager) attachWindowsLocked(ctx context.Context, target Target, writable bool) (record Attachment, err error) {
	if target.SFTP == nil {
		return record, fmt.Errorf("%w: this resource has no identity-confined SFTP transport", datafiles.ErrUnsupported)
	}
	helper, err := m.mountHelper()
	if err != nil {
		return record, err
	}
	runner, ok := m.runner.(runtimes.ProcessRunner)
	if !ok {
		return record, datafiles.ErrUnsupported
	}
	id := attachmentID(target.Resource, writable)
	record = Attachment{
		ID: id, Identity: target.Identity, Resource: target.Resource, Path: filepath.Join(m.root, id),
		ReadOnly: !writable, State: "connecting", Driver: "winfsp", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if _, err := os.Lstat(record.Path); !errors.Is(err, os.ErrNotExist) {
		return record, errors.Join(datafiles.ErrConflict, err)
	}
	ownedContext, cancel := context.WithCancel(context.Background())
	process, err := runner.Start(ownedContext, runtimes.Command{Name: helper})
	if err != nil {
		cancel()
		return record, err
	}
	live := &liveAttachment{record: record, target: target, process: process, cancel: cancel}
	installed := false
	defer func() {
		if installed {
			return
		}
		cancel()
		_ = process.Kill()
		_ = process.Stdin().Close()
		if live.done != nil {
			select {
			case <-live.done:
			case <-time.After(10 * time.Second):
				err = errors.Join(err, errors.New("native mount startup cleanup timed out"))
			}
		}
	}()
	if err := json.NewEncoder(process.Stdin()).Encode(MountRequest{
		Resource: target.Resource, Identity: target.Identity, Path: record.Path, ReadOnly: !writable, Command: *target.SFTP,
	}); err != nil {
		return record, err
	}
	event := make(chan MountEvent, 1)
	outputDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReaderSize(process.Stdout(), 4096)
		line, err := reader.ReadString('\n')
		if err != nil {
			outputDone <- err
			return
		}
		var ready MountEvent
		if len(line) > 4096 {
			outputDone <- datafiles.ErrLimit
			return
		}
		if err := json.Unmarshal([]byte(line), &ready); err != nil {
			outputDone <- err
			return
		}
		event <- ready
		_, err = io.Copy(io.Discard, reader)
		outputDone <- err
	}()
	var diagnostics limitedNativeErrors
	stderrDone := make(chan struct{})
	go func() { _, _ = io.Copy(&diagnostics, process.Stderr()); close(stderrDone) }()
	done := make(chan error, 1)
	live.done = done
	go func() {
		outputErr := <-outputDone
		<-stderrDone
		waitErr := process.Wait()
		if waitErr != nil {
			waitErr = fmt.Errorf("native mount process failed: %w: %s", waitErr, diagnostics.String())
		}
		done <- errors.Join(outputErr, waitErr)
		close(done)
	}()
	startup, cancelStartup := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStartup()
	select {
	case ready := <-event:
		if ready.Event != "initialised" || ready.Identity != record.Identity || ready.Path != record.Path || ready.Serial != nativeSerial(record.Identity) {
			return record, datafiles.ErrConflict
		}
		record.Serial = ready.Serial
	case processErr := <-done:
		return record, errors.Join(errors.New("native mount exited before readiness"), processErr)
	case <-startup.Done():
		return record, startup.Err()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := verifyWindowsMount(record); err == nil {
			break
		}
		select {
		case <-ticker.C:
		case processErr := <-done:
			return record, errors.Join(errors.New("native mount exited before becoming accessible"), processErr)
		case <-startup.Done():
			return record, fmt.Errorf("WinFsp did not expose the owned host folder: %w", startup.Err())
		}
	}
	record.BridgePID = process.PID()
	record.BridgeStart, err = windowsProcessStart(record.BridgePID)
	if err != nil {
		return record, err
	}
	record.State, record.Message = "connected", "WinFsp host folder: direct, synchronous SFTP writes; no cached copy-back."
	live.record = record
	if err := m.save(record); err != nil {
		return record, err
	}
	m.attachments[id] = live
	installed = true
	return record, nil
}

func verifyWindowsMount(record Attachment) error {
	name, err := windows.UTF16PtrFromString(record.Path + string(filepath.Separator))
	if err != nil {
		return err
	}
	var serial, maximum, flags uint32
	label, filesystem := make([]uint16, 256), make([]uint16, 256)
	if err := windows.GetVolumeInformation(name, &label[0], uint32(len(label)), &serial, &maximum, &flags, &filesystem[0], uint32(len(filesystem))); err != nil {
		return fmt.Errorf("native host folder is disconnected: %w", err)
	}
	if serial != record.Serial || windows.UTF16ToString(label) != "Porto-"+record.Identity[:12] || windows.UTF16ToString(filesystem) != "PortoFS" {
		return fmt.Errorf("%w: host folder is not the exact Porto-owned WinFsp volume", datafiles.ErrConflict)
	}
	return nil
}

func windowsProcessStart(pid int) (uint64, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), nil
}

func (m *Manager) detachWindowsLocked(ctx context.Context, live *liveAttachment) error {
	if live.process == nil {
		created, err := windowsProcessStart(live.record.BridgePID)
		if err != nil {
			if err := verifyWindowsMount(live.record); err == nil {
				return errors.New("native mount process is absent but its host folder remains; refusing an unowned detach")
			}
			return nil
		}
		if created != live.record.BridgeStart {
			return datafiles.ErrConflict
		}
		if err := verifyWindowsMount(live.record); err != nil {
			return err
		}
		if _, err := m.runner.Run(ctx, runtimes.Command{Name: "taskkill.exe", Args: []string{
			"/PID", strconv.Itoa(live.record.BridgePID), "/T", "/F",
		}}); err != nil {
			return err
		}
	} else {
		live.stdinMu.Lock()
		err := json.NewEncoder(live.process.Stdin()).Encode(MountEvent{Event: "detach", Identity: live.record.Identity})
		live.stdinMu.Unlock()
		if err != nil {
			return err
		}
		select {
		case processErr := <-live.done:
			if processErr != nil {
				return processErr
			}
		case <-ctx.Done():
			return fmt.Errorf("wait for owned WinFsp detach: %w", ctx.Err())
		}
		live.cancel()
		_ = live.process.Stdin().Close()
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Lstat(live.record.Path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		if err := verifyWindowsMount(live.record); err != nil {
			return errors.New("owned mount detached but the host path was replaced; refusing to delete it")
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return fmt.Errorf("native folder still mounted after detach: %w", ctx.Err())
		}
	}
}
