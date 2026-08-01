//go:build !windows

package certificates

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCertificateFilePermissions(t *testing.T) {
	dir := t.TempDir()
	status, err := testManager(dir).Ensure()
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
	authorityKeyInfo, err := os.Stat(filepath.Join(dir, "porto-root-ca-key.pem"))
	if err != nil {
		t.Fatalf("stat certificate authority key: %v", err)
	}
	if got := authorityKeyInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("certificate authority key mode = %o, want 600", got)
	}
}
