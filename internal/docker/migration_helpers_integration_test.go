package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
)

func TestUnattachedVolumeMigrationRealDockerArchive(t *testing.T) {
	endpoint := os.Getenv("PORTO_TEST_MIGRATION_SOURCE_SOCKET")
	if endpoint == "" {
		t.Skip("requires an explicitly selected isolated test Docker engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	source := &migrationClient{
		context: MigrationContext{Name: "synthetic-source", Endpoint: endpoint, Supported: true},
		client: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialMigrationSource(ctx, endpoint)
		}}},
	}
	var version struct {
		OS   string `json:"Os"`
		Arch string
	}
	if err := source.get(ctx, "/version", nil, &version); err != nil {
		t.Fatal(err)
	}
	if version.OS != "linux" {
		t.Fatal("synthetic source must be a Linux Docker engine")
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	owner := hex.EncodeToString(token)
	archive, _, err := scratchMigrationImage(owner, version.Arch)
	if err != nil {
		t.Fatal(err)
	}
	fixtureImage := dataops.SourceTemporary{
		Context: source.context.Name, Endpoint: endpoint, Kind: "image",
		Name: "porto-migration-helper:" + owner, Owner: owner,
	}
	response, err := source.request(ctx, http.MethodPost, "/images/load", nil, bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	fixtureImage.ID, err = source.temporaryIdentity(ctx, fixtureImage)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := source.cleanupTemporary(cleanupContext, fixtureImage); err != nil {
			t.Errorf("cleanup exact fixture image: %v", err)
		}
	})
	volumeName := "porto-migration-fixture-" + owner
	volumeRequest, _ := json.Marshal(map[string]any{"Name": volumeName, "Labels": map[string]string{sourceHelperOwnerLabel: owner}})
	response, err = source.request(ctx, http.MethodPost, "/volumes/create", nil, bytes.NewReader(volumeRequest))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var inspected struct{ Labels map[string]string }
		if err := source.get(cleanupContext, "/volumes/"+volumeName, nil, &inspected); err != nil || inspected.Labels[sourceHelperOwnerLabel] != owner {
			t.Errorf("fixture volume ownership not proven: %v", err)
			return
		}
		response, err := source.request(cleanupContext, http.MethodDelete, "/volumes/"+volumeName, nil, nil)
		if err != nil {
			t.Errorf("cleanup exact fixture volume: %v", err)
		} else {
			response.Body.Close()
		}
	})
	seed := dataops.SourceTemporary{Context: source.context.Name, Endpoint: endpoint, Kind: "container", Name: "porto-migration-seed-" + owner, Owner: owner}
	seedRequest, _ := json.Marshal(map[string]any{
		"Image": fixtureImage.ID, "Cmd": []string{"/never-started"}, "Labels": map[string]string{sourceHelperOwnerLabel: owner},
		"HostConfig": map[string]any{"NetworkMode": "none", "Mounts": []map[string]any{{
			"Type": "volume", "Source": volumeName, "Target": "/data", "VolumeOptions": map[string]bool{"NoCopy": true},
		}}},
	})
	response, err = source.request(ctx, http.MethodPost, "/containers/create", url.Values{"name": {seed.Name}}, bytes.NewReader(seedRequest))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	seed.ID = created.ID
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := source.cleanupTemporary(cleanupContext, seed); err != nil && !sourceNotFound(err) {
			t.Errorf("cleanup synthetic seed container: %v", err)
		}
	})
	var payload bytes.Buffer
	writer := tar.NewWriter(&payload)
	content := []byte("synthetic unattached volume contents\n")
	if err := writer.WriteHeader(&tar.Header{Name: "fixture.txt", Typeflag: tar.TypeReg, Mode: 0o640, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	writer.Write(content)
	writer.Close()
	response, err = source.request(ctx, http.MethodPut, "/containers/"+seed.ID+"/archive", url.Values{"path": {"/data"}}, bytes.NewReader(payload.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if err := source.cleanupTemporary(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if id, _, err := source.findVolumeContainer(ctx, volumeName); err != nil || id != "" {
		t.Fatalf("fixture volume is not unattached: %s %v", id, err)
	}
	var volume struct{ Name, CreatedAt, Mountpoint string }
	if err := source.get(ctx, "/volumes/"+volumeName, nil, &volume); err != nil {
		t.Fatal(err)
	}
	identity := sha256.Sum256([]byte(volume.Name + "\x00" + volume.CreatedAt + "\x00" + volume.Mountpoint))
	reservations := make(map[string]dataops.SourceTemporary)
	reserve := func(_ context.Context, resource dataops.SourceTemporary) error {
		reservations[resource.Kind] = resource
		return nil
	}
	clear := func(_ context.Context, resource dataops.SourceTemporary) error {
		delete(reservations, resource.Kind)
		return nil
	}
	stream, cleanup, err := source.openVolumeArchive(ctx, dataops.Request{AllowSourceHelper: true}, volumeName, hex.EncodeToString(identity[:]), reserve, clear)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cleanup(cleanupContext); err != nil {
			t.Errorf("cleanup exact consented source helpers: %v", err)
		}
	})
	file, err := os.CreateTemp(t.TempDir(), "verified-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	_, sealErr := datafiles.SealTar(ctx, stream, file, datafiles.Resource{Kind: "volume", Name: volumeName, ID: hex.EncodeToString(identity[:])})
	if err := errors.Join(sealErr, stream.Close()); err != nil {
		t.Fatal(err)
	}
	manifest, err := datafiles.Validate(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range manifest.Entries {
		if strings.HasSuffix(entry.Path, "fixture.txt") && entry.Size == int64(len(content)) && entry.Mode == 0o640 {
			found = true
		}
	}
	if !found {
		t.Fatalf("unattached fixture data/permissions not transferred: %+v", manifest)
	}
	if err := cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if len(reservations) != 0 {
		t.Fatalf("source helper reservations survived successful cleanup: %+v", reservations)
	}
	if err := source.get(ctx, "/volumes/"+volumeName, nil, &volume); err != nil {
		t.Fatal("original fixture volume was removed")
	}
}
