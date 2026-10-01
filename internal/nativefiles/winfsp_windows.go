//go:build windows

package nativefiles

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func ProbeNativeDriver() Capability {
	message := "Install WinFsp 2.1 or newer from https://winfsp.dev, then refresh native file capabilities. Driver installation needs administrator approval; mounts run as your ordinary user."
	if err := loadNativeDriver(); err != nil {
		return Capability{Driver: "WinFsp", Message: message + " " + err.Error(), Fallback: "Use Files or verified volume archives until the driver is installed."}
	}
	return Capability{Supported: true, Driver: "WinFsp / confined SFTP", Message: "Live, private host folders with synchronous random-access writes. No local data cache or delayed copy-back. Guest case/ownership semantics apply; escaping links and image writes are rejected.", Fallback: "Use Files or archives when a guest is disconnected."}
}

func loadNativeDriver() error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\WinFsp`, registry.QUERY_VALUE|registry.WOW64_32KEY)
	if err != nil {
		return fmt.Errorf("WinFsp is not installed: %w", err)
	}
	defer key.Close()
	directory, _, err := key.GetStringValue("InstallDir")
	if err != nil || !filepath.IsAbs(directory) {
		return errors.Join(errors.New("WinFsp installation directory is invalid"), err)
	}
	library := "winfsp-x64.dll"
	if runtime.GOARCH == "arm64" {
		library = "winfsp-a64.dll"
	} else if runtime.GOARCH != "amd64" {
		return errors.New("Porto native mounts require x64 or ARM64")
	}
	if err := windows.SetDefaultDllDirectories(windows.LOAD_LIBRARY_SEARCH_SYSTEM32 | windows.LOAD_LIBRARY_SEARCH_USER_DIRS); err != nil {
		return err
	}
	path, err := windows.UTF16PtrFromString(filepath.Join(directory, "bin"))
	if err != nil {
		return err
	}
	if _, err := windows.AddDllDirectory(path); err != nil {
		return err
	}
	handle, err := windows.LoadLibraryEx(filepath.Join(directory, "bin", library), 0, windows.LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|windows.LOAD_LIBRARY_SEARCH_SYSTEM32)
	if err != nil {
		return fmt.Errorf("load the installed WinFsp driver: %w", err)
	}
	return windows.FreeLibrary(handle)
}

func RunNativeMount(ctx context.Context, input io.Reader, output io.Writer) (err error) {
	reader := bufio.NewReaderSize(input, 1024*1024)
	header, err := reader.ReadSlice('\n')
	if err != nil {
		return err
	}
	var request MountRequest
	if err := json.Unmarshal(header, &request); err != nil {
		return err
	}
	if request.Identity != request.Resource.Fingerprint() || request.Identity == "" ||
		!filepath.IsAbs(request.Path) || filepath.Base(request.Path) != attachmentID(request.Resource, !request.ReadOnly) ||
		request.Resource.ReadOnly && !request.ReadOnly || request.Resource.Kind == "image" && !request.ReadOnly {
		return datafiles.ErrInvalid
	}
	if err := loadNativeDriver(); err != nil {
		return err
	}
	if _, err := os.Lstat(request.Path); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("native mount destination must not already exist"), err)
	}
	connectionContext, cancel := context.WithCancel(ctx)
	defer cancel()
	client, _, closeConnection, err := ConnectNativeSFTP(connectionContext, request.Command)
	if err != nil {
		return fmt.Errorf("connect owned native filesystem: %w", err)
	}
	defer func() {
		closeErr := closeConnection()
		cancel()
		err = errors.Join(err, closeErr)
	}()
	if _, err := client.Stat("/"); err != nil {
		return err
	}
	core := NewDirectSFTP(client, request.ReadOnly)
	ready := make(chan struct{})
	filesystem := &winfspFilesystem{core: core, ready: ready}
	host := fuse.NewFileSystemHost(filesystem)
	host.SetCapCaseInsensitive(false)
	host.SetCapReaddirPlus(true)
	host.SetCapDeleteAccess(true)
	host.SetCapOpenTrunc(true)
	stop := make(chan error, 1)
	go func() {
		var control MountEvent
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			stop <- readErr
		} else if err := json.Unmarshal([]byte(line), &control); err != nil || control.Event != "detach" || control.Identity != request.Identity {
			stop <- errors.Join(datafiles.ErrConflict, err)
		} else {
			stop <- nil
		}
	}()
	mounted := make(chan bool, 1)
	serial := nativeSerial(request.Identity)
	options := []string{
		"-o", "uid=-1,gid=-1,umask=077,attr_timeout=0,entry_timeout=0,negative_timeout=0",
		"-o", "FileSecurity=D:P(A;;FA;;;OW)",
		"-o", "ExactFileSystemName=PortoFS,FileInfoTimeout=0,DirInfoTimeout=0",
		"-o", "volname=Porto-" + request.Identity[:12],
		"-o", fmt.Sprintf("VolumeSerialNumber=%08x", serial),
	}
	if request.ReadOnly {
		options = append(options, "-o", "ro")
	}
	go func() { mounted <- host.Mount(request.Path, options) }()
	select {
	case <-ready:
	case ok := <-mounted:
		return fmt.Errorf("WinFsp mount exited before initialization (success=%v)", ok)
	case <-ctx.Done():
		return ctx.Err()
	case controlErr := <-stop:
		return errors.Join(errors.New("native mount owner disconnected before readiness"), controlErr)
	}
	if err := json.NewEncoder(output).Encode(MountEvent{Event: "initialised", Identity: request.Identity, Path: request.Path, Serial: serial}); err != nil {
		host.Unmount()
		return err
	}
	select {
	case controlErr := <-stop:
		err = controlErr
	case <-ctx.Done():
		err = ctx.Err()
	case ok := <-mounted:
		return fmt.Errorf("native filesystem disconnected (mount success=%v)", ok)
	}
	if !host.Unmount() {
		return errors.Join(err, errors.New("WinFsp refused owned native detach"))
	}
	select {
	case ok := <-mounted:
		if !ok {
			err = errors.Join(err, errors.New("WinFsp mount returned an error during detach"))
		}
	case <-time.After(20 * time.Second):
		err = errors.Join(err, errors.New("WinFsp detach did not complete; mount state must be recovered"))
	}
	return err
}

func nativeSerial(identity string) uint32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(identity))
	return hash.Sum32()
}

type winfspFilesystem struct {
	fuse.FileSystemBase
	core  *DirectSFTP
	ready chan struct{}
	once  sync.Once
}

func (fs *winfspFilesystem) Init() { fs.once.Do(func() { close(fs.ready) }) }

func (fs *winfspFilesystem) Destroy() {
	if err := fs.core.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "Porto: native filesystem close failed:", err)
	}
}

func nativeErrno(err error) int {
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return -fuse.ENOENT
	case errors.Is(err, os.ErrPermission):
		return -fuse.EACCES
	case errors.Is(err, os.ErrExist):
		return -fuse.EEXIST
	case errors.Is(err, os.ErrClosed), errors.Is(err, context.Canceled):
		return -fuse.ENOTCONN
	case errors.Is(err, datafiles.ErrInvalid):
		return -fuse.EINVAL
	case errors.Is(err, datafiles.ErrUnsupported):
		return -fuse.ENOTSUP
	default:
		return -fuse.EIO
	}
}

func windowsStat(info os.FileInfo, stat *fuse.Stat_t) {
	stat.Mode = uint32(info.Mode().Perm())
	switch {
	case info.IsDir():
		stat.Mode |= fuse.S_IFDIR
	case info.Mode()&os.ModeSymlink != 0:
		stat.Mode |= fuse.S_IFLNK
	default:
		stat.Mode |= fuse.S_IFREG
	}
	stat.Size, stat.Nlink, stat.Blksize = info.Size(), 1, 4096
	stat.Mtim = fuse.NewTimespec(info.ModTime())
	stat.Atim, stat.Ctim, stat.Birthtim = stat.Mtim, stat.Mtim, stat.Mtim
}

func (fs *winfspFilesystem) Getattr(value string, stat *fuse.Stat_t, handle uint64) int {
	if handle == ^uint64(0) {
		handle = 0
	}
	info, err := fs.core.Stat(value, handle)
	if err == nil {
		windowsStat(info, stat)
	}
	return nativeErrno(err)
}

func (fs *winfspFilesystem) Statfs(_ string, stat *fuse.Statfs_t) int {
	value, err := fs.core.Space()
	if err != nil {
		return nativeErrno(err)
	}
	stat.Bsize, stat.Frsize, stat.Blocks = value.Bsize, value.Frsize, value.Blocks
	stat.Bfree, stat.Bavail, stat.Files = value.Bfree, value.Bavail, value.Files
	stat.Ffree, stat.Favail, stat.Namemax = value.Ffree, value.Favail, value.Namemax
	return 0
}

func nativeOpenFlags(flags int) int {
	result := os.O_RDONLY
	switch flags & (fuse.O_WRONLY | fuse.O_RDWR) {
	case fuse.O_WRONLY:
		result = os.O_WRONLY
	case fuse.O_RDWR:
		result = os.O_RDWR
	}
	for from, to := range map[int]int{fuse.O_CREAT: os.O_CREATE, fuse.O_TRUNC: os.O_TRUNC, fuse.O_EXCL: os.O_EXCL} {
		if flags&from != 0 {
			result |= to
		}
	}
	return result
}

func (fs *winfspFilesystem) Open(value string, flags int) (int, uint64) {
	handle, err := fs.core.Open(value, nativeOpenFlags(flags), 0o600)
	return nativeErrno(err), handle
}

func (fs *winfspFilesystem) Create(value string, flags int, mode uint32) (int, uint64) {
	handle, err := fs.core.Open(value, nativeOpenFlags(flags)|os.O_CREATE, os.FileMode(mode))
	return nativeErrno(err), handle
}

func (fs *winfspFilesystem) Read(_ string, buffer []byte, offset int64, handle uint64) int {
	count, err := fs.core.Read(handle, buffer, offset)
	if errors.Is(err, io.EOF) {
		return count
	}
	if err != nil {
		return nativeErrno(err)
	}
	return count
}

func (fs *winfspFilesystem) Write(_ string, buffer []byte, offset int64, handle uint64) int {
	count, err := fs.core.Write(handle, buffer, offset)
	if err != nil {
		return nativeErrno(err)
	}
	return count
}

func (fs *winfspFilesystem) Release(_ string, handle uint64) int {
	return nativeErrno(fs.core.Release(handle))
}

func (fs *winfspFilesystem) Truncate(value string, size int64, handle uint64) int {
	if handle == ^uint64(0) {
		handle = 0
	}
	return nativeErrno(fs.core.Truncate(value, handle, size))
}

func (fs *winfspFilesystem) Flush(_ string, _ uint64) int { return 0 }
func (fs *winfspFilesystem) Fsync(_ string, _ bool, _ uint64) int {
	// The confined SFTP server fsyncs each write before acknowledging it.
	return 0
}

func (fs *winfspFilesystem) Opendir(value string) (int, uint64) {
	handle, err := fs.core.OpenDir(value)
	return nativeErrno(err), handle
}

func (fs *winfspFilesystem) Readdir(_ string, fill func(string, *fuse.Stat_t, int64) bool, offset int64, handle uint64) int {
	items, err := fs.core.ReadDir(handle)
	if err != nil {
		return nativeErrno(err)
	}
	names := []string{".", ".."}
	for index := offset; index < int64(len(items)+2); index++ {
		var stat fuse.Stat_t
		name := ""
		if index < 2 {
			name = names[index]
			stat.Mode = fuse.S_IFDIR | 0o700
		} else {
			info := items[index-2]
			name = info.Name()
			windowsStat(info, &stat)
		}
		if !fill(name, &stat, index+1) {
			break
		}
	}
	return 0
}

func (fs *winfspFilesystem) Releasedir(_ string, handle uint64) int {
	fs.core.ReleaseDir(handle)
	return 0
}

func (fs *winfspFilesystem) Rename(from, to string) int { return nativeErrno(fs.core.Rename(from, to)) }
func (fs *winfspFilesystem) Readlink(value string) (int, string) {
	target, err := fs.core.Readlink(value)
	return nativeErrno(err), target
}
func (fs *winfspFilesystem) Symlink(target, value string) int {
	return nativeErrno(fs.core.Link(target, value, true))
}
func (fs *winfspFilesystem) Link(from, to string) int {
	return nativeErrno(fs.core.Link(from, to, false))
}
func (fs *winfspFilesystem) Mkdir(value string, mode uint32) int {
	return nativeErrno(fs.core.Change(value, "mkdir", os.FileMode(mode), 0, 0, time.Time{}, time.Time{}))
}
func (fs *winfspFilesystem) Unlink(value string) int {
	return nativeErrno(fs.core.Change(value, "remove", 0, 0, 0, time.Time{}, time.Time{}))
}
func (fs *winfspFilesystem) Rmdir(value string) int {
	return nativeErrno(fs.core.Change(value, "rmdir", 0, 0, 0, time.Time{}, time.Time{}))
}
func (fs *winfspFilesystem) Chmod(value string, mode uint32) int {
	return nativeErrno(fs.core.Change(value, "chmod", os.FileMode(mode), 0, 0, time.Time{}, time.Time{}))
}
func (fs *winfspFilesystem) Chown(value string, uid, gid uint32) int {
	return nativeErrno(fs.core.Change(value, "chown", 0, int(uid), int(gid), time.Time{}, time.Time{}))
}
func (fs *winfspFilesystem) Utimens(value string, times []fuse.Timespec) int {
	if len(times) != 2 {
		return -fuse.EINVAL
	}
	return nativeErrno(fs.core.Change(value, "times", 0, 0, 0, time.Unix(times[0].Sec, times[0].Nsec), time.Unix(times[1].Sec, times[1].Nsec)))
}
func (fs *winfspFilesystem) Access(_ string, mask uint32) int {
	if fs.core.readOnly && mask&(fuse.W_OK|fuse.DELETE_OK) != 0 {
		return -fuse.EROFS
	}
	return 0
}
