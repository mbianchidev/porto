package docker

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/networking"
)

func TestGuestNetworkArchiveKeepsSecretsInPrivateInputFiles(t *testing.T) {
	data, err := guestNetworkArchive(networking.GuestConfig{
		Generation:  strings.Repeat("a", 64),
		Environment: []string{"HTTP_PROXY=http://synthetic:mock-password@proxy.example:3128", "NO_PROXY=localhost"},
	}, "synthetic-owner")
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(data))
	found := false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Mode != 0o600 {
			t.Fatalf("network configuration %s has mode %o", header.Name, header.Mode)
		}
		value, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "proxy.env" {
			found = strings.Contains(string(value), "mock-password")
		}
	}
	if !found {
		t.Fatal("guest configuration did not carry the proxy credential over private stdin")
	}
}
