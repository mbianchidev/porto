//go:build !windows

package certificates

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCertificateFilePermissions(t *testing.T) {
	dir := t.TempDir()
	status, err := New(filepath.Join(dir, "porto.local.pem"), filepath.Join(dir, "porto.local-key.pem")).Ensure()
	if err != nil {
		t.Fatalf("ensure certificate: %v", err)
	}
	certificateInfo, err := os.Stat(status.CertificatePath)
	if err != nil {
		t.Fatalf("stat certificate: %v", err)
	}
	keyInfo, err := os.Stat(status.KeyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if got := certificateInfo.Mode().Perm(); got != 0o644 {
		t.Fatalf("certificate mode = %o, want 644", got)
	}
	if got := keyInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("key mode = %o, want 600", got)
	}
}
