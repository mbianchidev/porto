package credentials

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/zalando/go-keyring"
)

const (
	maxCredentialBytes  = 256 << 10
	credentialPartChars = 768
)

// Keep every record below Windows Credential Manager's UTF-16 blob limit.
// The small root record commits a generation only after all of its parts exist.
type Chunked struct {
	backend Store
	mu      sync.Mutex
}
type credentialRecord struct {
	Version    int    `json:"version"`
	Hash       string `json:"hash"`
	Generation string `json:"generation"`
	Parts      int    `json:"parts"`
}

func NewChunked(backend Store) *Chunked {
	if existing, ok := backend.(*Chunked); ok {
		return existing
	}
	if backend == nil {
		backend = System{}
	}
	return &Chunked{backend: backend}
}
func partAccount(account, generation string, index int) string {
	return account + "/v1/" + generation + "/" + strconv.Itoa(index)
}
func (s *Chunked) record(service, account string) (credentialRecord, error) {
	var record credentialRecord
	value, err := s.backend.Get(service, account)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal([]byte(value), &record); err != nil {
		return record, errors.New("OS credential generation metadata is invalid")
	}
	if record.Version != 1 || record.Parts < 1 || record.Parts > 512 || len(record.Hash) != 64 || len(record.Generation) != 32 {
		return record, errors.New("OS credential generation is unsupported or incomplete")
	}
	if _, err := hex.DecodeString(record.Hash); err != nil {
		return record, errors.New("OS credential generation digest is invalid")
	}
	if _, err := hex.DecodeString(record.Generation); err != nil {
		return record, errors.New("OS credential generation ID is invalid")
	}
	return record, nil
}
func (s *Chunked) removeParts(service, account string, record credentialRecord) error {
	var errs []error
	for index := 0; index < record.Parts; index++ {
		err := s.backend.Delete(service, partAccount(account, record.Generation, index))
		if err != nil && !errors.Is(err, keyring.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
func (s *Chunked) Set(service, account, secret string) error {
	if len(secret) == 0 || len(secret) > maxCredentialBytes {
		return errors.New("OS credential payload must be 1 byte to 256 KiB")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.record(service, account)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	sum := sha256.Sum256([]byte(secret))
	generation := make([]byte, 16)
	if _, err := rand.Read(generation); err != nil {
		return err
	}
	record := credentialRecord{Version: 1, Hash: hex.EncodeToString(sum[:]), Generation: hex.EncodeToString(generation)}
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	record.Parts = (len(encoded) + credentialPartChars - 1) / credentialPartChars
	for index := 0; index < record.Parts; index++ {
		part := encoded[index*credentialPartChars : min((index+1)*credentialPartChars, len(encoded))]
		if err := s.backend.Set(service, partAccount(account, record.Generation, index), part); err != nil {
			return errors.Join(err, s.removeParts(service, account, record))
		}
	}
	metadata, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := s.backend.Set(service, account, string(metadata)); err != nil {
		return errors.Join(err, s.removeParts(service, account, record))
	}
	if old.Generation != "" {
		if err := s.removeParts(service, account, old); err != nil {
			return fmt.Errorf("new OS credential committed but old generation cleanup failed: %w", err)
		}
	}
	return nil
}
func (s *Chunked) Get(service, account string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.record(service, account)
	if err != nil {
		return "", err
	}
	encoded := make([]byte, 0, record.Parts*credentialPartChars)
	for index := 0; index < record.Parts; index++ {
		part, err := s.backend.Get(service, partAccount(account, record.Generation, index))
		if err != nil {
			return "", fmt.Errorf("read OS credential generation part: %w", err)
		}
		if len(part) > credentialPartChars {
			return "", errors.New("OS credential generation part exceeds its limit")
		}
		encoded = append(encoded, part...)
	}
	data, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil || len(data) > maxCredentialBytes {
		return "", errors.New("OS credential generation is invalid or oversized")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != record.Hash {
		return "", errors.New("OS credential generation integrity check failed")
	}
	return string(data), nil
}
func (s *Chunked) Delete(service, account string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.record(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.removeParts(service, account, record); err != nil {
		return err
	}
	err = s.backend.Delete(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
