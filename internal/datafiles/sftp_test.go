package datafiles

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/pkg/sftp"
)

func fixtureSFTP(t *testing.T, directory string, options SFTPOptions) *sftp.Client {
	t.Helper()
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeSFTP(ctx, server, directory, options) }()
	connection, err := sftp.NewClientPipe(client, client)
	if err != nil {
		cancel()
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close()
		_ = client.Close()
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("close synthetic SFTP session: %v", err)
		}
	})
	return connection
}

func TestRootedSFTPEditsAreDirectAndSupportRandomWrites(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "fixture.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := fixtureSFTP(t, directory, SFTPOptions{})
	file, err := client.OpenFile("/fixture.txt", os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("DATA"), 2); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "fixture.txt"))
	if err != nil || string(content) != "orDATAal" {
		t.Fatalf("write was cached or copied back: %q %v", content, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRootedSFTPRejectsReadonlyAndEscapingSymlinks(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "fixture.txt"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := fixtureSFTP(t, directory, SFTPOptions{ReadOnly: true})
	if file, err := client.OpenFile("/fixture.txt", os.O_WRONLY|os.O_TRUNC); err == nil {
		file.Close()
		t.Fatal("immutable native files accepted writes")
	}
	if err := client.Remove("/fixture.txt"); err == nil {
		t.Fatal("immutable native files accepted deletion")
	}
	if runtime.GOOS == "windows" {
		return
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("must stay outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "escape")); err != nil {
		t.Fatal(err)
	}
	if file, err := client.Open("/escape"); err == nil {
		file.Close()
		t.Fatal("native read escaped its resource")
	}
	if _, err := client.ReadLink("/escape"); err == nil {
		t.Fatal("escaping native symlink target was exposed")
	}
}
