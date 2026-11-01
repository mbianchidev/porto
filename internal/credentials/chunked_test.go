package credentials

import (
	"errors"
	"strings"
	"testing"
)

type boundedStore struct {
	memoryStore
	writes int
	failAt int
}

func (s *boundedStore) Set(service, account, value string) error {
	s.writes++
	if len(value) > credentialPartChars {
		return errors.New("synthetic native credential blob limit exceeded")
	}
	if s.failAt > 0 && s.writes == s.failAt {
		return errors.New("synthetic interrupted credential write")
	}
	return s.memoryStore.Set(service, account, value)
}
func TestChunkedOSCredentialsBoundNativeRecordsAndCommitAtomically(t *testing.T) {
	backend := &boundedStore{memoryStore: memoryStore{values: map[string]string{}}}
	store := NewChunked(backend)
	original := strings.Repeat("synthetic-identity-\u4e2d", 500)
	if err := store.Set("synthetic-service", "identity", original); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Get("synthetic-service", "identity"); err != nil || value != original {
		t.Fatalf("native-sized credential round trip: %v", err)
	}
	backend.failAt = backend.writes + 2
	if err := store.Set("synthetic-service", "identity", strings.Repeat("synthetic-new-credential", 100)); err == nil {
		t.Fatal("interrupted credential generation committed")
	}
	if value, err := store.Get("synthetic-service", "identity"); err != nil || value != original {
		t.Fatalf("old committed identity lost: %v", err)
	}
	backend.failAt = 0
	if err := store.Set("synthetic-service", "identity", "synthetic-rotated"); err != nil {
		t.Fatal(err)
	}
	if len(backend.values) != 2 {
		t.Fatalf("old native credential generation retained: %d records", len(backend.values))
	}
	if err := store.Delete("synthetic-service", "identity"); err != nil {
		t.Fatal(err)
	}
	if len(backend.values) != 0 {
		t.Fatal("credential uninstall retained native records")
	}
}
func TestChunkedOSCredentialsRejectMissingOrAlteredParts(t *testing.T) {
	backend := &boundedStore{memoryStore: memoryStore{values: map[string]string{}}}
	store := NewChunked(backend)
	if err := store.Set("synthetic", "identity", strings.Repeat("synthetic", 300)); err != nil {
		t.Fatal(err)
	}
	for key := range backend.values {
		if strings.Contains(key, "/v1/") {
			backend.values[key] = "altered"
			break
		}
	}
	if _, err := store.Get("synthetic", "identity"); err == nil {
		t.Fatal("altered credential parts accepted")
	}
}
