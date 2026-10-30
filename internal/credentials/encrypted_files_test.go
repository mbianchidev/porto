package credentials

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"
)

type memoryStore struct {
	mu     sync.Mutex
	values map[string]string
}

func (s *memoryStore) Set(service, account, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[service+"/"+account] = secret
	return nil
}
func (s *memoryStore) Get(service, account string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[service+"/"+account]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}
func (s *memoryStore) Delete(service, account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, service+"/"+account)
	return nil
}

func TestEncryptedDefinitionsBindIdentityAndNeverFallBackToPlaintext(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sealed")
	store := &memoryStore{values: map[string]string{}}
	files := NewEncryptedFiles(root, "synthetic", store)
	secret := bytes.Repeat([]byte("synthetic-secret"), 1024)
	if err := files.Set("revision-one", secret); err != nil {
		t.Fatal(err)
	}
	encrypted, err := os.ReadFile(filepath.Join(root, "revision-one.sealed"))
	if err != nil || bytes.Contains(encrypted, []byte("synthetic-secret")) {
		t.Fatal("definition was stored unencrypted")
	}
	value, err := NewEncryptedFiles(root, "synthetic", store).Get("revision-one")
	if err != nil || !bytes.Equal(value, secret) {
		t.Fatalf("large encrypted definition did not round-trip: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "revision-two.sealed"), encrypted, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Get("revision-two"); err == nil {
		t.Fatal("ciphertext could be substituted for another revision")
	}
	for account := range store.values {
		delete(store.values, account)
	}
	if _, err := NewEncryptedFiles(root, "synthetic", store).Get("revision-one"); err == nil {
		t.Fatal("missing key was silently replaced")
	}
	if err := files.Set("../escape", secret); err == nil {
		t.Fatal("encrypted file identity escaped its root")
	}
	if err := files.Delete("missing"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
