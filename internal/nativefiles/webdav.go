package nativefiles

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/net/webdav"
)

type sftpWebDAV struct {
	client   *sftp.Client
	readOnly bool
	verify   func(context.Context) error
}

type webDAVVerifiedRequest struct{}

func (fs *sftpWebDAV) check(ctx context.Context, name string, write bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if ctx.Value(webDAVVerifiedRequest{}) != fs {
		if err := fs.verify(ctx); err != nil {
			return "", err
		}
	}
	if write && fs.readOnly {
		return "", os.ErrPermission
	}
	return NativePath(name)
}

func (fs *sftpWebDAV) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	name, err := fs.check(ctx, name, true)
	if err != nil {
		return err
	}
	if name == "/" {
		return os.ErrPermission
	}
	if err := fs.client.Mkdir(name); err != nil {
		return err
	}
	return fs.client.Chmod(name, perm.Perm())
}

func (fs *sftpWebDAV) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	writing := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	name, err := fs.check(ctx, name, writing)
	if err != nil {
		return nil, err
	}
	info, statErr := fs.client.Lstat(name)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}
	if statErr == nil {
		if info.IsDir() {
			if writing {
				return nil, os.ErrPermission
			}
			return &sftpDAVFile{ctx: ctx, fs: fs, name: name, info: info}, nil
		}
		if !info.Mode().IsRegular() {
			return nil, os.ErrPermission
		}
	}
	file, created, err := openNativeSFTPFile(fs.client, name, flag)
	if err != nil {
		return nil, err
	}
	if created {
		if err := file.Chmod(perm.Perm()); err != nil {
			return nil, errors.Join(err, file.Close())
		}
	}
	return &sftpDAVFile{ctx: ctx, fs: fs, name: name, file: file, writing: writing}, nil
}

func (fs *sftpWebDAV) RemoveAll(ctx context.Context, name string) error {
	name, err := fs.check(ctx, name, true)
	if err != nil {
		return err
	}
	if name == "/" {
		return os.ErrPermission
	}
	info, err := fs.client.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fs.client.Remove(name)
	}
	entries, err := fs.client.ReadDir(name)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := fs.RemoveAll(ctx, path.Join(name, entry.Name())); err != nil {
			return err
		}
	}
	return fs.client.RemoveDirectory(name)
}

func (fs *sftpWebDAV) Rename(ctx context.Context, oldName, newName string) error {
	oldName, err := fs.check(ctx, oldName, true)
	if err != nil {
		return err
	}
	newName, err = fs.check(ctx, newName, true)
	if err != nil {
		return err
	}
	if oldName == "/" || newName == "/" {
		return os.ErrPermission
	}
	return fs.client.PosixRename(oldName, newName)
}

func (fs *sftpWebDAV) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	name, err := fs.check(ctx, name, false)
	if err != nil {
		return nil, err
	}
	info, err := fs.client.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, os.ErrPermission
	}
	return info, nil
}

type sftpDAVFile struct {
	ctx     context.Context
	fs      *sftpWebDAV
	name    string
	file    *sftp.File
	info    os.FileInfo
	entries []os.FileInfo
	offset  int
	writing bool
}

func (f *sftpDAVFile) Close() error {
	if f.file != nil {
		return f.file.Close()
	}
	return nil
}

func (f *sftpDAVFile) Read(buffer []byte) (int, error) {
	if _, err := f.fs.check(f.ctx, f.name, false); err != nil {
		return 0, err
	}
	if f.file == nil {
		return 0, os.ErrInvalid
	}
	return f.file.Read(buffer)
}

func (f *sftpDAVFile) Write(buffer []byte) (int, error) {
	if _, err := f.fs.check(f.ctx, f.name, true); err != nil {
		return 0, err
	}
	if f.file == nil || !f.writing {
		return 0, os.ErrPermission
	}
	return f.file.Write(buffer)
}

func (f *sftpDAVFile) Seek(offset int64, whence int) (int64, error) {
	if _, err := f.fs.check(f.ctx, f.name, false); err != nil {
		return 0, err
	}
	if f.file == nil {
		return 0, os.ErrInvalid
	}
	return f.file.Seek(offset, whence)
}

func (f *sftpDAVFile) Stat() (os.FileInfo, error) {
	if _, err := f.fs.check(f.ctx, f.name, false); err != nil {
		return nil, err
	}
	if f.file == nil {
		return f.info, nil
	}
	return f.file.Stat()
}

func (f *sftpDAVFile) Readdir(count int) ([]os.FileInfo, error) {
	if _, err := f.fs.check(f.ctx, f.name, false); err != nil {
		return nil, err
	}
	if f.file != nil {
		return nil, os.ErrInvalid
	}
	if f.entries == nil {
		items, err := f.fs.client.ReadDir(f.name)
		if err != nil {
			return nil, err
		}
		f.entries = make([]os.FileInfo, 0, len(items))
		for _, item := range items {
			if item.IsDir() || item.Mode().IsRegular() {
				f.entries = append(f.entries, item)
			}
		}
	}
	end := len(f.entries)
	if count > 0 {
		end = min(end, f.offset+count)
		if end == f.offset {
			return nil, io.EOF
		}
	}
	items := f.entries[f.offset:end]
	f.offset = end
	return items, nil
}

func nativeWebDAVHandler(id, password string, filesystem *sftpWebDAV) http.Handler {
	handler := &webdav.Handler{
		Prefix: "/" + id, FileSystem: filesystem, LockSystem: webdav.NewMemLS(),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, supplied, ok := r.BasicAuth()
		if !ok || user != "porto" || subtle.ConstantTimeCompare([]byte(supplied), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Porto native files"`)
			http.Error(w, "Private native filesystem credentials are required.", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Origin") != "" || r.URL.Path != handler.Prefix && !strings.HasPrefix(r.URL.Path, handler.Prefix+"/") {
			http.Error(w, "This request is outside the selected native filesystem.", http.StatusForbidden)
			return
		}
		if _, err := filesystem.check(r.Context(), "/", false); err != nil {
			http.Error(w, "The native resource is disconnected or its identity changed.", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), webDAVVerifiedRequest{}, filesystem)))
	})
}

func webDAVMountPath(id string) string {
	return "/" + id[:32] + id[64:]
}
