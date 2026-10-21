package nativefiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Local        bool
	Backend      *runtimefiles.Descriptor
	Token        string
	NamespacePID int32
	SFTP         *SFTPCommand
	Verify       func(context.Context) error
	Release      func(context.Context) error
}

type Capability struct {
	Supported bool                `json:"supported"`
	Driver    string              `json:"driver"`
	Message   string              `json:"message"`
	Fallback  string              `json:"fallback"`
	Resource  *datafiles.Resource `json:"resource,omitempty"`
	Identity  string              `json:"identity,omitempty"`
}

type Attachment struct {
	ID              string                   `json:"id"`
	Resource        datafiles.Resource       `json:"resource"`
	Identity        string                   `json:"identity"`
	Path            string                   `json:"path"`
	ReadOnly        bool                     `json:"readOnly"`
	State           string                   `json:"state"`
	Message         string                   `json:"message,omitempty"`
	CreatedAt       string                   `json:"createdAt"`
	Local           bool                     `json:"local"`
	Backend         *runtimefiles.Descriptor `json:"backend,omitempty"`
	Token           string                   `json:"token,omitempty"`
	NamespacePID    int32                    `json:"namespacePid,omitempty"`
	Driver          string                   `json:"driver,omitempty"`
	BridgePID       int                      `json:"bridgePid,omitempty"`
	BridgeStart     uint64                   `json:"bridgeStart,omitempty"`
	Serial          uint32                   `json:"serial,omitempty"`
	BackendReleased bool                     `json:"backendReleased,omitempty"`
	Endpoint        string                   `json:"endpoint,omitempty"`
}

type liveAttachment struct {
	record      Attachment
	target      Target
	process     runtimes.Process
	cancel      context.CancelFunc
	done        <-chan error
	closeBridge func(context.Context) error
	stdinMu     sync.Mutex
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
	if runtime.GOOS == "darwin" {
		return m.webDAVCapability()
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

// Attach owns the initialized target's release, including failed startup cleanup.
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
	registered := false
	defer func() {
		if err != nil && !registered {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			err = errors.Join(err, target.Release(cleanup))
		}
	}()
	id := attachmentID(target.Resource, writable)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.attachments[id]; existing != nil {
		registered = true
		return m.getLocked(ctx, id)
	}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return result, err
	}
	if err := os.Chmod(m.root, 0o700); err != nil {
		return result, err
	}
	if runtime.GOOS == "windows" {
		if err := target.Verify(ctx); err != nil {
			return result, err
		}
		capability := m.Capability(ctx, target.Local)
		if !capability.Supported {
			return result, fmt.Errorf("%w: %s %s", datafiles.ErrUnsupported, capability.Message, capability.Fallback)
		}
		return m.attachWindowsLocked(ctx, target, writable)
	}
	result = Attachment{
		ID: id, Resource: target.Resource, Identity: target.Identity, ReadOnly: !writable, State: "connecting",
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Backend: target.Backend, Token: target.Token,
		Local:        target.Local,
		NamespacePID: target.NamespacePID,
	}
	if runtime.GOOS == "darwin" {
		result.Driver = "webdav"
	}
	live := &liveAttachment{target: target, record: result}
	if target.Local {
		if runtime.GOOS != "linux" || !filepath.IsAbs(target.RemotePath) {
			return result, datafiles.ErrUnsupported
		}
		result.Path = target.RemotePath
	} else {
		if target.SFTP == nil {
			return result, fmt.Errorf("%w: native files require an identity-confined SFTP transport", datafiles.ErrUnsupported)
		}
		result.Path = filepath.Join(m.root, id)
		if err := os.Mkdir(result.Path, 0o700); err != nil {
			return result, fmt.Errorf("native mountpoint already exists or cannot be created: %w", err)
		}
	}
	live.record = result
	if err := m.save(result); err != nil {
		if !target.Local {
			err = errors.Join(err, os.Remove(result.Path))
		}
		return result, err
	}
	m.attachments[id] = live
	registered = true
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			err = errors.Join(err, m.detachLocked(cleanup, id))
			if remaining := m.attachments[id]; remaining != nil {
				result = remaining.record
			}
		}
	}()
	if err := target.Verify(ctx); err != nil {
		return result, err
	}
	capability := m.Capability(ctx, target.Local)
	if !capability.Supported {
		return result, fmt.Errorf("%w: %s %s", datafiles.ErrUnsupported, capability.Message, capability.Fallback)
	}
	if result.Driver == "webdav" {
		if err := m.mountWebDAVLocked(ctx, live, writable); err != nil {
			return live.record, err
		}
		result = live.record
	} else if !target.Local {
		if err := m.mountSSHFSLocked(ctx, live, writable); err != nil {
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
	return result, nil
}

func (m *Manager) waitMounted(ctx context.Context, record Attachment, done <-chan error) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return errors.Join(errors.New("native filesystem bridge exited before its mount was ready"), err)
		default:
		}
		mounted, err := m.mounted(ctx, record)
		if err != nil {
			return err
		}
		if mounted {
			info, err := os.Stat(record.Path)
			if err != nil {
				return fmt.Errorf("native mount is inaccessible: %w", err)
			}
			if !info.IsDir() {
				return errors.New("native mount is not an accessible directory")
			}
			return nil
		}
		select {
		case err := <-done:
			return errors.Join(errors.New("native filesystem bridge exited before its mount was ready"), err)
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
	defer m.mu.Unlock()
	return m.getLocked(ctx, id)
}

func (m *Manager) getLocked(ctx context.Context, id string) (Attachment, error) {
	live := m.attachments[id]
	if live == nil {
		return Attachment{}, os.ErrNotExist
	}
	if live.record.State == "detach-failed" {
		return live.record, fmt.Errorf("%w: native detach failed; retry detach before reconnecting", datafiles.ErrConflict)
	}
	if live.record.State != "connected" {
		return live.record, fmt.Errorf("%w: native attachment is %s; detach and explicitly reconnect", datafiles.ErrConflict, live.record.State)
	}
	err := live.target.Verify(ctx)
	if err == nil && live.done != nil {
		select {
		case processErr := <-live.done:
			err = errors.Join(errors.New("native filesystem bridge exited; detach and explicitly reconnect"), processErr)
		default:
		}
	}
	if err == nil {
		if live.record.Driver == "winfsp" {
			err = verifyWindowsMount(live.record)
		} else if !live.record.Local {
			var mounted bool
			mounted, err = m.mounted(ctx, live.record)
			if err == nil && !mounted {
				err = errors.New("native filesystem bridge is disconnected; detach and explicitly reconnect")
			}
		}
	}
	if err != nil {
		live.record.State, live.record.Message = "disconnected", err.Error()
		return live.record, errors.Join(err, m.save(live.record))
	}
	return live.record, nil
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
	return m.detachLocked(ctx, id)
}

func (m *Manager) detachLocked(ctx context.Context, id string) (err error) {
	live := m.attachments[id]
	if live == nil {
		return os.ErrNotExist
	}
	defer func() {
		if err != nil {
			live.record.State, live.record.Message = "detach-failed", err.Error()
			err = errors.Join(err, m.save(live.record))
		}
	}()
	if live.record.Driver == "winfsp" {
		if err := m.detachWindowsLocked(ctx, live); err != nil {
			return err
		}
	} else if !live.target.Local {
		mounted, err := m.mounted(ctx, live.record)
		if err != nil {
			return err
		}
		command := runtimes.Command{Name: "umount", Args: []string{live.record.Path}}
		if live.record.Driver == "webdav" && live.record.State == "disconnected" {
			command.Args = []string{"-f", live.record.Path}
		}
		if runtime.GOOS == "linux" {
			command = runtimes.Command{Name: "fusermount3", Args: []string{"-u", live.record.Path}}
		}
		if mounted {
			if output, unmountErr := m.runner.Run(ctx, command); unmountErr != nil {
				mounted, verifyErr := m.mounted(ctx, live.record)
				if mounted || verifyErr != nil {
					return errors.Join(fmt.Errorf("native detach failed; source data was not removed: %w: %.1024s", unmountErr, output), verifyErr)
				}
				log.Printf("native mount %s disappeared during unmount: %v: %.1024s", id, unmountErr, output)
			}
		}
	}
	if live.cancel != nil {
		live.cancel()
		if err := live.process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("stop owned native bridge: %w", err)
		}
		select {
		case <-live.done:
		case <-ctx.Done():
			return fmt.Errorf("wait for owned native bridge cleanup: %w", ctx.Err())
		}
		live.cancel, live.process, live.done = nil, nil, nil
	}
	if live.closeBridge != nil {
		if err := live.closeBridge(ctx); err != nil {
			return err
		}
		live.closeBridge, live.done = nil, nil
	}
	if !live.record.BackendReleased {
		if err := live.target.Release(ctx); err != nil {
			return err
		}
		live.record.BackendReleased = true
		if err := m.save(live.record); err != nil {
			return err
		}
	}
	if !live.target.Local && live.record.Driver != "winfsp" {
		if err := os.Remove(live.record.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(filepath.Join(m.root, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
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
