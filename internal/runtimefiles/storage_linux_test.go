//go:build linux

package runtimefiles

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	introspectionapi "github.com/containerd/containerd/api/services/introspection/v1"
	"github.com/mbianchidev/porto/internal/datafiles"
	"google.golang.org/grpc"
)

type metadataStoreServer struct {
	introspectionapi.UnimplementedIntrospectionServer
	database string
}

func (s *metadataStoreServer) Plugins(context.Context, *introspectionapi.PluginsRequest) (*introspectionapi.PluginsResponse, error) {
	return &introspectionapi.PluginsResponse{Plugins: []*introspectionapi.Plugin{
		{Type: "io.containerd.content.v1", ID: "content", Exports: map[string]string{"path": "/synthetic-unrelated/meta.db"}},
		{Type: "io.containerd.metadata.v1", ID: "bolt", Exports: map[string]string{"path": s.database}},
	}}, nil
}

func TestRuntimeMetadataUsageMeasuresAuthoritativeDatabaseStore(t *testing.T) {
	directory := t.TempDir()
	database := filepath.Join(directory, "meta.db")
	if err := os.WriteFile(database, bytes.Repeat([]byte("synthetic"), 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	address := filepath.Join(t.TempDir(), "containerd.sock")
	listener, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	introspectionapi.RegisterIntrospectionServer(server, &metadataStoreServer{database: database})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	var output bytes.Buffer
	descriptor := Descriptor{Address: address, Namespace: "synthetic"}
	if err := runtimeMetadataUsage(context.Background(), descriptor, &output); err != nil {
		t.Fatal(err)
	}
	var measured struct {
		LogicalBytes   int64  `json:"logicalBytes"`
		AllocatedBytes *int64 `json:"allocatedBytes"`
	}
	if err := json.Unmarshal(output.Bytes(), &measured); err != nil {
		t.Fatal(err)
	}
	logical, allocated, err := datafiles.DiskUsage(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if measured.LogicalBytes != logical || measured.AllocatedBytes == nil || allocated == nil || *measured.AllocatedBytes != *allocated {
		t.Fatalf("metadata store allocation does not reconcile: %s, want %d / %v", output.String(), logical, allocated)
	}
}

func TestNativeVolumeIdentitySurvivesDataReplacementButNotRecreation(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "volume.json"), []byte(`{"labels":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(directory, "_data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	descriptor := Descriptor{Resource: datafiles.Resource{Kind: "volume", Name: "synthetic-volume"}, RootPath: data}
	first, err := nativeVolumeResource(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := datafiles.ReplaceDirectory(context.Background(), data, func(stage string) error {
		return os.WriteFile(filepath.Join(stage, "synthetic.txt"), []byte("replacement"), 0o600)
	}, nil); err != nil {
		t.Fatal(err)
	}
	replaced, err := nativeVolumeResource(descriptor)
	if err != nil || replaced.Fingerprint() != first.Fingerprint() {
		t.Fatalf("data replacement retargeted backup/native attachment identity: %+v, %v", replaced, err)
	}
	metadata := filepath.Join(directory, "volume.json")
	if err := os.Rename(metadata, metadata+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"labels":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	recreated, err := nativeVolumeResource(descriptor)
	if err != nil || recreated.Fingerprint() == first.Fingerprint() {
		t.Fatalf("recreated native volume retained an obsolete identity: %+v, %v", recreated, err)
	}
}
