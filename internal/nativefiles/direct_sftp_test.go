package nativefiles

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/pkg/sftp"
)

func nativeFixtureClient(t *testing.T, directory string, readOnly bool, allowedCloseErrors ...error) *sftp.Client {
	t.Helper()
	server, socket := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- datafiles.ServeSFTP(ctx, server, directory, datafiles.SFTPOptions{ReadOnly: readOnly}) }()
	client, err := sftp.NewClientPipe(socket, socket)
	if err != nil {
		cancel()
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		socket.Close()
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			for _, allowed := range allowedCloseErrors {
				if errors.Is(err, allowed) {
					return
				}
			}
			t.Errorf("close synthetic native SFTP server: %v", err)
		}
	})
	return client
}

func TestNativeDirectEditorWriteRenameAndConcurrentGuestChanges(t *testing.T) {
	directory := t.TempDir()
	client := nativeFixtureClient(t, directory, false)
	filesystem := NewDirectSFTP(client, false)
	name := "/fixture-\u03b4.txt"
	handle, err := filesystem.Open(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.Write(handle, []byte("synthetic file"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.Write(handle, []byte("DATA"), 2); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, name[1:]))
	if err != nil || string(content) != "syDATAtic file" {
		t.Fatalf("random editor write not visible before close: %q %v", content, err)
	}
	if err := filesystem.Release(handle); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.Rename(name, "/renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "renamed.txt"), []byte("guest changed contents"), 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := filesystem.Stat("/renamed.txt", 0)
	if err != nil || info.Size() != int64(len("guest changed contents")) {
		t.Fatalf("guest change was hidden by host caching: %v %v", info, err)
	}
}

func TestNativeDirectReadonlyPathValidationAndClosedHandles(t *testing.T) {
	directory := t.TempDir()
	client := nativeFixtureClient(t, directory, true)
	filesystem := NewDirectSFTP(client, true)
	if _, err := filesystem.Open("/new.txt", os.O_CREATE|os.O_WRONLY, 0o600); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("immutable filesystem accepted native edit: %v", err)
	}
	for _, name := range []string{"/../outside", "/nested/../../outside", `C:\outside`, "/bad\x00path"} {
		if _, err := NativePath(name); err == nil {
			t.Fatalf("invalid native path accepted: %q", name)
		}
	}
	if _, err := filesystem.Read(99, make([]byte, 1), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("missing file handle did not fail clearly: %v", err)
	}
}

func TestNativeDirectLongUnicodePathsPreservePermissionsAndConfinedLinks(t *testing.T) {
	directory := t.TempDir()
	relative := filepath.Join("synthetic-\u03b4", strings.Repeat("long-name-", 18), "nested", "file-\u03b4.txt")
	if err := os.MkdirAll(filepath.Join(directory, filepath.Dir(relative)), 0o700); err != nil {
		t.Fatal(err)
	}
	client := nativeFixtureClient(t, directory, false)
	filesystem := NewDirectSFTP(client, false)
	handle, err := filesystem.Open("/"+filepath.ToSlash(relative), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.Write(handle, []byte("synthetic direct content"), 0); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.Release(handle); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(directory, relative))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("native creation changed Unix permissions: %v", info.Mode())
	}
	if err := filesystem.Link("/"+filepath.ToSlash(relative), "/hardlink.txt", false); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(directory, "hardlink.txt")); err != nil || string(content) != "synthetic direct content" {
		t.Fatalf("native hardlink did not reach the original data: %q, %v", content, err)
	}
	t.Run("symlink containment", func(t *testing.T) {
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("synthetic outside boundary"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(directory, "escape")); err != nil {
			if runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission) {
				t.Skip("host policy does not permit synthetic symlinks")
			}
			t.Fatal(err)
		}
		if _, err := filesystem.Open("/escape/outside.txt", os.O_RDONLY, 0); err == nil {
			t.Fatal("a symlink exposed files outside the selected resource")
		}
		if err := filesystem.Link("../outside", "/unsafe-link", true); err == nil {
			t.Fatal("native symlink creation accepted an escaping target")
		}
	})
}

func TestNativeDirectConnectionLossReturnsAnErrorForOpenHandles(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "synthetic.txt"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := nativeFixtureClient(t, directory, false, io.ErrUnexpectedEOF)
	filesystem := NewDirectSFTP(client, false)
	handle, err := filesystem.Open("/synthetic.txt", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := filesystem.Read(handle, make([]byte, 16), 0); err == nil {
		t.Fatal("connection loss returned success-shaped native file contents")
	}
	if _, err := filesystem.Write(handle, []byte("forbidden"), 0); err == nil {
		t.Fatal("connection loss reported an uncommitted write as successful")
	}
}
