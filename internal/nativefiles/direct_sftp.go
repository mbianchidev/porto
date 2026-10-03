package nativefiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/pkg/sftp"
)

type SFTPCommand struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
	Env  []string `json:"env,omitempty"`
}

type MountRequest struct {
	Resource datafiles.Resource `json:"resource"`
	Identity string             `json:"identity"`
	Path     string             `json:"path"`
	ReadOnly bool               `json:"readOnly"`
	Command  SFTPCommand        `json:"command"`
}

type MountEvent struct {
	Event    string `json:"event"`
	Identity string `json:"identity"`
	Path     string `json:"path"`
	Serial   uint32 `json:"serial,omitempty"`
	Message  string `json:"message,omitempty"`
}

type DirectSFTP struct {
	client   *sftp.Client
	readOnly bool
	mu       sync.Mutex
	next     uint64
	files    map[uint64]*sftp.File
	dirs     map[uint64][]os.FileInfo
}

func NewDirectSFTP(client *sftp.Client, readOnly bool) *DirectSFTP {
	return &DirectSFTP{client: client, readOnly: readOnly, next: 1, files: make(map[uint64]*sftp.File), dirs: make(map[uint64][]os.FileInfo)}
}

func NativePath(value string) (string, error) {
	if value == "" || !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00\r\n") {
		return "", datafiles.ErrInvalid
	}
	relative := strings.TrimPrefix(value, "/")
	if relative == "" {
		return "/", nil
	}
	if err := datafiles.ValidatePath(relative); err != nil {
		return "", err
	}
	return "/" + relative, nil
}

func (fs *DirectSFTP) writable() error {
	if fs.readOnly {
		return os.ErrPermission
	}
	return nil
}

func (fs *DirectSFTP) Stat(value string, handle uint64) (os.FileInfo, error) {
	if handle != 0 {
		file, err := fs.file(handle)
		if err != nil {
			return nil, err
		}
		return file.Stat()
	}
	name, err := NativePath(value)
	if err != nil {
		return nil, err
	}
	return fs.client.Lstat(name)
}

func (fs *DirectSFTP) Open(value string, flags int, mode os.FileMode) (uint64, error) {
	name, err := NativePath(value)
	if err != nil {
		return 0, err
	}
	if flags&(os.O_WRONLY|os.O_RDWR|os.O_TRUNC|os.O_CREATE|os.O_APPEND) != 0 {
		if err := fs.writable(); err != nil {
			return 0, err
		}
	}
	file, err := fs.client.OpenFile(name, flags)
	if err != nil {
		return 0, err
	}
	if flags&os.O_CREATE != 0 {
		if err := file.Chmod(mode.Perm()); err != nil {
			return 0, errors.Join(err, file.Close())
		}
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	handle := fs.next
	fs.next++
	fs.files[handle] = file
	return handle, nil
}

func (fs *DirectSFTP) file(handle uint64) (*sftp.File, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	file := fs.files[handle]
	if file == nil {
		return nil, os.ErrClosed
	}
	return file, nil
}

func (fs *DirectSFTP) Read(handle uint64, buffer []byte, offset int64) (int, error) {
	file, err := fs.file(handle)
	if err != nil {
		return 0, err
	}
	return file.ReadAt(buffer, offset)
}

func (fs *DirectSFTP) Write(handle uint64, buffer []byte, offset int64) (int, error) {
	if err := fs.writable(); err != nil {
		return 0, err
	}
	file, err := fs.file(handle)
	if err != nil {
		return 0, err
	}
	return file.WriteAt(buffer, offset)
}

func (fs *DirectSFTP) Release(handle uint64) error {
	fs.mu.Lock()
	file := fs.files[handle]
	delete(fs.files, handle)
	fs.mu.Unlock()
	if file == nil {
		return os.ErrClosed
	}
	return file.Close()
}

func (fs *DirectSFTP) Truncate(value string, handle uint64, size int64) error {
	if err := fs.writable(); err != nil {
		return err
	}
	if size < 0 || size > datafiles.MaxArchiveBytes {
		return datafiles.ErrLimit
	}
	if handle != 0 {
		file, err := fs.file(handle)
		if err != nil {
			return err
		}
		return file.Truncate(size)
	}
	name, err := NativePath(value)
	if err != nil {
		return err
	}
	return fs.client.Truncate(name, size)
}

func (fs *DirectSFTP) OpenDir(value string) (uint64, error) {
	name, err := NativePath(value)
	if err != nil {
		return 0, err
	}
	items, err := fs.client.ReadDir(name)
	if err != nil {
		return 0, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	handle := fs.next
	fs.next++
	fs.dirs[handle] = items
	return handle, nil
}

func (fs *DirectSFTP) ReadDir(handle uint64) ([]os.FileInfo, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	items, exists := fs.dirs[handle]
	if !exists {
		return nil, os.ErrClosed
	}
	return items, nil
}

func (fs *DirectSFTP) ReleaseDir(handle uint64) {
	fs.mu.Lock()
	delete(fs.dirs, handle)
	fs.mu.Unlock()
}

func (fs *DirectSFTP) Change(value, action string, mode os.FileMode, uid, gid int, atime, mtime time.Time) error {
	if err := fs.writable(); err != nil {
		return err
	}
	name, err := NativePath(value)
	if err != nil {
		return err
	}
	switch action {
	case "mkdir":
		if err := fs.client.Mkdir(name); err != nil {
			return err
		}
		return fs.client.Chmod(name, mode.Perm())
	case "remove":
		return fs.client.Remove(name)
	case "rmdir":
		return fs.client.RemoveDirectory(name)
	case "chmod":
		return fs.client.Chmod(name, mode.Perm())
	case "chown":
		return fs.client.Chown(name, uid, gid)
	case "times":
		return fs.client.Chtimes(name, atime, mtime)
	default:
		return datafiles.ErrInvalid
	}
}

func (fs *DirectSFTP) Rename(source, destination string) error {
	if err := fs.writable(); err != nil {
		return err
	}
	from, err := NativePath(source)
	if err != nil {
		return err
	}
	to, err := NativePath(destination)
	if err != nil {
		return err
	}
	return fs.client.PosixRename(from, to)
}

func (fs *DirectSFTP) Link(source, destination string, symbolic bool) error {
	if err := fs.writable(); err != nil {
		return err
	}
	to, err := NativePath(destination)
	if err != nil {
		return err
	}
	if symbolic {
		if source == "" || strings.HasPrefix(source, "/") || strings.ContainsAny(source, "\\\x00\r\n") ||
			strings.HasPrefix(path.Clean(path.Join(path.Dir(to), source)), "/../") {
			return datafiles.ErrInvalid
		}
		return fs.client.Symlink(source, to)
	}
	from, err := NativePath(source)
	if err != nil {
		return err
	}
	return fs.client.Link(from, to)
}

func (fs *DirectSFTP) Readlink(value string) (string, error) {
	name, err := NativePath(value)
	if err != nil {
		return "", err
	}
	return fs.client.ReadLink(name)
}

func (fs *DirectSFTP) Space() (*sftp.StatVFS, error) { return fs.client.StatVFS("/") }

func (fs *DirectSFTP) Close() error {
	fs.mu.Lock()
	handles := fs.files
	fs.files = make(map[uint64]*sftp.File)
	fs.dirs = make(map[uint64][]os.FileInfo)
	fs.mu.Unlock()
	var result error
	for _, file := range handles {
		result = errors.Join(result, file.Close())
	}
	return errors.Join(result, fs.client.Close())
}

func ConnectNativeSFTP(ctx context.Context, command SFTPCommand) (*sftp.Client, runtimes.Process, func() error, error) {
	if command.Name == "" || len(command.Args) == 0 {
		return nil, nil, nil, datafiles.ErrInvalid
	}
	connection, err := (runtimes.ExecRunner{}).Start(ctx, runtimes.Command{Name: command.Name, Args: command.Args, Env: command.Env})
	if err != nil {
		return nil, nil, nil, err
	}
	var stderr limitedNativeErrors
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&stderr, connection.Stderr()); close(done) }()
	client, err := sftp.NewClientPipe(connection.Stdout(), connection.Stdin())
	if err != nil {
		_ = connection.Kill()
		return nil, nil, nil, errors.Join(err, connection.Wait(), fmt.Errorf("native SFTP initialization failed: %s", stderr.String()))
	}
	closeConnection := func() error {
		_ = client.Close()
		_ = connection.Stdin().Close()
		waitErr := connection.Wait()
		<-done
		if waitErr != nil {
			return fmt.Errorf("native SFTP transport failed: %w: %s", waitErr, stderr.String())
		}
		return nil
	}
	return client, connection, closeConnection, nil
}

type limitedNativeErrors struct {
	mu   sync.Mutex
	text strings.Builder
}

func (output *limitedNativeErrors) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if remaining := 4096 - output.text.Len(); remaining > 0 {
		output.text.Write(data[:min(len(data), remaining)])
	}
	return len(data), nil
}

func (output *limitedNativeErrors) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.text.String()
}
