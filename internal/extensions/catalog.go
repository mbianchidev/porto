package extensions

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
)

func (m *Manager) Trust(publicKey, label string) error {
	key, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize || strings.TrimSpace(label) == "" || len(label) > 100 {
		return failure(400, "extension_publisher", "Provide an Ed25519 public key and publisher label up to 100 characters, verified out of band.")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previous, exists := m.state.Publishers[publicKey]
	if !exists && len(m.state.Publishers) >= 64 {
		return failure(429, "extension_publishers", "Publisher trust limit reached.")
	}
	m.state.Publishers[publicKey] = label
	if err := m.saveLocked(); err != nil {
		if exists {
			m.state.Publishers[publicKey] = previous
		} else {
			delete(m.state.Publishers, publicKey)
		}
		return err
	}
	return nil
}

func (m *Manager) RevokePublisher(publicKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.state.Publishers, publicKey)
	for id, value := range m.state.Installed {
		if value.Publisher == publicKey {
			value.Enabled, value.Approved, value.State, value.LastError = false, false, "disabled", "Publisher trust revoked. Verify a replacement explicitly."
			value.Generation++
			m.state.Installed[id] = value
			m.cancelLocked(id)
		}
	}
	return m.saveLocked()
}

func (m *Manager) Publishers() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	values := map[string]string{}
	for key, label := range m.state.Publishers {
		values[key] = label
	}
	return values
}

func CatalogPayload(catalog Catalog) []byte {
	data, _ := json.Marshal(struct {
		Version int            `json:"version"`
		Entries []CatalogEntry `json:"entries"`
	}{catalog.Version, catalog.Entries})
	return data
}
func (m *Manager) AddCatalog(catalog Catalog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if catalog.Version != 1 || len(catalog.Entries) > 128 {
		return failure(409, "extension_catalog", "Catalog format version 1 and at most 128 entries are supported.")
	}
	if m.state.Publishers[catalog.PublicKey] == "" {
		return failure(403, "extension_catalog_publisher", "Explicitly trust the verified catalog publisher before importing its signed index.")
	}
	if err := verifySignature(catalog.PublicKey, catalog.Signature, CatalogPayload(catalog)); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, entry := range catalog.Entries {
		if !extensionID.MatchString(entry.ID) || !digestPattern.MatchString(entry.SHA256) || entry.Publisher != catalog.PublicKey || seen[entry.ID] {
			return failure(400, "extension_catalog_entry", "Catalog entries require unique extension IDs, pinned digests and the same trusted publisher.")
		}
		if _, err := httpsURL(entry.URL); err != nil {
			return err
		}
		seen[entry.ID] = true
	}
	previous := m.state.Catalog
	values := []CatalogEntry{}
	for _, entry := range previous {
		if entry.Publisher != catalog.PublicKey {
			values = append(values, entry)
		}
	}
	values = append(values, catalog.Entries...)
	if len(values) > 256 {
		return failure(429, "extension_catalog_limit", "At most 256 catalog entries can be retained.")
	}
	m.state.Catalog = values
	if err := m.saveLocked(); err != nil {
		m.state.Catalog = previous
		return err
	}
	return nil
}
func (m *Manager) Catalog() []CatalogEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	values := []CatalogEntry{}
	for _, entry := range m.state.Catalog {
		if m.state.Publishers[entry.Publisher] != "" {
			values = append(values, entry)
		}
	}
	slices.SortFunc(values, func(a, b CatalogEntry) int { return strings.Compare(a.ID+a.Version, b.ID+b.Version) })
	return values
}
func (m *Manager) InstallCatalog(ctx context.Context, id, sha string) (Installed, error) {
	var selected *CatalogEntry
	for _, entry := range m.Catalog() {
		if entry.ID == id && entry.SHA256 == sha {
			copy := entry
			selected = &copy
			break
		}
	}
	if selected == nil {
		return Installed{}, failure(404, "extension_catalog_entry", "Select an exact extension identity and digest from a currently trusted catalog.")
	}
	data, _, err := fetchHTTPS(ctx, selected.URL, MaxPackageBytes)
	if err != nil {
		return Installed{}, err
	}
	var p Package
	if err := json.Unmarshal(data, &p); err != nil {
		return Installed{}, failure(400, "extension_catalog_artifact", "Catalog artifact is not a valid extension package.")
	}
	if p.PublicKey != selected.Publisher || p.Content.Manifest.ID != selected.ID || p.Content.Manifest.Version != selected.Version {
		return Installed{}, failure(403, "extension_catalog_provenance", "Downloaded artifact does not match the catalog identity, version and publisher.")
	}
	return m.Install(InstallRequest{Package: p, ExpectedSHA256: selected.SHA256})
}
