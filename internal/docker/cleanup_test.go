package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/runtimes"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
)

type cleanupBuildKitServer struct {
	buildKitPlatformServer
	pruneMu    sync.Mutex
	pruneCalls int
	all        bool
	records    []*controlapi.UsageRecord
	pruneErr   error
	wait       bool
}

func (s *cleanupBuildKitServer) Prune(request *controlapi.PruneRequest, stream grpc.ServerStreamingServer[controlapi.UsageRecord]) error {
	s.pruneMu.Lock()
	s.pruneCalls++
	s.all = request.All
	s.pruneMu.Unlock()
	if len(request.Filter) != 0 || request.KeepDuration != 0 {
		return errors.New("cleanup changed the requested all-unused cache semantics")
	}
	for _, record := range s.records {
		if err := stream.Send(record); err != nil {
			return err
		}
	}
	if s.wait {
		<-stream.Context().Done()
		return context.Cause(stream.Context())
	}
	return s.pruneErr
}

type cleanupImageStore struct {
	images.Store
	blobs     content.Store
	mu        sync.Mutex
	records   map[string]images.Image
	deleted   []string
	deleteErr map[string]error
	current   func(images.Image) images.Image
}

func (s *cleanupImageStore) List(context.Context, ...string) ([]images.Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]images.Image, 0, len(s.records))
	for _, name := range slices.Sorted(maps.Keys(s.records)) {
		records = append(records, s.records[name])
	}
	return records, nil
}

func (s *cleanupImageStore) Get(_ context.Context, name string) (images.Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[name]
	if !ok {
		return images.Image{}, errdefs.ErrNotFound
	}
	if s.current != nil {
		record = s.current(record)
	}
	return record, nil
}

func (s *cleanupImageStore) Delete(ctx context.Context, name string, options ...images.DeleteOpt) error {
	var settings images.DeleteOptions
	for _, option := range options {
		if err := option(ctx, &settings); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.deleteErr[name]; err != nil {
		return err
	}
	record, ok := s.records[name]
	if !ok {
		return errdefs.ErrNotFound
	}
	if settings.Target == nil || settings.Target.Digest != record.Target.Digest {
		return errors.New("image deletion must pin the listed target")
	}
	delete(s.records, name)
	s.deleted = append(s.deleted, name)
	return nil
}

type cleanupContainerStore struct {
	containers.Store
	users map[string]bool
}

func (s *cleanupContainerStore) List(_ context.Context, filters ...string) ([]containers.Container, error) {
	for _, filter := range filters {
		name, err := strconv.Unquote(strings.TrimPrefix(filter, "image=="))
		if err != nil {
			return nil, fmt.Errorf("unexpected container filter %q", filter)
		}
		if s.users[name] {
			return []containers.Container{{ID: "late-user", Image: name}}, nil
		}
	}
	return nil, nil
}

func cleanupImageRecord(name string, created, updated time.Time, labels map[string]string) images.Image {
	return images.Image{
		Name: name, Labels: labels, CreatedAt: created, UpdatedAt: updated,
		Target: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromString(name)},
	}
}

// cleanupImages has two removable images and seven that cleanup keeps: used
// by name, used by image ID, a default and a custom-named checkpoint, the base
// images their restores load by name and by target, and a tag first pulled
// long ago but rebuilt an hour ago.
func cleanupImages(t *testing.T, now time.Time) *cleanupImageStore {
	t.Helper()
	blobs, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old, recent := now.Add(-72*time.Hour), now.Add(-time.Hour)
	store := &cleanupImageStore{blobs: blobs, records: map[string]images.Image{}}
	add := func(record images.Image) images.Image {
		store.records[record.Name] = record
		return record
	}
	checkpoint := func(name, base string, baseTarget ocispec.Descriptor) {
		index := ocispec.Index{Manifests: []ocispec.Descriptor{baseTarget}}
		if base != "" {
			index.Annotations = map[string]string{ocispec.AnnotationRefName: base}
		}
		data, err := json.Marshal(index)
		if err != nil {
			t.Fatal(err)
		}
		descriptor := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(data), Size: int64(len(data))}
		if err := content.WriteBlob(context.Background(), blobs, descriptor.Digest.String(), bytes.NewReader(data), descriptor); err != nil {
			t.Fatal(err)
		}
		record := cleanupImageRecord(name, old, old, map[string]string{checkpointSourceIDLabel: "stopped-app"})
		record.Target = descriptor
		add(record)
	}
	add(cleanupImageRecord("docker.io/example/unused:one", old, old, nil))
	add(cleanupImageRecord("docker.io/example/unused:two", old, old, nil))
	add(cleanupImageRecord("docker.io/example/used:latest", old, old, nil))
	add(cleanupImageRecord("docker.io/example/used-by-id:latest", old, old, nil))
	add(cleanupImageRecord("docker.io/example/rebuilt:dev", old, recent, nil))
	byName := add(cleanupImageRecord("docker.io/example/restore-base:latest", old, old, nil))
	byTarget := add(cleanupImageRecord("registry.example.test/team/restore-base:v1", old, old, nil))
	checkpoint(checkpointImagePrefix+"stopped-app:before-upgrade", byName.Name, byName.Target)
	checkpoint("registry.example.test/team/custom-checkpoint:v1", "", byTarget.Target)
	return store
}

func cleanupContainerList() []byte {
	return []byte(`{"ID":"running-app","Names":"running-app","Image":"example/used","State":"running"}` + "\n" +
		`{"ID":"pinned-app","Names":"pinned-app","Image":"","ImageID":"` + digest.FromString("docker.io/example/used-by-id:latest").String() + `","State":"running"}` + "\n")
}

func newCleanupManager(t *testing.T, runner *fakeRunner, store *cleanupImageStore, service *cleanupBuildKitServer) *Manager {
	t.Helper()
	if runner.outputs == nil {
		runner.outputs = map[string][]byte{}
	}
	if _, ok := runner.outputs["nerdctl ps -a --no-trunc --format {{json .}}"]; !ok {
		runner.outputs["nerdctl ps -a --no-trunc --format {{json .}}"] = cleanupContainerList()
	}
	manager := New(runner)
	manager.dialBuildKit = buildKitControlTestDialer(t, service)
	manager.imageMetadataConnector = cleanupMetadata(store, &cleanupContainerStore{})
	return manager
}

func cleanupMetadata(store *cleanupImageStore, users *cleanupContainerStore) func(context.Context) (context.Context, imageMetadataStores, func() error, error) {
	return func(ctx context.Context) (context.Context, imageMetadataStores, func() error, error) {
		return ctx, imageMetadataStores{images: store, containers: users, content: store.blobs}, func() error { return nil }, nil
	}
}

func TestCleanupUnusedRemovesOnlyOldUnusedNonCheckpointImages(t *testing.T) {
	service := &cleanupBuildKitServer{records: []*controlapi.UsageRecord{
		{ID: "synthetic-cache-1", Size: 1024},
		{ID: "synthetic-cache-2", Size: 2048},
	}}
	store := cleanupImages(t, time.Now())
	result, err := newCleanupManager(t, &fakeRunner{}, store, service).CleanupUnused(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if service.pruneCalls != 1 || !service.all {
		t.Fatalf("duplicate or incomplete cache prune: calls=%d, all=%t", service.pruneCalls, service.all)
	}
	if result.BuildCache.Status != app.CleanupSucceeded || result.BuildCache.ItemsRemoved != 2 ||
		result.BuildCache.BytesReclaimed == nil || *result.BuildCache.BytesReclaimed != 3072 {
		t.Fatalf("incorrect build-cache result: %+v", result.BuildCache)
	}
	if want := []string{"docker.io/example/unused:one", "docker.io/example/unused:two"}; !reflect.DeepEqual(store.deleted, want) {
		t.Fatalf("deleted images %v, want only %v", store.deleted, want)
	}
	if result.Images.Status != app.CleanupSucceeded || result.Images.ItemsRemoved != 2 || result.Images.BytesReclaimed != nil ||
		!strings.Contains(result.Images.Output, "example/unused:two") || !strings.Contains(result.Images.Output, "Kept 7 image reference(s)") {
		t.Fatalf("incorrect image result: %+v", result.Images)
	}
	if result.Containers.Status != app.CleanupSucceeded || result.Volumes.Status != app.CleanupSucceeded ||
		result.HostDisk.Status != app.CleanupSkipped {
		t.Fatalf("cleanup skipped a runtime step or claimed a host disk it does not own: %+v", result)
	}
}

func TestCleanupImageRemovalRechecksEachReferenceBeforeDeleting(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name    string
		current func(images.Image) images.Image
		users   bool
		err     error
	}{
		{name: "tag rebuilt", current: func(record images.Image) images.Image {
			record.Target.Digest = digest.FromString("rebuilt")
			return record
		}},
		{name: "tag re-pulled", current: func(record images.Image) images.Image {
			record.UpdatedAt = now
			return record
		}},
		{name: "container created", users: true},
		{name: "target changed during delete", err: errdefs.ErrFailedPrecondition},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &cleanupImageStore{current: test.current, records: map[string]images.Image{
				"docker.io/example/unused:one": cleanupImageRecord("docker.io/example/unused:one", now.Add(-72*time.Hour), now.Add(-72*time.Hour), nil),
			}}
			if test.err != nil {
				store.deleteErr = map[string]error{"docker.io/example/unused:one": test.err}
			}
			manager := newCleanupManager(t, &fakeRunner{}, store, &cleanupBuildKitServer{})
			manager.imageMetadataConnector = cleanupMetadata(store, &cleanupContainerStore{users: map[string]bool{"docker.io/example/unused:one": test.users}})
			step, err := manager.pruneUnusedImages(context.Background(), now)
			if err != nil || step.Status != app.CleanupSucceeded || step.ItemsRemoved != 0 || len(store.deleted) != 0 {
				t.Fatalf("changed reference was removed: %+v, deleted=%v, error=%v", step, store.deleted, err)
			}
		})
	}
}

func TestCleanupUnusedPreservesActiveBuilds(t *testing.T) {
	service := &cleanupBuildKitServer{}
	service.active = true
	store := cleanupImages(t, time.Now())
	result, err := newCleanupManager(t, &fakeRunner{}, store, service).CleanupUnused(context.Background())
	if !errors.Is(err, ErrConflict) || service.pruneCalls != 0 || len(store.deleted) != 0 ||
		result.BuildCache.Status != app.CleanupNotRun || result.Images.Status != app.CleanupNotRun {
		t.Fatalf("active build was not protected: result=%+v, error=%v, cache=%d, images=%v", result, err, service.pruneCalls, store.deleted)
	}
}

func TestCleanupUnusedRetainsPartialResultsAndContinuesAfterFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		cacheError error
		imageError error
	}{
		{name: "cache failure", cacheError: errors.New("synthetic cache failure")},
		{name: "image deletion failure", imageError: errors.New("synthetic image deletion failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &cleanupBuildKitServer{
				records:  []*controlapi.UsageRecord{{ID: "synthetic-cache", Size: 4096}},
				pruneErr: test.cacheError,
			}
			store := cleanupImages(t, time.Now())
			if test.imageError != nil {
				store.deleteErr = map[string]error{"docker.io/example/unused:one": test.imageError}
			}
			result, err := newCleanupManager(t, &fakeRunner{}, store, service).CleanupUnused(context.Background())
			if err == nil {
				t.Fatal("cleanup hid a failure")
			}
			if result.BuildCache.ItemsRemoved != 1 || result.Volumes.Status != app.CleanupSucceeded {
				t.Fatalf("cleanup lost partial results or stopped early: %+v", result)
			}
			if test.cacheError != nil {
				if result.BuildCache.Status != app.CleanupFailed || result.BuildCache.Error == "" ||
					result.Images.Status != app.CleanupSucceeded || result.Images.ItemsRemoved != 2 {
					t.Fatalf("independent image cleanup did not finish after a cache failure: %+v", result)
				}
			} else if result.Images.Status != app.CleanupFailed || result.Images.ItemsRemoved != 1 ||
				!strings.Contains(result.Images.Error, "synthetic image deletion failure") {
				t.Fatalf("image failure was not retained beside the other removal: %+v", result.Images)
			}
		})
	}
}

func TestCleanupUnusedCancellationDoesNotStartImageRemoval(t *testing.T) {
	store := cleanupImages(t, time.Now())
	manager := newCleanupManager(t, &fakeRunner{}, store, &cleanupBuildKitServer{wait: true})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result, err := manager.CleanupUnused(ctx)
	if err == nil || len(store.deleted) != 0 || result.Images.Status != app.CleanupNotRun {
		t.Fatalf("canceled cleanup started more deletions: result=%+v, error=%v, deleted=%v", result, err, store.deleted)
	}
}

func TestCleanupUnusedIsScopedToOwnedLimaEngine(t *testing.T) {
	directory := filepath.Join(t.TempDir(), engineInstanceName)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "disk"), []byte("synthetic disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := json.Marshal(map[string]string{"name": engineInstanceName, "status": "Running", "dir": directory})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: map[string][]byte{
		"limactl list porto-engine --json":                                                listing,
		`limactl shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`: []byte("synthetic-owner\n"),
		"limactl shell --workdir=/ porto-engine -- sudo -n fstrim --all --verbose":        []byte("/: 1 MiB (1048576 bytes) trimmed on /dev/vda1\n"),
	}}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.lookPath = func(name string) (string, error) { return name, nil }
	manager.dialBuildKit = buildKitControlTestDialer(t, &cleanupBuildKitServer{})
	manager.imageMetadataConnector = cleanupMetadata(cleanupImages(t, time.Now()), &cleanupContainerStore{})
	if err := manager.writeEngineState(engineState{Mode: "lima", Instance: engineInstanceName, OwnerID: "synthetic-owner"}); err != nil {
		t.Fatal(err)
	}
	result, err := manager.CleanupUnused(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.HostDisk.Status != app.CleanupSucceeded || result.HostDisk.BytesReclaimed == nil ||
		!strings.Contains(result.HostDisk.Output, "1048576 bytes") {
		t.Fatalf("owned engine disk was not trimmed and measured: %+v", result.HostDisk)
	}
}

func TestCleanupUnusedReportsEngineDisksWithoutDiscardAsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk")
	if err := os.WriteFile(path, []byte("synthetic disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(&fakeRunner{errors: map[string]error{
		"limactl shell --workdir=/ porto-engine -- sudo -n fstrim --all --verbose": errors.New("fstrim: /: the discard operation is not supported"),
	}})
	step, err := manager.reclaimHostDisk(context.Background(), hostDiskTarget{instance: engineInstanceName, path: path})
	if err != nil || step.Status != app.CleanupSkipped || step.Error != "" || !strings.Contains(step.Output, "does not support discard") {
		t.Fatalf("unsupported discard was not reported as skipped: %+v, %v", step, err)
	}
}

func TestCleanupUnusedDoesNotInventUnknownCacheSizes(t *testing.T) {
	service := &cleanupBuildKitServer{records: []*controlapi.UsageRecord{{ID: "synthetic-cache", Size: -1}}}
	result, err := newCleanupManager(t, &fakeRunner{}, &cleanupImageStore{}, service).CleanupUnused(context.Background())
	if err != nil || result.BuildCache.ItemsRemoved != 1 || result.BuildCache.BytesReclaimed != nil {
		t.Fatalf("unknown cache size was reported as a known byte count: %+v, %v", result, err)
	}
}

func TestCleanupUnusedBoundsOutputWithoutLosingRemovalCounts(t *testing.T) {
	const references = 2000
	old := time.Now().Add(-72 * time.Hour)
	store := &cleanupImageStore{records: map[string]images.Image{}}
	for index := range references {
		name := fmt.Sprintf("registry.example.test/synthetic/unused-%04d:latest", index)
		store.records[name] = cleanupImageRecord(name, old, old, nil)
	}
	result, err := newCleanupManager(t, &fakeRunner{}, store, &cleanupBuildKitServer{}).CleanupUnused(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Images.ItemsRemoved != references || len(result.Images.Output) > maxCleanupOutput+128 ||
		!strings.Contains(result.Images.Output, "truncated") {
		t.Fatalf("unbounded or inaccurate image output: count=%d, bytes=%d", result.Images.ItemsRemoved, len(result.Images.Output))
	}
}

func TestCleanupUnusedRejectsOverlapBeforeAnyMutation(t *testing.T) {
	manager := New(&fakeRunner{})
	manager.cleanupMu.Lock()
	defer manager.cleanupMu.Unlock()
	if _, err := manager.CleanupUnused(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlapping cleanup = %v, want conflict", err)
	}
}

func TestCleanupRemovesOnlyStandaloneContainersStoppedLongEnough(t *testing.T) {
	now := time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour).Format(time.RFC3339Nano)
	recent := now.Add(-time.Hour).Format(time.RFC3339Nano)
	manager := New(&fakeRunner{})
	manager.SetManagedContainerGuard(func(_ context.Context, name string) error {
		if name == "porto-cluster-node" {
			return errors.New("managed cluster node")
		}
		return nil
	})
	retained := checkpointedContainers([]images.Image{
		{Name: checkpointImagePrefix + "checkpointed-app:before-upgrade"},
		{Name: "registry.example.test/team/custom-checkpoint:v1", Labels: map[string]string{checkpointSourceIDLabel: "custom-checkpointed-app"}},
	})
	retained["sleeping-app"] = true
	cutoff := now.Add(-app.DockerCleanupMinimumAge)
	for _, test := range []struct {
		container Container
		remove    bool
	}{
		{Container{Name: "one-off", State: "exited", ExitAt: old}, true},
		{Container{Name: "one-off-updated-recently", State: "exited", ExitAt: old, UpdatedAt: recent}, false},
		{Container{Name: "recently-stopped", State: "exited", ExitAt: recent}, false},
		{Container{Name: "unknown-stop-time", State: "exited", CreatedAt: old}, false},
		{Container{Name: "compose-service", State: "exited", ExitAt: old, ComposeProject: "app"}, false},
		{Container{Name: "kind-node", State: "exited", ExitAt: old, Labels: map[string]string{"io.x-k8s.kind.cluster": "dev"}}, false},
		{Container{Name: "k3d-node", State: "exited", ExitAt: old, Labels: map[string]string{"k3d.cluster": "dev"}}, false},
		{Container{Name: "minikube", State: "exited", ExitAt: old, Labels: map[string]string{"name.minikube.sigs.k8s.io": "minikube"}}, false},
		{Container{ID: "checkpointed-app", Name: "checkpointed", State: "exited", ExitAt: old}, false},
		{Container{ID: "custom-checkpointed-app", Name: "custom-checkpointed", State: "exited", ExitAt: old}, false},
		{Container{Name: "sleeping-app", State: "exited", ExitAt: old}, false},
		{Container{Name: "porto-cluster-node", State: "exited", ExitAt: old}, false},
		{Container{Name: "transfer-helper", State: "exited", ExitAt: old, Labels: map[string]string{transferOwnerLabel: "synthetic"}}, false},
	} {
		if got := manager.removableStoppedContainer(context.Background(), test.container, retained, cutoff); got != test.remove {
			t.Errorf("%s removable = %t, want %t", test.container.Name, got, test.remove)
		}
	}
	for _, state := range []string{"running", "paused", "created", "restarting"} {
		if stoppedContainer(Container{State: state}) {
			t.Errorf("%s container was treated as stopped", state)
		}
	}
}

func TestCleanupRemovesOnlyUnusedAnonymousVolumes(t *testing.T) {
	generated := strings.Repeat("b", 64)
	attached := strings.Repeat("c", 64)
	var removed []string
	manager := New(&fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		switch {
		case args == "volume ls --format {{json .}}":
			return []byte(strings.Join([]string{
				`{"Name":"` + generated + `","Labels":""}`,
				`{"Name":"porto-0123456789ab","Labels":"nerdctl/anonymous-volumes="}`,
				`{"Name":"` + attached + `","Labels":""}`,
				`{"Name":"app_postgres-data","Labels":"com.docker.compose.project=app,com.docker.compose.volume=postgres-data"}`,
				`{"Name":"manual-data","Labels":""}`,
			}, "\n") + "\n"), nil
		case strings.HasPrefix(args, "ps "):
			return []byte(`{"ID":"stopped-app","Names":"stopped-app","State":"exited","Mounts":"` + attached + `"}` + "\n"), nil
		case strings.HasPrefix(args, "volume inspect "):
			name := strings.TrimPrefix(args, "volume inspect ")
			return []byte(`[{"Name":"` + name + `","Mountpoint":"/var/lib/nerdctl/volumes/default/` + name + `/_data"}]`), nil
		case strings.HasPrefix(args, "volume rm "):
			removed = append(removed, strings.TrimPrefix(args, "volume rm "))
			return nil, nil
		default:
			return nil, errors.New("unexpected command: " + args)
		}
	}})
	step, err := manager.pruneAnonymousVolumes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{generated, "porto-0123456789ab"}; !reflect.DeepEqual(removed, want) || step.ItemsRemoved != 2 {
		t.Fatalf("removed %v (%d), want only unused anonymous volumes %v", removed, step.ItemsRemoved, want)
	}
	if !strings.Contains(step.Output, "Kept 2 named volume(s)") || !strings.Contains(step.Output, "Kept 1 anonymous volume(s)") {
		t.Fatalf("retained volumes were not reported: %s", step.Output)
	}
}
