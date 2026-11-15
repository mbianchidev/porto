package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	apitypes "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/runtimefiles"
	"github.com/mbianchidev/porto/internal/runtimes"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
)

func TestPrunePreviewProtectsOwnersAndRevalidatesIdentity(t *testing.T) {
	manager := New(&fakeRunner{})
	unused := StorageResource{
		Resource: datafiles.Resource{Kind: "volume", Name: "unused-fixture", ID: "fixture-volume-1"},
	}
	owned := StorageResource{
		Resource: datafiles.Resource{Kind: "volume", Name: "owned-fixture", ID: "fixture-volume-2"},
		InUse:    true, Owners: []StorageOwner{{ID: "fixture-container", ComposeProject: "fixture-compose"}},
	}
	finishStorageResource(manager, &unused)
	finishStorageResource(manager, &owned)
	manager.storageReader = func(context.Context) (StorageUsage, error) {
		return StorageUsage{Resources: []StorageResource{unused, owned}}, nil
	}
	request := dataops.Request{Action: "prune", Categories: []string{"volume"}}
	first, err := manager.PreviewPrune(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Candidates) != 1 || len(first.Excluded) != 1 || first.UpperBound != nil {
		t.Fatalf("unsafe or fabricated prune preview: %+v", first)
	}
	unused.Resource.ID = "recreated-fixture-volume"
	finishStorageResource(manager, &unused)
	second, err := manager.PreviewPrune(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token {
		t.Fatal("a reused volume name did not invalidate the preview")
	}
	if _, err := manager.PreviewPrune(context.Background(), dataops.Request{}); err == nil {
		t.Fatal("broad cleanup became the default")
	}
}

func TestDockerSystemDiskUsageReportsUniqueStoresWithoutInventingTotals(t *testing.T) {
	manager := New(&fakeRunner{})
	size := int64(1024)
	manager.storageReader = func(context.Context) (StorageUsage, error) {
		return StorageUsage{
			ContentBytes: 1024, SnapshotBytes: 2048,
			Resources: []StorageResource{{Resource: datafiles.Resource{Kind: "image", ID: "fixture-image", Name: "fixture:latest"}, LogicalBytes: &size, SharedBytes: &size}},
			Warnings:  []string{"Metadata allocation unavailable"}, Accounting: "namespace-unique",
		}, nil
	}
	response := httptest.NewRecorder()
	NewAPI(manager, "/tmp/fixture.sock").ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1.47/system/df", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("disk usage status=%d: %s", response.Code, response.Body.String())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if string(result["LayersSize"]) != "1024" || result["Warnings"] == nil || result["PortoAccounting"] == nil {
		t.Fatalf("accounting provenance missing: %s", response.Body.String())
	}
	if _, fabricated := result["TotalBytes"]; fabricated {
		t.Fatal("a shared-store grand total was fabricated")
	}
}

func TestPrunePreviewFreezesCategoryToExactUnusedObjects(t *testing.T) {
	manager := New(&fakeRunner{})
	unused := StorageResource{Resource: datafiles.Resource{Kind: "volume", Name: "synthetic-unused", ID: "volume-1"}}
	owned := StorageResource{Resource: datafiles.Resource{Kind: "volume", Name: "synthetic-owned", ID: "volume-2"}, InUse: true}
	finishStorageResource(manager, &unused)
	finishStorageResource(manager, &owned)
	manager.storageReader = func(context.Context) (StorageUsage, error) {
		return StorageUsage{Resources: []StorageResource{unused, owned}}, nil
	}
	preview, err := manager.PreviewPrune(context.Background(), dataops.Request{Action: "prune", Categories: []string{"volume"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Request.Categories) != 0 || len(preview.Request.Selections) != 1 ||
		preview.Request.Selections[0].ID != unused.Resource.ID {
		t.Fatalf("confirmation is not bound to the exact previewed objects: %+v", preview.Request)
	}
	another := StorageResource{Resource: datafiles.Resource{Kind: "volume", Name: "synthetic-new", ID: "volume-3"}}
	finishStorageResource(manager, &another)
	manager.storageReader = func(context.Context) (StorageUsage, error) {
		return StorageUsage{Resources: []StorageResource{unused, owned, another}}, nil
	}
	refreshed, err := manager.PreviewPrune(context.Background(), preview.Request)
	if err != nil || refreshed.Token != preview.Token || len(refreshed.Candidates) != 1 {
		t.Fatalf("a new unused object changed the confirmed removal boundary: %+v, %v", refreshed, err)
	}
}

func TestPrunePreviewRejectsMissingOrInvalidSelectedObjects(t *testing.T) {
	manager := New(&fakeRunner{})
	item := StorageResource{Resource: datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"}}
	finishStorageResource(manager, &item)
	manager.storageReader = func(context.Context) (StorageUsage, error) {
		return StorageUsage{Resources: []StorageResource{item}}, nil
	}
	for _, test := range []struct {
		name   string
		choice dataops.Selection
		want   error
	}{
		{"unknown category", dataops.Selection{Kind: "snapshot", Name: item.Resource.Name, ID: item.Resource.ID}, datafiles.ErrInvalid},
		{"missing name", dataops.Selection{Kind: "volume", ID: item.Resource.ID}, datafiles.ErrInvalid},
		{"missing identity", dataops.Selection{Kind: "volume", Name: item.Resource.Name}, datafiles.ErrInvalid},
		{"recreated resource", dataops.Selection{Kind: "volume", Name: item.Resource.Name, ID: "old-volume"}, datafiles.ErrConflict},
		{"removed resource", dataops.Selection{Kind: "volume", Name: "synthetic-missing", ID: "old-volume"}, datafiles.ErrConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := manager.PreviewPrune(context.Background(), dataops.Request{Action: "prune", Selections: []dataops.Selection{test.choice}})
			if !errors.Is(err, test.want) {
				t.Fatalf("invalid selection was silently omitted: %v, want %v", err, test.want)
			}
		})
	}
}

func TestStorageUsageReconcilesContentAndExplicitBackendSnapshotter(t *testing.T) {
	for _, label := range []string{"", "native"} {
		t.Run("namespace="+label, func(t *testing.T) {
			service := &snapshotterTestServer{snapshotter: firstNonEmpty(label, "overlayfs"), usage: map[string]*snapshotsapi.UsageResponse{
				"synthetic-base": {Size: 4096}, "synthetic-write": {Size: 2048},
			}}
			backend := newSnapshotterTestRuntime(t, "synthetic-engine", label, service)
			fixture := seedSnapshotterTestImage(t, backend, service)
			manager := New(&fakeRunner{})
			manager.runtimeConnector = func(context.Context) (containerRuntime, error) { return backend, nil }
			usage, err := manager.StorageUsage(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			if want := fixture.layer.Size + fixture.config.Size + fixture.manifest.Size; usage.ContentBytes != want {
				t.Fatalf("content allocation = %d, want exactly %d unique stored bytes", usage.ContentBytes, want)
			}
			if usage.SnapshotBytes != 6144 || usage.WritableBytes != 2048 {
				t.Fatalf("snapshot stores do not reconcile: %+v", usage)
			}
		})
	}
}

func TestStorageUsageBorrowsTheHealthyInventoryRuntime(t *testing.T) {
	service := &snapshotterTestServer{snapshotter: "overlayfs"}
	backend := newSnapshotterTestRuntime(t, "synthetic-engine", "", service)
	seedSnapshotterTestImage(t, backend, service)
	manager := New(&fakeRunner{})
	manager.inventoryCancel = func() {}
	manager.inventory = &containerInventory{
		snapshot: ContainerSnapshot{Available: true}, runtimeClient: backend,
	}
	manager.runtimeConnector = func(context.Context) (containerRuntime, error) {
		return nil, errors.New("storage unexpectedly opened another runtime connection")
	}
	if _, err := manager.StorageUsage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Snapshot(context.Background()); err != nil {
		t.Fatalf("storage closed its borrowed inventory connection: %v", err)
	}
}

func TestStorageUsageProtectsConfigDigestOwnersAndRetainsDigestReferences(t *testing.T) {
	service := &snapshotterTestServer{snapshotter: "overlayfs"}
	backend := newSnapshotterTestRuntime(t, "synthetic-engine", "", service)
	fixture := seedSnapshotterTestImage(t, backend, service)
	service.image.Name = "registry.invalid/synthetic@" + fixture.manifest.Digest.String()
	service.container = &containersapi.Container{
		ID:     "synthetic-owner",
		Labels: map[string]string{nerdctlNameLabel: "synthetic-app", nerdctlImageDigestLabel: fixture.config.Digest.String()},
	}
	manager := New(&fakeRunner{})
	manager.runtimeConnector = func(context.Context) (containerRuntime, error) { return backend, nil }
	usage, err := manager.StorageUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range usage.Resources {
		if item.Resource.Kind == "image" {
			if !item.InUse || len(item.Owners) != 1 || item.Dangling {
				t.Fatalf("a config-digest owner or retained digest reference was misclassified: %+v", item)
			}
			return
		}
	}
	t.Fatal("synthetic image was omitted")
}

func TestStorageUsageHandlesPartiallyPulledMultiplatformImages(t *testing.T) {
	service := &snapshotterTestServer{snapshotter: "overlayfs"}
	backend := newSnapshotterTestRuntime(t, "synthetic-engine", "", service)
	fixture := seedSnapshotterTestImage(t, backend, service)
	index, err := json.Marshal(ocispec.Index{
		Versioned: imagespec.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{fixture.manifest, {
			MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("synthetic-foreign-manifest"), Size: 1234,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(index), Size: int64(len(index))}
	if err := content.WriteBlob(context.Background(), fixture.store, descriptor.Digest.String(), bytes.NewReader(index), descriptor); err != nil {
		t.Fatal(err)
	}
	service.image.Target = &apitypes.Descriptor{MediaType: descriptor.MediaType, Digest: descriptor.Digest.String(), Size: descriptor.Size}
	manager := New(&fakeRunner{})
	manager.runtimeConnector = func(context.Context) (containerRuntime, error) { return backend, nil }
	usage, err := manager.StorageUsage(context.Background())
	if err != nil {
		t.Fatalf("one missing foreign-platform manifest prevented actual store accounting: %v", err)
	}
	if want := fixture.layer.Size + fixture.config.Size + fixture.manifest.Size + descriptor.Size; usage.ContentBytes != want {
		t.Fatalf("downloaded blob allocation = %d, want %d", usage.ContentBytes, want)
	}
	if !usage.Resources[0].Protected || usage.Resources[0].Reason == "" {
		t.Fatal("incomplete image ownership was not protected and explained")
	}
}

type scopedCacheServer struct {
	buildKitPlatformServer
	records []*controlapi.UsageRecord
	calls   int
}

func (s *scopedCacheServer) Prune(request *controlapi.PruneRequest, stream grpc.ServerStreamingServer[controlapi.UsageRecord]) error {
	s.calls++
	if !request.All || len(request.Filter) != 1 || request.Filter[0] != "id==synthetic-cache" {
		return fmt.Errorf("cleanup escaped the exact cache selection: %+v", request)
	}
	for _, record := range s.records {
		if err := stream.Send(record); err != nil {
			return err
		}
	}
	return nil
}

func (s *scopedCacheServer) DiskUsage(context.Context, *controlapi.DiskUsageRequest) (*controlapi.DiskUsageResponse, error) {
	return &controlapi.DiskUsageResponse{Record: s.records}, nil
}

type lineageCacheServer struct {
	buildKitPlatformServer
	records []*controlapi.UsageRecord
}

func (s *lineageCacheServer) Prune(request *controlapi.PruneRequest, stream grpc.ServerStreamingServer[controlapi.UsageRecord]) error {
	id, ok := strings.CutPrefix(strings.Join(request.Filter, ","), "id==")
	if !request.All || !ok || strings.Contains(id, ",") {
		return fmt.Errorf("cleanup escaped the exact cache selection: %+v", request)
	}
	for _, record := range s.records {
		if slices.Contains(record.Parents, id) {
			return nil
		}
	}
	for index, record := range s.records {
		if record.ID == id {
			s.records = slices.Delete(s.records, index, index+1)
			return stream.Send(record)
		}
	}
	return nil
}

func (s *lineageCacheServer) DiskUsage(context.Context, *controlapi.DiskUsageRequest) (*controlapi.DiskUsageResponse, error) {
	return &controlapi.DiskUsageResponse{Record: s.records}, nil
}

func TestScopedPruneRemovesCacheDescendantsBeforeParents(t *testing.T) {
	service := &lineageCacheServer{records: []*controlapi.UsageRecord{
		{ID: "a-parent", Size: 1024},
		{ID: "b-child", Size: 1024, Parents: []string{"a-parent"}},
		{ID: "c-grandchild", Size: 1024, Parents: []string{"b-child"}},
		{ID: "z-unrelated", Size: 1024},
	}}
	manager := New(&fakeRunner{})
	manager.dialBuildKit = buildKitControlTestDialer(t, service)
	manager.storageReader = func(ctx context.Context) (StorageUsage, error) {
		resources, err := manager.BuildCacheUsage(ctx)
		return StorageUsage{Resources: resources}, err
	}
	preview, err := manager.PreviewPrune(context.Background(), dataops.Request{Action: "prune", Categories: []string{"cache"}})
	if err != nil {
		t.Fatal(err)
	}
	request := preview.Request
	request.Confirm = true
	if result, err := manager.Prune(context.Background(), request, func(string, int64) error { return nil }); err != nil {
		t.Fatalf("cache lineage blocked cleanup: %+v, %v", result, err)
	}
	if len(service.records) != 0 {
		t.Fatalf("unused cache records remained: %+v", service.records)
	}
}

func TestStorageUsageReportsMeasuredMetadataAndExclusiveBuildCache(t *testing.T) {
	service := &snapshotterTestServer{snapshotter: "overlayfs"}
	backend := newSnapshotterTestRuntime(t, "synthetic-engine", "", service)
	seedSnapshotterTestImage(t, backend, service)
	backend.address = "/synthetic/containerd.sock"
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		switch strings.Join(command.Args, " ") {
		case "list porto-engine --json":
			return []byte(`{"name":"porto-engine","status":"Running"}`), nil
		case `shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`:
			return []byte("synthetic-owner\n"), nil
		}
		return nil, nil
	}, streamer: func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		header, err := bufio.NewReader(command.StdinReader).ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var envelope runtimefiles.Envelope
		if err := json.Unmarshal(header, &envelope); err != nil {
			return nil, err
		}
		if envelope.Request.Action != "metadata-usage" || envelope.Descriptor.Address != backend.address {
			return nil, fmt.Errorf("unexpected metadata request: %+v", envelope)
		}
		return nil, emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte(`{"logicalBytes":32768,"allocatedBytes":65536}`)})
	}}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.lookPath = func(name string) (string, error) { return name, nil }
	if err := manager.writeEngineState(engineState{Mode: "lima", Instance: engineInstanceName, OwnerID: "synthetic-owner"}); err != nil {
		t.Fatal(err)
	}
	manager.runtimeConnector = func(context.Context) (containerRuntime, error) { return backend, nil }
	manager.dialBuildKit = buildKitControlTestDialer(t, &scopedCacheServer{records: []*controlapi.UsageRecord{
		{ID: "exclusive", Size: 1024, Description: "synthetic copy", RecordType: "regular"},
		{ID: "shared", Size: 2048, Shared: true},
	}})
	usage, err := manager.StorageUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.MetadataBytes == nil || *usage.MetadataBytes != 65536 {
		t.Fatalf("measurable shared metadata was omitted: %+v", usage)
	}
	if usage.BuildCacheBytes == nil || *usage.BuildCacheBytes != 1024 {
		t.Fatalf("shared cache obscured exclusive allocation: %+v", usage)
	}
}

func TestScopedPruneRechecksOwnershipAndReportsActualCacheRemoval(t *testing.T) {
	for _, test := range []struct {
		name    string
		changed bool
		records []*controlapi.UsageRecord
		wantErr bool
	}{
		{"selected record removed", false, []*controlapi.UsageRecord{{ID: "synthetic-cache", Size: 1024}}, false},
		{"record acquired by a build", true, []*controlapi.UsageRecord{{ID: "synthetic-cache", Size: 1024}}, true},
		{"record not removed", false, nil, true},
		{"unexpected record removed", false, []*controlapi.UsageRecord{{ID: "other-cache", Size: 1024}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &scopedCacheServer{records: test.records}
			manager := New(&fakeRunner{})
			manager.dialBuildKit = buildKitControlTestDialer(t, service)
			item := StorageResource{Resource: datafiles.Resource{Kind: "cache", Name: "synthetic-cache", ID: "synthetic-cache"}}
			finishStorageResource(manager, &item)
			manager.storageReader = func(context.Context) (StorageUsage, error) {
				return StorageUsage{Resources: []StorageResource{item}}, nil
			}
			preview, err := manager.PreviewPrune(context.Background(), dataops.Request{Action: "prune", Categories: []string{"cache"}})
			if err != nil {
				t.Fatal(err)
			}
			request := preview.Request
			request.Confirm = true
			result, err := manager.Prune(context.Background(), request, func(string, int64) error {
				item.InUse = test.changed
				return nil
			})
			if (err != nil) != test.wantErr {
				t.Fatalf("cleanup falsely reported its result: %+v, %v", result, err)
			}
			if test.changed && service.calls != 0 {
				t.Fatal("a newly referenced cache record reached the destructive backend")
			}
			if !test.wantErr && (len(result.Steps) != 1 || result.Steps[0].Status != "succeeded") {
				t.Fatalf("actual removal was not recorded: %+v", result)
			}
		})
	}
}

func TestCreatedContainerVolumesReceiveDockerDefaultRootMode(t *testing.T) {
	var mu sync.Mutex
	requests := map[string]runtimefiles.Envelope{}
	runner := &fakeRunner{outputs: map[string][]byte{
		"limactl list porto-engine --json":                                                []byte(`{"name":"porto-engine","status":"Running"}`),
		`limactl shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`: []byte("synthetic-owner\n"),
	}}
	runner.streamer = func(command runtimes.Command, _ func(runtimes.OutputChunk) error) ([]byte, error) {
		header, err := bufio.NewReader(command.StdinReader).ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var envelope runtimefiles.Envelope
		if err := json.Unmarshal(header, &envelope); err != nil {
			return nil, err
		}
		mu.Lock()
		requests[envelope.Descriptor.Resource.Name] = envelope
		mu.Unlock()
		return nil, nil
	}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.lookPath = func(name string) (string, error) { return name, nil }
	if err := manager.writeEngineState(engineState{Mode: "lima", Instance: engineInstanceName, OwnerID: "synthetic-owner"}); err != nil {
		t.Fatal(err)
	}
	manager.fileDescriptorReader = func(_ context.Context, kind, name string) (runtimefiles.Descriptor, error) {
		return runtimefiles.Descriptor{
			Resource:       datafiles.Resource{Kind: kind, Name: name, ID: name},
			RootPath:       "/var/lib/nerdctl/volumes/default/" + name + "/_data",
			VolumeIdentity: "synthetic-identity-" + name,
		}, nil
	}
	anonymous := strings.Repeat("d", 64)
	manager.inventoryCancel = func() {}
	manager.inventory = &containerInventory{snapshot: ContainerSnapshot{Available: true, Containers: []Container{{
		ID: "synthetic-app", Name: "synthetic-app", Labels: map[string]string{
			"nerdctl/mounts.0": `{"Type":"volume","Name":"app_session-data","Source":"/var/lib/nerdctl/volumes/default/app_session-data/_data","Destination":"/sessions"}`,
			"nerdctl/mounts.1": `{"Type":"bind","Source":"/srv/app","Destination":"/app"}`,
			"nerdctl/mounts.2": `{"Type":"volume","Name":"` + anonymous + `","Source":"/var/lib/nerdctl/volumes/default/` + anonymous + `/_data","Destination":"/cache"}`,
		},
	}}}}
	manager.applyContainerVolumeRootModes(context.Background(), "synthetic-app")
	if len(requests) != 2 {
		t.Fatalf("volume permission requests = %v, want the named and anonymous volumes only", slices.Sorted(maps.Keys(requests)))
	}
	for _, name := range []string{"app_session-data", anonymous} {
		envelope, ok := requests[name]
		if !ok || envelope.Request.Action != "default-root-mode" || envelope.Request.Identity != envelope.Descriptor.Resource.Fingerprint() ||
			envelope.Descriptor.VolumeIdentity != "synthetic-identity-"+name {
			t.Fatalf("volume %s permission request = %+v", name, envelope)
		}
	}
	clear(requests)
	if _, err := manager.CreateVolume(context.Background(), "app_cache-data", "local", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateVolume(context.Background(), "restored-data", "local", map[string]string{transferOwnerLabel: "synthetic-transfer"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := requests["app_cache-data"]; !ok || len(requests) != 1 {
		t.Fatalf("volume creation permission requests = %v, want only the new non-transfer volume", slices.Sorted(maps.Keys(requests)))
	}
}
