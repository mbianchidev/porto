package imageanalysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/localtools"
	"github.com/mbianchidev/porto/internal/networking"
)

const databaseListing = "https://grype.anchore.io/databases/v6/latest.json"

func (m *Manager) DatabaseStatus(ctx context.Context) (Database, error) {
	m.databaseMu.RLock()
	defer m.databaseMu.RUnlock()
	return m.databaseStatus(ctx)
}

func (m *Manager) databaseStatus(ctx context.Context) (Database, error) {
	status := Database{State: "unavailable", Location: filepath.Join(m.root, "database"), MaxAgeHours: int(DefaultMaxAge.Hours()), Offline: true}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return status, err
	}
	work, err := os.MkdirTemp(m.root, ".db-status-")
	if err != nil {
		return status, err
	}
	defer os.RemoveAll(work)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, commandErr := m.tool(ctx, work, "grype", "db", "status", "-o", "json")
	var document struct {
		SchemaVersion, Built, Path, Error string
		Valid                             bool
	}
	if err := json.Unmarshal(output.Stdout, &document); err != nil {
		status.Message = "Grype database status is unavailable."
		if commandErr != nil {
			status.Message = commandErr.Error()
		}
		return status, errors.New(status.Message)
	}
	status.SchemaVersion, status.Built = document.SchemaVersion, document.Built
	if !document.Valid || commandErr != nil {
		status.State, status.Message = "missing", networking.Redact(document.Error)
		if status.Message == "" {
			status.Message = "Import an offline database or explicitly download one before scanning."
		}
		return status, nil
	}
	built, err := time.Parse(time.RFC3339, document.Built)
	if err != nil || built.After(m.now().Add(5*time.Minute)) || !strings.HasPrefix(document.SchemaVersion, "v6.") {
		status.Message = "Grype database has unsupported schema or invalid build time."
		return status, nil
	}
	path, err := filepath.Abs(document.Path)
	if err != nil {
		return status, err
	}
	root, err := filepath.Abs(status.Location)
	if err != nil {
		return status, err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return status, errors.New("Grype database is outside the configured local database directory")
	}
	file, err := os.Open(path)
	if err != nil {
		return status, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return status, errors.New("Grype database is not a readable regular file")
	}
	hasher := sha256.New()
	if _, err := localtools.CopyBounded(hasher, file, 4*1024*1024*1024); err != nil {
		return status, err
	}
	status.Bytes, status.Checksum, status.State = info.Size(), "sha256:"+hex.EncodeToString(hasher.Sum(nil)), "ready"
	if m.now().Sub(built) > DefaultMaxAge {
		status.State, status.Message = "stale", "Database is older than 120 hours; matching requires explicit allow-stale consent and never produces a clean exit code."
	}
	data, err := json.Marshal(status)
	if err != nil {
		return status, err
	}
	if err := config.WriteFileAtomic(filepath.Join(m.root, "current-database.json"), data, 0o600); err != nil {
		return status, err
	}
	return status, nil
}

func (m *Manager) ImportDatabase(ctx context.Context, source io.Reader) (Database, error) {
	if !m.databaseUpdateMu.TryLock() {
		return Database{}, ErrBusy
	}
	defer m.databaseUpdateMu.Unlock()
	return m.importDatabase(ctx, source)
}

func (m *Manager) importDatabase(ctx context.Context, source io.Reader) (Database, error) {
	m.databaseMu.Lock()
	defer m.databaseMu.Unlock()
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return Database{}, err
	}
	work, err := os.MkdirTemp(m.root, ".db-import-")
	if err != nil {
		return Database{}, err
	}
	defer os.RemoveAll(work)
	archive, err := os.OpenFile(filepath.Join(work, "database.tar.zst"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Database{}, err
	}
	_, copyErr := localtools.CopyBounded(archive, source, MaxDatabaseBytes)
	if err := errors.Join(copyErr, archive.Close()); err != nil {
		return Database{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := m.tool(ctx, work, "grype", "db", "import", archive.Name()); err != nil {
		return Database{}, err
	}
	status, err := m.databaseStatus(ctx)
	if err == nil && status.State != "ready" && status.State != "stale" {
		err = fmt.Errorf("imported database is %s: %s", status.State, status.Message)
	}
	return status, err
}

func (m *Manager) UpdateDatabase(ctx context.Context, maxBytes int64) (Database, error) {
	if !m.databaseUpdateMu.TryLock() {
		return Database{}, ErrBusy
	}
	defer m.databaseUpdateMu.Unlock()
	if maxBytes == 0 {
		maxBytes = MaxDatabaseBytes
	}
	if maxBytes < 1024*1024 || maxBytes > MaxDatabaseBytes {
		return Database{}, fmt.Errorf("%w: download limit must be 1-512 MiB", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	policy, err := networking.Default()
	if err != nil {
		return Database{}, err
	}
	transport, err := policy.Transport()
	if err != nil {
		return Database{}, err
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Minute, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 || request.URL.Scheme != "https" || request.URL.Hostname() != "grype.anchore.io" || request.URL.User != nil {
			return errors.New("database redirect must remain on the verified HTTPS Grype database host")
		}
		return nil
	}}
	response, err := databaseGET(ctx, client, databaseListing)
	if err != nil {
		return Database{}, err
	}
	listing, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	closeErr := response.Body.Close()
	if err != nil || closeErr != nil || len(listing) > 1024*1024 {
		return Database{}, errors.New("unable to read bounded Grype database listing")
	}
	var document struct{ Status, SchemaVersion, Built, Path, Checksum string }
	if json.Unmarshal(listing, &document) != nil || document.Status != "active" || !strings.HasPrefix(document.SchemaVersion, "v6.") || !strings.HasPrefix(document.Checksum, "sha256:") || len(document.Checksum) != 71 {
		return Database{}, errors.New("Grype database listing has unsupported or missing integrity metadata")
	}
	archiveURL, err := databaseArchiveURL(document.Path)
	if err != nil {
		return Database{}, err
	}
	response, err = databaseGET(ctx, client, archiveURL)
	if err != nil {
		return Database{}, err
	}
	defer response.Body.Close()
	if response.ContentLength > maxBytes {
		return Database{}, fmt.Errorf("database download is %d bytes, above the configured %d-byte limit", response.ContentLength, maxBytes)
	}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return Database{}, err
	}
	archive, err := os.CreateTemp(m.root, ".database-download-")
	if err != nil {
		return Database{}, err
	}
	defer func() { archive.Close(); os.Remove(archive.Name()) }()
	hasher := sha256.New()
	if _, err := localtools.CopyBounded(io.MultiWriter(archive, hasher), response.Body, maxBytes); err != nil {
		return Database{}, err
	}
	if "sha256:"+hex.EncodeToString(hasher.Sum(nil)) != document.Checksum {
		return Database{}, errors.New("Grype database download checksum does not match the HTTPS listing")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return Database{}, err
	}
	return m.importDatabase(ctx, archive)
}

func databaseArchiveURL(archive string) (string, error) {
	if archive == "" || strings.ContainsAny(archive, "/\\?#%") || strings.Contains(archive, "..") || !strings.HasSuffix(archive, ".tar.zst") {
		return "", errors.New("Grype database listing has an invalid archive path")
	}
	base, err := url.Parse(databaseListing)
	if err != nil {
		return "", err
	}
	base.Path = path.Join(path.Dir(base.Path), archive)
	return base.String(), nil
}

func databaseGET(ctx context.Context, client *http.Client, address string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download Grype database: %s", networking.Redact(err.Error()))
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("Grype database host returned HTTP %d", response.StatusCode)
	}
	return response, nil
}
