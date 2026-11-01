package extensions

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func ReadJSON(path string, out any, limit int64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("selected JSON file exceeds its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("decode selected JSON file: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("exactly one JSON document is required")
	}
	return nil
}

func Pack(directory string, privateKey []byte) (Package, error) {
	var manifest Manifest
	if err := ReadJSON(filepath.Join(directory, "manifest.json"), &manifest, 64<<10); err != nil {
		return Package{}, err
	}
	if err := validateManifest(manifest); err != nil {
		return Package{}, err
	}
	ui, err := os.ReadFile(filepath.Join(directory, "view.html"))
	if err != nil {
		return Package{}, err
	}
	module, err := os.ReadFile(filepath.Join(directory, "action.wasm"))
	if err != nil {
		return Package{}, err
	}
	if len(ui) > 512<<10 || len(module) > 8<<20 || len(module) < 8 || !bytes.Equal(module[:4], []byte{0, 97, 115, 109}) {
		return Package{}, failure(400, "extension_package", "Supply view.html up to 512 KiB and a valid action.wasm up to 8 MiB.")
	}
	p := Package{Content: Content{Manifest: manifest, UI: string(ui), Module: module}}
	p.SHA256 = Digest(p.Content)
	if len(privateKey) > 0 {
		if len(privateKey) != ed25519.PrivateKeySize {
			return Package{}, errors.New("signing key must contain a decoded Ed25519 private key (64 bytes)")
		}
		p.PublicKey = base64.StdEncoding.EncodeToString(ed25519.PrivateKey(privateKey).Public().(ed25519.PublicKey))
		p.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(p.SHA256)))
	}
	return p, nil
}
