package nativefiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimefiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type Target struct {
	Resource     datafiles.Resource
	Identity     string
	RemotePath   string
	SSHConfig    string
	SSHHost      string
	Local        bool
	Backend      *runtimefiles.Descriptor
	Token        string
	NamespacePID int32
	SFTP         *SFTPCommand
	Verify       func(context.Context) error
	Release      func(context.Context) error
}

type Capability struct {
	Supported bool   `json:"supported"`
	Driver    string `json:"driver"`
	Message   string `json:"message"`
	Fallback  string `json:"fallback"`
}

type Attachment struct {
	ID           string                   `json:"id"`
	Resource     datafiles.Resource       `json:"resource"`
	Identity     string                   `json:"identity"`
	Path         string                   `json:"path"`
	ReadOnly     bool                     `json:"readOnly"`
	State        string                   `json:"state"`
	Message      string                   `json:"message,omitempty"`
	CreatedAt    string                   `json:"createdAt"`
	Local        bool                     `json:"local"`
	Backend      *runtimefiles.Descriptor `json:"backend,omitempty"`
	Token        string                   `json:"token,omitempty"`
	NamespacePID int32                    `json:"namespacePid,omitempty"`
	Driver       string                   `json:"driver,omitempty"`
	BridgePID    int                      `json:"bridgePid,omitempty"`
	BridgeStart  uint64                   `json:"bridgeStart,omitempty"`
	Serial       uint32                   `json:"serial,omitempty"`
}

type liveAttachment struct {
	record  Attachment
	target  Target
	process runtimes.Process
	cancel  context.CancelFunc
	done    <-chan error
	stdinMu sync.Mutex
}

type Manager struct {
	mu          sync.Mutex
	root        string
	runner      runtimes.Runner
	lookPath    func(string) (string, error)
	attachments map[string]*liveAttachment
}

func New(root string, runner runtimes.Runner) *Manager {
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	return &Manager{root: root, runner: runner, lookPath: runtimes.LookPath, attachments: make(map[string]*liveAttachment)}
}

func (m *Manager) Capability(ctx context.Context, local bool) Capability {
	fallback := "Use the in-app Files inspector, verified volume archives, or porto vm copy. No implicit host copy-back is performed."
	if local && runtime.GOOS == "linux" {
		return Capability{Supported: true, Driver: "Linux bind mount", Message: "Backend-local, ownership-checked bind mount; images are kernel read-only.", Fallback: fallback}
	}
	if runtime.GOOS == "windows" {
		return m.windowsCapability(ctx)
	}
	binary, err := m.lookPath("sshfs")
	if err != nil {
		return Capability{Driver: "SSHFS", Message: "Install a maintained SSHFS build with symlink containment and the platform filesystem driver (macFUSE on macOS, WinFsp on Windows, FUSE3 on Linux).", Fallback: fallback}
	}
	output, err := m.runner.Run(ctx, runtimes.Command{Name: binary, Args: []string{"--help"}})
	if err != nil || !strings.Contains(string(output), "contain_symlinks") {
		return Capability{Driver: "SSHFS", Message: "The installed SSHFS lacks verified symlink containment. Upgrade it; Porto refuses an unsafe mount rather than exposing unrelated files.", Fallback: fallback}
	}
	return Capability{Supported: true, Driver: "SSHFS", Message: "Private foreground SSHFS bridge with contained links, uncached I/O and explicit read/write boundaries. Filesystem notifications are not guaranteed; refresh or poll for remote changes.", Fallback: fallback}
}

func attachmentID(resource datafiles.Resource, writable bool) string {
	suffix := "-ro"
	if writable {
		suffix = "-rw"
	}
	return resource.Fingerprint() + suffix
}

func (m *Manager) Attach(ctx context.Context, target Target, writable bool) (result Attachment, err error) {
	if target.Resource.ID == "" || target.Identity != target.Resource.Fingerprint() {
		return result, datafiles.ErrConflict
	}
	if writable && (target.Resource.Kind == "image" || target.Resource.ReadOnly) {
		return result, fmt.Errorf("%w: this resource is immutable or read-only", datafiles.ErrUnsupported)
	}
	if target.Verify == nil || target.Release == nil {
		return result, errors.New("native attachment lifecycle is not initialized")
	}
	if err := target.Verify(ctx); err != nil {
		return result, err
	}
	capability := m.Capability(ctx, target.Local)
	if !capability.Supported {
		return result, fmt.Errorf("%w: %s %s", datafiles.ErrUnsupported, capability.Message, capability.Fallback)
	}
	id := attachmentID(target.Resource, writable)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.attachments[id]; existing != nil {
		return existing.record, nil
	}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return result, err
	}
	if err := os.Chmod(m.root, 0o700); err != nil {
		return result, err
	}
	if runtime.GOOS == "windows" {
		return m.attachWindowsLocked(ctx, target, writable)
	}
	result = Attachment{
		ID: id, Resource: target.Resource, Identity: target.Identity, ReadOnly: !writable, State: "connecting",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Backend: target.Backend, Token: target.Token,
		Local:        target.Local,
		NamespacePID: target.NamespacePID,
	}
	live := &liveAttachment{target: target, record: result}
	if target.Local {
		if runtime.GOOS != "linux" || !filepath.IsAbs(target.RemotePath) {
			return result, datafiles.ErrUnsupported
		}
		result.Path = target.RemotePath
	} else {
		if !filepath.IsAbs(target.SSHConfig) || target.SSHHost == "" || !strings.HasPrefix(target.RemotePath, "/") || strings.ContainsAny(target.SSHHost+target.RemotePath, "\x00\r\n") {
			return result, datafiles.ErrInvalid
		}
		result.Path = filepath.Join(m.root, id)
		if err := os.Mkdir(result.Path, 0o700); err != nil {
			return result, fmt.Errorf("native mountpoint already exists or cannot be created: %w", err)
		}
		runner, ok := m.runner.(runtimes.ProcessRunner)
		if !ok {
			return result, datafiles.ErrUnsupported
		}
		binary, err := m.lookPath("sshfs")
		if err != nil {
			return result, err
		}
		ownedContext, cancel := context.WithCancel(context.Background())
		live.cancel = cancel
		options := "contain_symlinks,default_permissions,idmap=user,direct_io,dir_cache=no,nodev,nosuid,noexec,ServerAliveInterval=5,ServerAliveCountMax=2"
		if !writable {
			options += ",ro"
		}
		args := []string{"-f", "-F", target.SSHConfig, "-o", options}
		if target.Resource.Kind != "vm" {
			server := "sudo -n /usr/lib/openssh/sftp-server"
			if target.NamespacePID > 0 {
				server = fmt.Sprintf("sudo -n nsenter --target %d --mount --user -- /usr/lib/openssh/sftp-server", target.NamespacePID)
			}
			args = append(args, "-o", "sftp_server="+server)
		}
		args = append(args, target.SSHHost+":"+target.RemotePath, result.Path)
		process, err := runner.Start(ownedContext, runtimes.Command{Name: binary, Args: args})
		if err != nil {
			cancel()
			_ = os.Remove(result.Path)
			return result, err
		}
		live.process = process
		_ = process.Stdin().Close()
		diagnostics := make(chan error, 1)
		go func() {
			output, readErr := io.ReadAll(io.LimitReader(process.Stderr(), 4096))
			if readErr != nil {
				diagnostics <- readErr
				return
			}
			if len(output) != 0 {
				diagnostics <- fmt.Errorf("SSHFS: %.4096s", output)
				return
			}
			diagnostics <- errors.New("SSHFS exited before its mount was ready")
		}()
		go func() { _, _ = io.Copy(io.Discard, process.Stdout()) }()
		if err := m.waitMounted(ctx, result.Path, diagnostics); err != nil {
			cancel()
			_ = process.Kill()
			_ = process.Wait()
			_ = os.Remove(result.Path)
			return result, err
		}
	}
	result.State = "connected"
	result.Message = capability.Message
	live.record = result
	if err := m.save(result); err != nil {
		if live.cancel != nil {
			live.cancel()
		}
		return result, err
	}
	m.attachments[id] = live
	return result, nil
}

func (m *Manager) waitMounted(ctx context.Context, mountpoint string, diagnostics <-chan error) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		output, err := m.runner.Run(ctx, runtimes.Command{Name: "mount"})
		if err == nil && strings.Contains(string(output), " on "+mountpoint+" ") {
			return nil
		}
		select {
		case err := <-diagnostics:
			return err
		case <-ctx.Done():
			return fmt.Errorf("native filesystem mount did not become accessible: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (m *Manager) save(record Attachment) error {
	document, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temporary := filepath.Join(m.root, record.ID+".json.tmp")
	if err := os.WriteFile(temporary, document, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(m.root, record.ID+".json"))
}

func (m *Manager) List() []Attachment {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Attachment, 0, len(m.attachments))
	for _, live := range m.attachments {
		result = append(result, live.record)
	}
	return result
}

func (m *Manager) Get(ctx context.Context, id string) (Attachment, error) {
	m.mu.Lock()
	live := m.attachments[id]
	m.mu.Unlock()
	if live == nil {
		return Attachment{}, os.ErrNotExist
	}
	if err := live.target.Verify(ctx); err != nil {
		m.mu.Lock()
		live.record.State, live.record.Message = "disconnected", err.Error()
		record := live.record
		m.mu.Unlock()
		return record, fmt.Errorf("%w: native resource is disconnected or its identity changed", datafiles.ErrConflict)
	}
	if live.record.Driver == "winfsp" {
		if err := verifyWindowsMount(live.record); err != nil {
			m.mu.Lock()
			live.record.State, live.record.Message = "disconnected", err.Error()
			record := live.record
			m.mu.Unlock()
			return record, err
		}
	}
	m.mu.Lock()
	record := live.record
	m.mu.Unlock()
	return record, nil
}

func (m *Manager) Guard(kind, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, live := range m.attachments {
		if live.record.Resource.Kind == kind && (name == "*" || live.record.Resource.Name == name) {
			return fmt.Errorf("%w: detach native files for %s before changing its storage", datafiles.ErrConflict, live.record.Resource.Name)
		}
	}
	return nil
}

func (m *Manager) Detach(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	live := m.attachments[id]
	if live == nil {
		return os.ErrNotExist
	}
	if live.record.Driver == "winfsp" {
		if err := m.detachWindowsLocked(ctx, live); err != nil {
			live.record.State, live.record.Message = "detach-failed", err.Error()
			return errors.Join(err, m.save(live.record))
		}
		if err := live.target.Release(ctx); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(m.root, id+".json")); err != nil {
			return err
		}
		delete(m.attachments, id)
		return nil
	}
	if !live.target.Local {
		mounted, err := m.mounted(ctx, live.record.Path)
		if err != nil {
			return err
		}
		command := runtimes.Command{Name: "umount", Args: []string{live.record.Path}}
		if runtime.GOOS == "linux" {
			command = runtimes.Command{Name: "fusermount3", Args: []string{"-u", live.record.Path}}
		}
		if mounted {
			if _, err := m.runner.Run(ctx, command); err != nil {
				live.record.State, live.record.Message = "detach-failed", err.Error()
				return errors.Join(fmt.Errorf("native detach failed; source data was not removed: %w", err), m.save(live.record))
			}
		}
	}
	if live.cancel != nil {
		live.cancel()
		_ = live.process.Kill()
		_ = live.process.Wait()
	}
	if err := live.target.Release(ctx); err != nil {
		live.record.State, live.record.Message = "detach-failed", err.Error()
		return errors.Join(err, m.save(live.record))
	}
	if !live.target.Local {
		if err := os.Remove(live.record.Path); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(m.root, id+".json")); err != nil {
		return err
	}
	delete(m.attachments, id)
	return nil
}

func (m *Manager) Close(ctx context.Context) error {
	var result error
	for _, record := range m.List() {
		result = errors.Join(result, m.Detach(ctx, record.ID))
	}
	return result
}

func (m *Manager) Monitor(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, record := range m.List() {
				probe, cancel := context.WithTimeout(ctx, 10*time.Second)
				_, err := m.Get(probe, record.ID)
				cancel()
				if err != nil {
					detach, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					if err := m.Detach(detach, record.ID); err != nil {
						log.Printf("detach disconnected native files: %v", err)
					}
					cancel()
				}
			}
		}
	}
}
