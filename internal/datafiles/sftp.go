package datafiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

type SFTPOptions struct {
	ReadOnly   bool
	ReadOnlyAt func(string) bool
	MapOwner   func(int, int) (int, int, error)
	Owner      func(int, int) (int, int, error)
	Verify     func(context.Context) error
}

func ServeSFTP(ctx context.Context, connection io.ReadWriteCloser, directory string, options SFTPOptions) (err error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fmt.Errorf("open native filesystem root: %w", err)
	}
	defer root.Close()
	handler := &rootedSFTP{ctx: ctx, root: root, directory: directory, options: options}
	server := sftp.NewRequestServer(connection, sftp.Handlers{
		FileGet: handler, FilePut: handler, FileCmd: handler, FileList: handler,
	})
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	err = server.Serve()
	closeErr := server.Close()
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
		err = nil
	}
	if errors.Is(closeErr, net.ErrClosed) || errors.Is(closeErr, os.ErrClosed) {
		closeErr = nil
	}
	return errors.Join(err, closeErr)
}

type rootedSFTP struct {
	ctx       context.Context
	root      *os.Root
	directory string
	options   SFTPOptions
}

func (h *rootedSFTP) check(value string, write bool) (string, error) {
	if err := h.ctx.Err(); err != nil {
		return "", err
	}
	if h.options.Verify != nil {
		if err := h.options.Verify(h.ctx); err != nil {
			return "", err
		}
	}
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00\r\n") {
		return "", ErrInvalid
	}
	relative := strings.TrimPrefix(value, "/")
	if relative == "" {
		relative = "."
	}
	if err := ValidatePath(relative); err != nil {
		return "", err
	}
	if write && (h.options.ReadOnly || h.options.ReadOnlyAt != nil && h.options.ReadOnlyAt(relative)) {
		return "", os.ErrPermission
	}
	return relative, nil
}

func (h *rootedSFTP) Fileread(request *sftp.Request) (io.ReaderAt, error) {
	return h.open(request, false)
}

func (h *rootedSFTP) Filewrite(request *sftp.Request) (io.WriterAt, error) {
	return h.open(request, true)
}

func (h *rootedSFTP) OpenFile(request *sftp.Request) (sftp.WriterAtReaderAt, error) {
	return h.open(request, true)
}

func (h *rootedSFTP) open(request *sftp.Request, writing bool) (*nativeFile, error) {
	name, err := h.check(request.Filepath, writing)
	if err != nil {
		return nil, err
	}
	flags := request.Pflags()
	mode := os.O_RDONLY
	if writing {
		mode = os.O_WRONLY
		if flags.Read {
			mode = os.O_RDWR
		}
		if flags.Creat {
			mode |= os.O_CREATE
		}
		if flags.Excl {
			mode |= os.O_EXCL
		}
		if flags.Trunc {
			mode |= os.O_TRUNC
		}
	}
	info, statErr := h.root.Stat(name)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if statErr == nil && !info.Mode().IsRegular() {
		return nil, ErrUnsupported
	}
	if flags.Append {
		return nil, fmt.Errorf("%w: SFTP append requests require explicit offsets", ErrUnsupported)
	}
	file, err := h.root.OpenFile(name, mode|nativeNonblockFlag(), 0o600)
	if err != nil {
		return nil, err
	}
	current, err := file.Stat()
	if err != nil || !current.Mode().IsRegular() {
		return nil, errors.Join(ErrUnsupported, err, file.Close())
	}
	if writing && statErr != nil && h.options.MapOwner != nil {
		uid, gid, err := h.options.MapOwner(0, 0)
		if err == nil {
			err = file.Chown(uid, gid)
		}
		if err != nil {
			return nil, errors.Join(err, file.Close())
		}
	}
	return &nativeFile{File: file, ctx: h.ctx, writing: writing}, nil
}

type nativeFile struct {
	*os.File
	ctx     context.Context
	writing bool
}

func (f *nativeFile) ReadAt(buffer []byte, offset int64) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	return f.File.ReadAt(buffer, offset)
}

func (f *nativeFile) WriteAt(buffer []byte, offset int64) (int, error) {
	if err := f.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := f.File.WriteAt(buffer, offset)
	if err == nil {
		err = f.File.Sync()
	}
	return count, err
}

func (f *nativeFile) Close() error {
	if f.writing {
		return errors.Join(f.File.Sync(), f.File.Close())
	}
	return f.File.Close()
}

func (h *rootedSFTP) Filecmd(request *sftp.Request) error {
	name, err := h.check(request.Filepath, true)
	if request.Method == "Symlink" {
		name, err = h.check(request.Target, true)
		if err != nil {
			return err
		}
		if err := validateLink(name, request.Filepath, false); err != nil {
			return err
		}
		return h.root.Symlink(request.Filepath, name)
	}
	if err != nil {
		return err
	}
	if name == "." {
		return os.ErrPermission
	}
	switch request.Method {
	case "Mkdir":
		mode := os.FileMode(0o700)
		if request.AttrFlags().Permissions {
			mode = request.Attributes().FileMode().Perm()
		}
		if err := h.root.Mkdir(name, mode); err != nil {
			return err
		}
		if h.options.MapOwner != nil {
			uid, gid, err := h.options.MapOwner(0, 0)
			if err != nil {
				return err
			}
			return h.root.Chown(name, uid, gid)
		}
		return nil
	case "Remove", "Rmdir":
		return h.root.Remove(name)
	case "Rename", "Link":
		target, err := h.check(request.Target, true)
		if err != nil {
			return err
		}
		if request.Method == "Link" {
			return h.root.Link(name, target)
		}
		if _, err := h.root.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(os.ErrExist, err)
		}
		return h.root.Rename(name, target)
	case "Setstat":
		return h.setAttributes(name, request)
	default:
		return ErrUnsupported
	}
}

func (h *rootedSFTP) PosixRename(request *sftp.Request) error {
	name, err := h.check(request.Filepath, true)
	if err != nil || name == "." {
		return errors.Join(os.ErrPermission, err)
	}
	target, err := h.check(request.Target, true)
	if err != nil || target == "." {
		return errors.Join(os.ErrPermission, err)
	}
	return h.root.Rename(name, target)
}

func (h *rootedSFTP) setAttributes(name string, request *sftp.Request) error {
	flags, attributes := request.AttrFlags(), request.Attributes()
	info, err := h.root.Stat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return ErrUnsupported
	}
	mode := os.O_RDONLY
	if flags.Size {
		if info.IsDir() || attributes.Size > uint64(MaxArchiveBytes) {
			return ErrLimit
		}
		mode = os.O_WRONLY
	}
	file, err := h.root.OpenFile(name, mode|nativeNonblockFlag(), 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if flags.Size {
		if err := file.Truncate(int64(attributes.Size)); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
	}
	if flags.Permissions {
		if attributes.FileMode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return ErrUnsupported
		}
		if err := file.Chmod(attributes.FileMode().Perm()); err != nil {
			return err
		}
	}
	if flags.UidGid {
		uid, gid := int(attributes.UID), int(attributes.GID)
		if h.options.MapOwner != nil {
			uid, gid, err = h.options.MapOwner(uid, gid)
			if err != nil {
				return err
			}
		}
		if err := file.Chown(uid, gid); err != nil {
			return err
		}
	}
	if flags.Acmodtime {
		return h.root.Chtimes(name, time.Unix(int64(attributes.Atime), 0), time.Unix(int64(attributes.Mtime), 0))
	}
	return nil
}

func (h *rootedSFTP) Filelist(request *sftp.Request) (sftp.ListerAt, error) {
	name, err := h.check(request.Filepath, false)
	if err != nil {
		return nil, err
	}
	if request.Method == "Stat" {
		info, err := h.root.Stat(name)
		if err != nil {
			return nil, err
		}
		return h.statList(info)
	}
	if request.Method != "List" {
		return nil, ErrUnsupported
	}
	file, err := h.root.Open(name)
	if err != nil {
		return nil, err
	}
	return &nativeDirectoryList{file: file, options: h.options}, nil
}

func (h *rootedSFTP) Lstat(request *sftp.Request) (sftp.ListerAt, error) {
	name, err := h.check(request.Filepath, false)
	if err != nil {
		return nil, err
	}
	info, err := h.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	return h.statList(info)
}

func (h *rootedSFTP) statList(info os.FileInfo) (sftp.ListerAt, error) {
	mapped, err := sftpFileInfo(info, h.options.Owner)
	if err != nil {
		return nil, err
	}
	return nativeInfoList{mapped}, nil
}

func (h *rootedSFTP) RealPath(value string) (string, error) {
	name, err := h.check(value, false)
	if err != nil {
		return "", err
	}
	if name == "." {
		return "/", nil
	}
	return "/" + name, nil
}

func (h *rootedSFTP) Readlink(value string) (string, error) {
	name, err := h.check(value, false)
	if err != nil {
		return "", err
	}
	target, err := h.root.Readlink(name)
	if err != nil {
		return "", err
	}
	if err := validateLink(name, target, false); err != nil {
		return "", err
	}
	if _, err := h.root.Stat(path.Clean(path.Join(path.Dir(name), target))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return target, nil
}

func (h *rootedSFTP) StatVFS(request *sftp.Request) (*sftp.StatVFS, error) {
	if _, err := h.check(request.Filepath, false); err != nil {
		return nil, err
	}
	return nativeFilesystemStat(h.directory)
}

type nativeInfoList []os.FileInfo

func (list nativeInfoList) ListAt(buffer []os.FileInfo, offset int64) (int, error) {
	if offset < 0 || offset >= int64(len(list)) {
		return 0, io.EOF
	}
	count := copy(buffer, list[offset:])
	if int(offset)+count >= len(list) {
		return count, io.EOF
	}
	return count, nil
}

type nativeDirectoryList struct {
	mu      sync.Mutex
	file    *os.File
	offset  int64
	options SFTPOptions
}

func (list *nativeDirectoryList) ListAt(buffer []os.FileInfo, offset int64) (int, error) {
	list.mu.Lock()
	defer list.mu.Unlock()
	if offset != list.offset {
		return 0, ErrConflict
	}
	items, err := list.file.Readdir(len(buffer))
	for index, item := range items {
		mapped, mapErr := sftpFileInfo(item, list.options.Owner)
		if mapErr != nil {
			return 0, mapErr
		}
		buffer[index] = mapped
	}
	list.offset += int64(len(items))
	return len(items), err
}

func (list *nativeDirectoryList) Close() error { return list.file.Close() }

type nativeInfo struct {
	os.FileInfo
	uid, gid uint32
}

func (info nativeInfo) Uid() uint32 { return info.uid }
func (info nativeInfo) Gid() uint32 { return info.gid }

func sftpFileInfo(info os.FileInfo, mapper func(int, int) (int, int, error)) (os.FileInfo, error) {
	uid, gid, _ := fileMetadata(info)
	if mapper != nil {
		var err error
		uid, gid, err = mapper(uid, gid)
		if err != nil {
			return nil, err
		}
	}
	return nativeInfo{FileInfo: info, uid: uint32(uid), gid: uint32(gid)}, nil
}
