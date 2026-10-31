package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/zalando/go-keyring"
)

var sealedName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type EncryptedFiles struct {
	root, service string
	store         Store
	mu            sync.Mutex
	key           []byte
	destroyed     bool
}

func NewEncryptedFiles(root, service string, store Store) *EncryptedFiles {
	if store == nil {
		store = System{}
	}
	return &EncryptedFiles{root: root, service: service, store: store}
}

func (s *EncryptedFiles) cipher() (cipher.AEAD, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.destroyed {
		return nil, errors.New("the owned credential store was removed")
	}
	if s.key == nil {
		sum := sha256.Sum256([]byte(s.root))
		account := "state-key-" + hex.EncodeToString(sum[:12])
		encoded, err := s.store.Get(s.service, account)
		if errors.Is(err, keyring.ErrNotFound) {
			entries, readErr := os.ReadDir(s.root)
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				return nil, readErr
			}
			if len(entries) != 0 {
				return nil, errors.New("delivery encryption key is missing from the system credential store; recover it before opening existing definitions")
			}
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			encoded = base64.StdEncoding.EncodeToString(key)
			if err := s.store.Set(s.service, account, encoded); err != nil {
				return nil, fmt.Errorf("save local delivery key in system credential store: %w", err)
			}
		} else if err != nil {
			return nil, fmt.Errorf("unlock system credential store for delivery definitions: %w", err)
		}
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(key) != 32 {
			return nil, errors.New("stored local delivery key is invalid; recover the credential store instead of overwriting it")
		}
		s.key = key
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *EncryptedFiles) Set(name string, value []byte) error {
	if !sealedName.MatchString(name) || len(value) > 4*1024*1024 {
		return errors.New("invalid or oversized encrypted local definition")
	}
	aead, err := s.cipher()
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := aead.Seal(nonce, nonce, value, []byte(name))
	return config.WriteFileAtomic(filepath.Join(s.root, name+".sealed"), sealed, 0o600)
}

func (s *EncryptedFiles) Get(name string) ([]byte, error) {
	if !sealedName.MatchString(name) {
		return nil, errors.New("invalid encrypted definition identity")
	}
	data, err := os.ReadFile(filepath.Join(s.root, name+".sealed"))
	if err != nil {
		return nil, err
	}
	if len(data) > 4*1024*1024+64 {
		return nil, errors.New("encrypted definition exceeds its limit")
	}
	aead, err := s.cipher()
	if err != nil {
		return nil, err
	}
	if len(data) < aead.NonceSize() {
		return nil, errors.New("encrypted local definition is truncated")
	}
	value, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(name))
	if err != nil {
		return nil, errors.New("encrypted local definition failed authentication")
	}
	return value, nil
}

func (s *EncryptedFiles) Delete(name string) error {
	if !sealedName.MatchString(name) {
		return errors.New("invalid encrypted definition identity")
	}
	err := os.Remove(filepath.Join(s.root, name+".sealed"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Destroy is for an explicitly removed owner, after its operations have stopped.
func (s *EncryptedFiles) Destroy() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.destroyed = true
	for i := range s.key {
		s.key[i] = 0
	}
	s.key = nil
	entries, err := os.ReadDir(s.root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".sealed")
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sealed") || !sealedName.MatchString(name) {
			return errors.New("unexpected entry in owned encrypted state; cleanup refused")
		}
		if err := os.Remove(filepath.Join(s.root, entry.Name())); err != nil {
			return err
		}
	}
	sum := sha256.Sum256([]byte(s.root))
	err = s.store.Delete(s.service, "state-key-"+hex.EncodeToString(sum[:12]))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
