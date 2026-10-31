package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/extensions"
)

func TestCatalogAuthoringSignsCanonicalIdentityAndRejectsBrokenKeys(t *testing.T) {
	root := t.TempDir()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "synthetic.private")
	if err := writeNewPrivateFile(keyPath, []byte(base64.StdEncoding.EncodeToString(private))); err != nil {
		t.Fatal(err)
	}
	unsigned := extensions.Catalog{Version: 1, Entries: []extensions.CatalogEntry{{ID: "dev.synthetic.catalog", Name: "Synthetic catalog package", Version: "1.0.0", Description: "Synthetic only", URL: "https://synthetic.example.invalid/package", SHA256: strings.Repeat("a", 64)}}}
	data, err := json.Marshal(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	index, output := filepath.Join(root, "unsigned.json"), filepath.Join(root, "signed.json")
	if err := os.WriteFile(index, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := signExtensionCatalog([]string{"--index", index, "--signing-key", keyPath, "--out", output}); err != nil {
		t.Fatal(err)
	}
	var catalog extensions.Catalog
	if err := extensions.ReadJSON(output, &catalog, 1<<20); err != nil {
		t.Fatal(err)
	}
	signature, err := base64.StdEncoding.DecodeString(catalog.Signature)
	if err != nil || !ed25519.Verify(public, extensions.CatalogPayload(catalog), signature) {
		t.Fatalf("authoring signature is not consumable: %v", err)
	}
	if catalog.Entries[0].Publisher != catalog.PublicKey {
		t.Fatal("catalog entries did not bind the selected signing identity")
	}
	if err := signExtensionCatalog([]string{"--index", index, "--signing-key", keyPath, "--out", output}); err == nil {
		t.Fatal("signing overwrote an existing artifact without consent")
	}
	if err := os.WriteFile(keyPath, []byte("synthetic-invalid-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readExtensionSigningKey(keyPath); err == nil {
		t.Fatal("invalid signing key accepted")
	}
}
