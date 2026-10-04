package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/dataops"
)

func TestUnattachedMigrationRequiresConsentWithoutMutatingTheSource(t *testing.T) {
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutations++
		}
		switch r.URL.Path {
		case "/containers/json":
			io.WriteString(w, "[]")
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	source := fixtureMigrationClient(server)
	if _, _, err := source.openVolumeArchive(context.Background(), dataops.Request{}, "fixture-volume", "fixture-identity", nil, nil); err == nil {
		t.Fatal("unattached volume helper ran without explicit consent")
	}
	if mutations != 0 {
		t.Fatal("source runtime was mutated by an unconsented request")
	}
}

func fixtureMigrationClient(server *httptest.Server) *migrationClient {
	return &migrationClient{
		context: MigrationContext{Name: "fixture-source", Endpoint: "unix:///synthetic-source.sock", Supported: true},
		client: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
		}}},
	}
}

func TestConsentedMigrationHelperIsStoppedReadonlyNoCopyAndOwnershipCleaned(t *testing.T) {
	var commands []string
	var imageConfig map[string]any
	var creation map[string]any
	reservations := make(map[string]dataops.SourceTemporary)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		commands = append(commands, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/volumes/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Name": "fixture-volume", "CreatedAt": "fixture-created", "Mountpoint": "/synthetic/data"})
		case r.URL.Path == "/containers/json":
			io.WriteString(w, "[]")
		case r.URL.Path == "/version":
			io.WriteString(w, `{"Os":"linux","Arch":"amd64"}`)
		case r.Method == "POST" && r.URL.Path == "/images/load":
			var err error
			imageConfig, err = fixtureScratchConfig(r.Body)
			if err != nil {
				t.Errorf("read helper image: %v", err)
			}
			io.WriteString(w, `{"stream":"Loaded temporary helper\n"}`+"\n")
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/images/"):
			lease := reservations["image"]
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:synthetic-source-helper-manifest", "RepoTags": []string{lease.Name}, "Config": imageConfig["config"]})
		case r.Method == "POST" && r.URL.Path == "/containers/create":
			if err := json.NewDecoder(r.Body).Decode(&creation); err != nil {
				t.Error(err)
			}
			io.WriteString(w, `{"Id":"synthetic-helper-id"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "synthetic-helper-id", "Name": "/" + reservations["container"].Name, "Config": map[string]any{"Labels": creation["Labels"]}})
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/archive"):
			w.Header().Set("Content-Type", "application/x-tar")
			io.WriteString(w, "synthetic archive stream")
		case r.Method == "DELETE" && (strings.HasPrefix(r.URL.Path, "/containers/") || strings.HasPrefix(r.URL.Path, "/images/")):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected source request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "forbidden", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	reserve := func(_ context.Context, resource dataops.SourceTemporary) error {
		reservations[resource.Kind] = resource
		return nil
	}
	clear := func(_ context.Context, resource dataops.SourceTemporary) error {
		delete(reservations, resource.Kind)
		return nil
	}
	digest := sha256.Sum256([]byte("fixture-volume\x00fixture-created\x00/synthetic/data"))
	stream, cleanup, err := fixtureMigrationClient(server).openVolumeArchive(context.Background(),
		dataops.Request{AllowSourceHelper: true}, "fixture-volume", hex.EncodeToString(digest[:]), reserve, clear)
	if err != nil {
		t.Fatal(err)
	}
	var contents bytes.Buffer
	if _, err := io.Copy(&contents, stream); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	host, ok := creation["HostConfig"].(map[string]any)
	if !ok || host["ReadonlyRootfs"] != true || host["NetworkMode"] != "none" {
		t.Fatalf("unsafe helper host config: %+v", creation)
	}
	mounts, ok := host["Mounts"].([]any)
	if !ok || len(mounts) != 1 {
		t.Fatalf("helper mounts=%v", host["Mounts"])
	}
	mount := mounts[0].(map[string]any)
	if mount["ReadOnly"] != true || mount["Source"] != "fixture-volume" || mount["VolumeOptions"].(map[string]any)["NoCopy"] != true {
		t.Fatalf("helper could modify/populate source volume: %+v", mount)
	}
	for _, command := range commands {
		if strings.Contains(command, "/start") || strings.HasPrefix(command, "DELETE /volumes/") {
			t.Fatalf("original source workload/data was modified: %s", command)
		}
	}
	if len(reservations) != 0 {
		t.Fatalf("helper cleanup reservations remained: %+v", reservations)
	}
}

func fixtureScratchConfig(input io.Reader) (map[string]any, error) {
	reader := tar.NewReader(input)
	for {
		header, err := reader.Next()
		if err != nil {
			return nil, err
		}
		if header.Name != "manifest.json" && strings.HasSuffix(header.Name, ".json") {
			var document map[string]any
			err := json.NewDecoder(reader).Decode(&document)
			return document, err
		}
	}
}
