package runtimefiles

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/pkg/sftp"
)

func TestNativeSFTPRequestUsesRootedDirectIO(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "fixture.txt"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	resource := datafiles.Resource{Kind: "volume", Name: "fixture", ID: "fixture-native"}
	envelope := Envelope{
		Descriptor: Descriptor{Resource: resource, RootPath: directory},
		Request:    datafiles.Request{Action: "sftp", Identity: resource.Fingerprint(), Writable: true},
	}
	server, socket := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ExecuteDirectory(ctx, envelope.Descriptor, envelope.Request, server, server) }()
	client, err := sftp.NewClientPipe(socket, socket)
	if err != nil {
		t.Fatal(err)
	}
	file, err := client.OpenFile("/fixture.txt", os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("LIVE"), 1); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(directory, "fixture.txt"))
	if err != nil || string(content) != "sLIVEetic" {
		t.Fatalf("native root request did not expose direct I/O: %q %v", content, err)
	}
	file.Close()
	client.Close()
	socket.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
