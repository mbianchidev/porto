package wsl

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

type mockVault map[string]string

func (v mockVault) Set(_, account, secret string) error { v[account] = secret; return nil }
func (v mockVault) Get(_, account string) (string, error) {
	if value, found := v[account]; found {
		return value, nil
	}
	return "", keyring.ErrNotFound
}
func (v mockVault) Delete(_, account string) error { delete(v, account); return nil }

func TestWSLCredentialProtocolUsesNativeVaultNotLinuxFiles(t *testing.T) {
	root := t.TempDir()
	vault := NewCredentialVault(root)
	vault.secrets = mockVault{}
	if err := vault.Handle("store", strings.NewReader(`{"ServerURL":"registry.example","Username":"synthetic","Secret":"mock-password"}`), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(root, "wsl-credential-index.json"))
	if err != nil || strings.Contains(string(index), "mock-password") {
		t.Fatalf("credential index = %s, %v", index, err)
	}
	var output bytes.Buffer
	if err := vault.Handle("get", strings.NewReader("registry.example\n"), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"Secret":"mock-password"`) {
		t.Fatalf("credential protocol response = %s", output.String())
	}
	if err := vault.Handle("erase", strings.NewReader("registry.example\n"), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := vault.Handle("get", strings.NewReader("registry.example\n"), &bytes.Buffer{}); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("missing credentials = %v", err)
	}
}
