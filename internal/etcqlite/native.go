package etcqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
)

const (
	discoveryPath   = "/etcqlite/v1/discovery"
	snapshotPath    = "/etcqlite/v1/snapshots"
	maxSnapshotSize = int64(8 << 30)
)

func identityETag(clusterID, memberID string) (string, error) {
	cluster, err := strconv.ParseUint(clusterID, 10, 64)
	if err != nil || cluster == 0 {
		return "", errors.New("invalid Etcqlite cluster identity")
	}
	member, err := strconv.ParseUint(memberID, 10, 64)
	if err != nil || member == 0 {
		return "", errors.New("invalid Etcqlite member identity")
	}
	return fmt.Sprintf(`"%016x:%016x"`, cluster, member), nil
}

func nativeRequest(ctx context.Context, method, target, token string, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("Etcqlite authorization token contains a newline")
	}
	if token != "" {
		request.Header.Set("Authorization", token)
	}
	return request, nil
}

func nativeDiscovery(ctx context.Context, client *http.Client, base, token, clusterID, memberID string) (Discovery, error) {
	var discovery Discovery
	request, err := nativeRequest(ctx, http.MethodGet, base+discoveryPath, token, nil)
	if err != nil {
		return discovery, err
	}
	response, err := client.Do(request)
	if err != nil {
		return discovery, fmt.Errorf("discover Etcqlite member: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return discovery, fmt.Errorf("Etcqlite discovery returned HTTP %d; check mTLS, native authorization and member logs", response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if err := decoder.Decode(&discovery); err != nil {
		return discovery, fmt.Errorf("decode Etcqlite discovery: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return discovery, errors.New("Etcqlite discovery contains trailing or oversized data")
	}
	if discovery.Format != 1 || discovery.Kind != "etcqlite" || discovery.SchemaVersion == 0 {
		return discovery, errors.New("unsupported Etcqlite discovery format")
	}
	if discovery.ClusterID != clusterID || discovery.MemberID != memberID {
		return discovery, errors.New("Etcqlite endpoint identity changed; refusing to retarget the managed member")
	}
	if !slices.Contains(discovery.Capabilities, "native-snapshot-v1") {
		return discovery, errors.New("Etcqlite member does not advertise native-snapshot-v1")
	}
	if discovery.SnapshotPath != snapshotPath {
		return discovery, errors.New("unsupported Etcqlite native snapshot path")
	}
	expected, err := identityETag(clusterID, memberID)
	if err != nil {
		return discovery, err
	}
	discovery.ETag = response.Header.Get("ETag")
	if discovery.ETag != expected {
		return discovery, errors.New("Etcqlite discovery ETag does not bind the expected member identity")
	}
	return discovery, nil
}

func nativeReady(ctx context.Context, client *http.Client, base, token string) error {
	request, err := nativeRequest(ctx, http.MethodGet, base+"/readyz", token, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("Etcqlite quorum/read readiness: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Etcqlite quorum/read readiness returned HTTP %d; inspect the member logs", response.StatusCode)
	}
	return nil
}

func nativeSnapshot(ctx context.Context, client *http.Client, base, token string, discovery Discovery, target string) (err error) {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create private native snapshot without overwriting existing recovery data: %w", err)
	}
	defer func() {
		err = errors.Join(err, file.Close())
		if err != nil {
			err = errors.Join(err, os.Remove(target))
		}
	}()
	request, err := nativeRequest(ctx, http.MethodPost, base+snapshotPath, token, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", discovery.ETag)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request native Etcqlite snapshot: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("native Etcqlite snapshot returned HTTP %d; identity/precondition and authorization failures never fall back to projections", response.StatusCode)
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "application/vnd.sqlite3" || response.Header.Get("ETag") != discovery.ETag {
		return errors.New("native Etcqlite snapshot response has an unexpected content type or identity ETag")
	}
	copied, err := io.Copy(file, io.LimitReader(response.Body, maxSnapshotSize+1))
	if err != nil {
		return fmt.Errorf("stream native Etcqlite snapshot: %w", err)
	}
	if copied < 48 || copied > maxSnapshotSize {
		return fmt.Errorf("native Etcqlite snapshot size %d is outside the supported 48-byte to 8-GiB range", copied)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync native Etcqlite snapshot: %w", err)
	}
	return nil
}

func verifySnapshot(path string, status SnapshotStatus, clusterID, memberID string) error {
	if status.Format != 1 || status.SchemaVersion == 0 || status.Revision < 1 ||
		status.CompactRevision < -1 || status.CompactRevision > status.Revision || status.ConfigurationID == 0 ||
		strconv.FormatUint(status.ClusterID, 10) != clusterID ||
		strconv.FormatUint(status.MemberID, 10) != memberID {
		return errors.New("native snapshot status does not match the expected format and member identity")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 48 || info.Size() > maxSnapshotSize || uint64(info.Size()) != status.Bytes {
		return errors.New("native snapshot size does not match verified status")
	}
	header := make([]byte, 16)
	if _, err := io.ReadFull(file, header); err != nil || !bytes.Equal(header, []byte("SQLite format 3\x00")) {
		return errors.New("native recovery image is not SQLite")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, io.NewSectionReader(file, 0, info.Size()-32)); err != nil {
		return fmt.Errorf("verify native snapshot payload: %w", err)
	}
	trailer := make([]byte, 32)
	if _, err := file.ReadAt(trailer, info.Size()-32); err != nil {
		return fmt.Errorf("read native snapshot SHA-256 trailer: %w", err)
	}
	sum := digest.Sum(nil)
	if !bytes.Equal(trailer, sum) || status.SHA256 != hex.EncodeToString(sum) {
		return errors.New("native snapshot SHA-256 trailer or verified status digest does not match the recovery image")
	}
	return nil
}
