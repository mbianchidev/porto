package nativefiles

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/pkg/sftp"
)

func nativeFixtureClient(t *testing.T, directory string, readOnly bool) *sftp.Client {
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
