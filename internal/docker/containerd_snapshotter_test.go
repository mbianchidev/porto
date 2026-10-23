package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	diffapi "github.com/containerd/containerd/api/services/diff/v1"
	imagesapi "github.com/containerd/containerd/api/services/images/v1"
	introspectionapi "github.com/containerd/containerd/api/services/introspection/v1"
	namespacesapi "github.com/containerd/containerd/api/services/namespaces/v1"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	apitypes "github.com/containerd/containerd/api/types"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

const snapshotterTestNamespace = "synthetic-snapshotter"

func TestContainerdBootstrapRPCsHaveIndependentBoundedDeadlines(t *testing.T) {
	deadlines := make(chan time.Time, 2)
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "bootstrap RPC has no deadline")
		}
		deadlines <- deadline
		if strings.HasSuffix(info.FullMethod, "/Check") {
			time.Sleep(100 * time.Millisecond)
		}
		return handler(ctx, request)
	}))
	healthService := health.NewServer()
	healthService.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(server, healthService)
	namespacesapi.RegisterNamespacesServer(server, &snapshotterNamespaceServer{})
	go func() {
		if err := server.Serve(listener); err != nil {
			t.Errorf("serve synthetic bootstrap: %v", err)
		}
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtimeClient, err := newGRPCContainerRuntime(
		ctx, snapshotterTestNamespace, "synthetic bootstrap",
		func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) },
		nil, t.TempDir(), &fakeRunner{}, "", "", nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeClient.Close()
	healthDeadline, namespaceDeadline := <-deadlines, <-deadlines
	if namespaceDeadline.Sub(healthDeadline) < 75*time.Millisecond {
		t.Fatalf("namespace discovery inherited the health/connection handshake's consumed deadline: health=%s namespace=%s", healthDeadline, namespaceDeadline)
	}
	if time.Until(namespaceDeadline) > 10*time.Second {
		t.Fatal("namespace RPC exceeded its original ten-second validation cap")
	}
}

func TestContainerdSnapshotterSelectionAndCleanup(t *testing.T) {
	for _, test := range []struct {
		name        string
		lima        string
		label       string
		snapshotter string
		removeErr   error
		wantError   string
	}{
		{name: "Lima uses Linux default", lima: "synthetic-engine", snapshotter: "overlayfs"},
		{name: "Lima honors namespace", lima: "synthetic-engine", label: "native", snapshotter: "native"},
		{name: "explicit EROFS is preserved", lima: "synthetic-engine", label: "erofs", snapshotter: "erofs"},
		{name: "local default is preserved", snapshotter: defaults.DefaultSnapshotter},
		{name: "local namespace is preserved", label: "native", snapshotter: "native"},
		{
			name: "absent snapshot is already clean", lima: "synthetic-engine", snapshotter: "overlayfs",
			removeErr: status.Error(codes.NotFound, "synthetic snapshot is absent"),
		},
		{
			name: "cleanup failure is reported", lima: "synthetic-engine", snapshotter: "overlayfs",
			removeErr: status.Error(codes.PermissionDenied, "synthetic cleanup failure"),
			wantError: "synthetic cleanup failure",
		},
		{
			name: "unavailable configured snapshotter is not replaced", lima: "synthetic-engine",
			label: "erofs", snapshotter: "erofs",
			removeErr: status.Error(codes.FailedPrecondition, "EROFS snapshotter is unavailable"),
			wantError: "EROFS snapshotter is unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &snapshotterTestServer{snapshotter: test.snapshotter, removeErr: test.removeErr}
			runtimeClient := newSnapshotterTestRuntime(t, test.lima, test.label, service)
			err := runtimeClient.cleanupDirectSnapshot(context.Background(), "synthetic-snapshot")
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("cleanup error = %v, want %q", err, test.wantError)
				}
			} else if err != nil {
				t.Fatalf("cleanup: %v", err)
			}
			service.mu.Lock()
			defer service.mu.Unlock()
			if want := []string{test.snapshotter}; !reflect.DeepEqual(service.removed, want) {
				t.Fatalf("cleanup snapshotters = %q, want %q", service.removed, want)
			}
		})
	}
}

func TestLimaImageUnpackAndCreateUseBackendSnapshotter(t *testing.T) {
	for _, label := range []string{"", "native", "erofs"} {
		for _, unpacked := range []bool{false, true} {
			t.Run(fmt.Sprintf("namespace=%q/unpacked=%t", label, unpacked), func(t *testing.T) {
				snapshotter := firstNonEmpty(label, "overlayfs")
				service := &snapshotterTestServer{snapshotter: snapshotter, unpacked: unpacked}
				runtimeClient := newSnapshotterTestRuntime(t, "synthetic-engine", label, service)
				fixture := seedSnapshotterTestImage(t, runtimeClient, service)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := runtimeClient.resolveContainerImage(ctx, "synthetic:latest", "linux/arm64"); err != nil {
					t.Fatalf("resolve synthetic image: %v", err)
				}
				if !unpacked {
					info, err := fixture.store.Info(ctx, fixture.config.Digest)
					if err != nil {
						t.Fatal(err)
					}
					if got := info.Labels["containerd.io/gc.ref.snapshot."+snapshotter]; got != fixture.layer.Digest.String() {
						t.Fatalf("unpacked snapshot reference = %q, want %q", got, fixture.layer.Digest)
					}
				}
				id, err := runtimeClient.Create(ctx, CreateContainerRequest{
					Image: "synthetic:latest", Platform: "linux/arm64",
					Networks: []ContainerNetwork{{Name: directNetworkNone}},
				})
				if err != nil {
					t.Fatalf("create synthetic container: %v", err)
				}
				service.mu.Lock()
				defer service.mu.Unlock()
				if service.container.GetID() != id || service.container.GetSnapshotter() != snapshotter ||
					service.container.GetSnapshotKey() != "porto-"+id {
					t.Fatalf("container snapshot metadata = %v", service.container)
				}
				if len(service.prepared) == 0 || service.prepared[len(service.prepared)-1] != "porto-"+id {
					t.Fatalf("prepared snapshots = %q", service.prepared)
				}
			})
		}
	}
}

func TestLimaImagePullUsesBackendSnapshotter(t *testing.T) {
	service := &snapshotterTestServer{snapshotter: "overlayfs"}
	runtimeClient := newSnapshotterTestRuntime(t, "synthetic-engine", "", service)
	fixture := seedSnapshotterTestImage(t, runtimeClient, service)
	service.mu.Lock()
	service.image = nil
	service.mu.Unlock()
	registry := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v2/" {
			response.WriteHeader(http.StatusOK)
			return
		}
		if request.URL.Path != "/v2/synthetic/manifests/latest" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
		response.Header().Set("Docker-Content-Digest", fixture.manifest.Digest.String())
		response.Header().Set("Content-Length", fmt.Sprint(len(fixture.manifestData)))
		if request.Method != http.MethodHead {
			if _, err := response.Write(fixture.manifestData); err != nil {
				t.Errorf("serve synthetic image: %v", err)
			}
		}
	}))
	defer registry.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reference := strings.TrimPrefix(registry.URL, "http://") + "/synthetic:latest"
	image, err := runtimeClient.resolveContainerImage(ctx, reference, "linux/arm64")
	if err != nil {
		t.Fatalf("pull synthetic image: %v", err)
	}
	if image.Name() != reference {
		t.Fatalf("pulled image = %q, want %q", image.Name(), reference)
	}
	info, err := fixture.store.Info(ctx, fixture.config.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Labels["containerd.io/gc.ref.snapshot.overlayfs"]; got != fixture.layer.Digest.String() {
		t.Fatalf("pulled snapshot reference = %q, want %q", got, fixture.layer.Digest)
	}
}

type snapshotterTestServer struct {
	snapshotsapi.UnimplementedSnapshotsServer
	mu          sync.Mutex
	snapshotter string
	removed     []string
	removeErr   error
	unpacked    bool
	prepared    []string
	image       *imagesapi.Image
	container   *containersapi.Container
	layer       *apitypes.Descriptor
	usage       map[string]*snapshotsapi.UsageResponse
}

func (s *snapshotterTestServer) check(ctx context.Context, snapshotter string) error {
	incoming, _ := metadata.FromIncomingContext(ctx)
	if got := incoming.Get(containerdNamespaceHeader); !reflect.DeepEqual(got, []string{snapshotterTestNamespace}) {
		return status.Errorf(codes.InvalidArgument, "snapshot namespace = %q", got)
	}
	if snapshotter != s.snapshotter {
		return status.Errorf(codes.FailedPrecondition, "snapshotter %q is unavailable", snapshotter)
	}
	return nil
}

func (s *snapshotterTestServer) Stat(ctx context.Context, request *snapshotsapi.StatSnapshotRequest) (*snapshotsapi.StatSnapshotResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx, request.GetSnapshotter()); err != nil {
		return nil, err
	}
	if !s.unpacked {
		return nil, status.Error(codes.NotFound, "synthetic image is not unpacked")
	}
	return &snapshotsapi.StatSnapshotResponse{Info: &snapshotsapi.Info{
		Name: request.GetKey(), Kind: snapshotsapi.Kind_COMMITTED,
	}}, nil
}

func (s *snapshotterTestServer) List(request *snapshotsapi.ListSnapshotsRequest, stream snapshotsapi.Snapshots_ListServer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(stream.Context(), request.GetSnapshotter()); err != nil {
		return err
	}
	infos := make([]*snapshotsapi.Info, 0, len(s.usage))
	for name := range s.usage {
		kind := snapshotsapi.Kind_COMMITTED
		if name == "synthetic-write" {
			kind = snapshotsapi.Kind_ACTIVE
		}
		infos = append(infos, &snapshotsapi.Info{Name: name, Kind: kind})
	}
	return stream.Send(&snapshotsapi.ListSnapshotsResponse{Info: infos})
}

func (s *snapshotterTestServer) Usage(ctx context.Context, request *snapshotsapi.UsageRequest) (*snapshotsapi.UsageResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx, request.GetSnapshotter()); err != nil {
		return nil, err
	}
	value, ok := s.usage[request.GetKey()]
	if !ok {
		return nil, status.Error(codes.NotFound, "synthetic snapshot is absent")
	}
	return value, nil
}

func (s *snapshotterTestServer) Prepare(ctx context.Context, request *snapshotsapi.PrepareSnapshotRequest) (*snapshotsapi.PrepareSnapshotResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx, request.GetSnapshotter()); err != nil {
		return nil, err
	}
	s.prepared = append(s.prepared, request.GetKey())
	return &snapshotsapi.PrepareSnapshotResponse{Mounts: []*apitypes.Mount{
		{Type: "bind", Source: "/synthetic/rootfs", Options: []string{"rw"}},
	}}, nil
}

func (s *snapshotterTestServer) Commit(ctx context.Context, request *snapshotsapi.CommitSnapshotRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx, request.GetSnapshotter()); err != nil {
		return nil, err
	}
	s.unpacked = true
	return &emptypb.Empty{}, nil
}

func (s *snapshotterTestServer) Remove(ctx context.Context, request *snapshotsapi.RemoveSnapshotRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removed = append(s.removed, request.GetSnapshotter())
	if err := s.check(ctx, request.GetSnapshotter()); err != nil {
		return nil, err
	}
	if s.removeErr != nil {
		return nil, s.removeErr
	}
	return &emptypb.Empty{}, nil
}

type snapshotterNamespaceServer struct {
	namespacesapi.UnimplementedNamespacesServer
	label string
}

func (s *snapshotterNamespaceServer) List(context.Context, *namespacesapi.ListNamespacesRequest) (*namespacesapi.ListNamespacesResponse, error) {
	return &namespacesapi.ListNamespacesResponse{Namespaces: []*namespacesapi.Namespace{
		{Name: "other-namespace", Labels: map[string]string{defaults.DefaultSnapshotterNSLabel: "other-snapshotter"}},
		{Name: snapshotterTestNamespace, Labels: map[string]string{defaults.DefaultSnapshotterNSLabel: s.label}},
	}}, nil
}

func (s *snapshotterNamespaceServer) Get(_ context.Context, request *namespacesapi.GetNamespaceRequest) (*namespacesapi.GetNamespaceResponse, error) {
	if request.GetName() != snapshotterTestNamespace {
		return nil, status.Error(codes.NotFound, "synthetic namespace is absent")
	}
	return &namespacesapi.GetNamespaceResponse{Namespace: &namespacesapi.Namespace{
		Name: snapshotterTestNamespace, Labels: map[string]string{defaults.DefaultSnapshotterNSLabel: s.label},
	}}, nil
}

type snapshotterImageServer struct {
	imagesapi.UnimplementedImagesServer
	state *snapshotterTestServer
}

func (s *snapshotterImageServer) List(context.Context, *imagesapi.ListImagesRequest) (*imagesapi.ListImagesResponse, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	response := &imagesapi.ListImagesResponse{}
	if s.state.image != nil {
		response.Images = []*imagesapi.Image{s.state.image}
	}
	return response, nil
}

func (s *snapshotterImageServer) Get(_ context.Context, request *imagesapi.GetImageRequest) (*imagesapi.GetImageResponse, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if s.state.image == nil || s.state.image.GetName() != request.GetName() {
		return nil, status.Error(codes.NotFound, "synthetic image is absent")
	}
	return &imagesapi.GetImageResponse{Image: s.state.image}, nil
}

func (s *snapshotterImageServer) Create(_ context.Context, request *imagesapi.CreateImageRequest) (*imagesapi.CreateImageResponse, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	s.state.image = request.GetImage()
	return &imagesapi.CreateImageResponse{Image: s.state.image}, nil
}

type snapshotterContainerServer struct {
	containersapi.UnimplementedContainersServer
	state *snapshotterTestServer
}

func (s *snapshotterContainerServer) List(context.Context, *containersapi.ListContainersRequest) (*containersapi.ListContainersResponse, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	response := &containersapi.ListContainersResponse{}
	if s.state.container != nil {
		response.Containers = []*containersapi.Container{s.state.container}
	}
	return response, nil
}

func (s *snapshotterContainerServer) Get(_ context.Context, request *containersapi.GetContainerRequest) (*containersapi.GetContainerResponse, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if s.state.container == nil || request.ID != s.state.container.ID {
		return nil, status.Error(codes.NotFound, "synthetic container is absent")
	}
	return &containersapi.GetContainerResponse{Container: s.state.container}, nil
}

type snapshotterTaskServer struct {
	tasksapi.UnimplementedTasksServer
}

func (*snapshotterTaskServer) List(context.Context, *tasksapi.ListTasksRequest) (*tasksapi.ListTasksResponse, error) {
	return &tasksapi.ListTasksResponse{}, nil
}

func (s *snapshotterContainerServer) Create(_ context.Context, request *containersapi.CreateContainerRequest) (*containersapi.CreateContainerResponse, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	s.state.container = request.GetContainer()
	return &containersapi.CreateContainerResponse{Container: s.state.container}, nil
}

type snapshotterDiffServer struct {
	diffapi.UnimplementedDiffServer
	state *snapshotterTestServer
}

func (s *snapshotterDiffServer) Apply(context.Context, *diffapi.ApplyRequest) (*diffapi.ApplyResponse, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return &diffapi.ApplyResponse{Applied: s.state.layer}, nil
}

type snapshotterIntrospectionServer struct {
	introspectionapi.UnimplementedIntrospectionServer
	snapshotter string
}

func (s *snapshotterIntrospectionServer) Plugins(_ context.Context, request *introspectionapi.PluginsRequest) (*introspectionapi.PluginsResponse, error) {
	if !reflect.DeepEqual(request.GetFilters(), []string{"type==io.containerd.snapshotter.v1, id==" + s.snapshotter}) {
		return &introspectionapi.PluginsResponse{}, nil
	}
	return &introspectionapi.PluginsResponse{Plugins: []*introspectionapi.Plugin{{
		Type: "io.containerd.snapshotter.v1", ID: s.snapshotter,
		Platforms: []*apitypes.Platform{{OS: "linux", Architecture: "arm64"}},
	}}}, nil
}

type snapshotterLeaseManager struct {
	leases.Manager
}

func (*snapshotterLeaseManager) Create(_ context.Context, options ...leases.Opt) (leases.Lease, error) {
	lease := leases.Lease{}
	for _, option := range options {
		if err := option(&lease); err != nil {
			return leases.Lease{}, err
		}
	}
	return lease, nil
}

func (*snapshotterLeaseManager) Delete(context.Context, leases.Lease, ...leases.DeleteOpt) error {
	return nil
}

func newSnapshotterTestRuntime(
	t *testing.T,
	lima, label string,
	service *snapshotterTestServer,
) *grpcContainerRuntime {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	healthService := health.NewServer()
	healthService.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(server, healthService)
	namespacesapi.RegisterNamespacesServer(server, &snapshotterNamespaceServer{label: label})
	snapshotsapi.RegisterSnapshotsServer(server, service)
	imagesapi.RegisterImagesServer(server, &snapshotterImageServer{state: service})
	containersapi.RegisterContainersServer(server, &snapshotterContainerServer{state: service})
	tasksapi.RegisterTasksServer(server, &snapshotterTaskServer{})
	diffapi.RegisterDiffServer(server, &snapshotterDiffServer{state: service})
	introspectionapi.RegisterIntrospectionServer(server, &snapshotterIntrospectionServer{snapshotter: service.snapshotter})
	go func() {
		if err := server.Serve(listener); err != nil {
			t.Errorf("serve synthetic containerd: %v", err)
		}
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runtimeClient, err := newGRPCContainerRuntime(
		ctx, snapshotterTestNamespace, "synthetic containerd",
		func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) },
		nil, t.TempDir(), &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
			if command.Args[len(command.Args)-1] == "path" {
				return []byte("/synthetic/porto-runtime-helper\n"), nil
			}
			return []byte(`{"OSType":"linux","Architecture":"arm64","CgroupDriver":"systemd","SecurityOptions":["name=rootless"]}`), nil
		}}, lima, "", nil, nil,
	)
	if err != nil {
		t.Fatal(fmt.Errorf("connect synthetic containerd: %w", err))
	}
	t.Cleanup(func() {
		if runtimeClient.connection.GetState() != connectivity.Shutdown {
			if err := runtimeClient.Close(); err != nil {
				t.Errorf("close synthetic containerd: %v", err)
			}
		}
	})
	return runtimeClient
}

type snapshotterImageFixture struct {
	store        content.Store
	config       ocispec.Descriptor
	layer        ocispec.Descriptor
	manifest     ocispec.Descriptor
	manifestData []byte
}

type snapshotterLabelStore struct {
	mu     sync.Mutex
	labels map[digest.Digest]map[string]string
}

func (s *snapshotterLabelStore) Get(digest digest.Digest) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneStringMap(s.labels[digest]), nil
}

func (s *snapshotterLabelStore) Set(digest digest.Digest, labels map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.labels[digest] = cloneStringMap(labels)
	return nil
}

func (s *snapshotterLabelStore) Update(digest digest.Digest, labels map[string]string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.labels[digest] == nil {
		s.labels[digest] = map[string]string{}
	}
	for key, value := range labels {
		if value == "" {
			delete(s.labels[digest], key)
		} else {
			s.labels[digest][key] = value
		}
	}
	return cloneStringMap(s.labels[digest]), nil
}

func seedSnapshotterTestImage(t *testing.T, runtimeClient *grpcContainerRuntime, service *snapshotterTestServer) snapshotterImageFixture {
	t.Helper()
	store, err := local.NewLabeledStore(t.TempDir(), &snapshotterLabelStore{labels: map[digest.Digest]map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	writeBlob := func(mediaType string, data []byte) ocispec.Descriptor {
		t.Helper()
		descriptor := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
		if err := content.WriteBlob(context.Background(), store, descriptor.Digest.String(), bytes.NewReader(data), descriptor); err != nil {
			t.Fatal(err)
		}
		return descriptor
	}
	var layerData bytes.Buffer
	archive := tar.NewWriter(&layerData)
	fileData := []byte("synthetic snapshotter fixture\n")
	if err := archive.WriteHeader(&tar.Header{Name: "synthetic.txt", Mode: 0o644, Size: int64(len(fileData))}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(fileData); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	layer := writeBlob(ocispec.MediaTypeImageLayer, layerData.Bytes())
	configData, err := json.Marshal(ocispec.Image{
		Platform: ocispec.Platform{OS: "linux", Architecture: "arm64"},
		Config:   ocispec.ImageConfig{Cmd: []string{"true"}},
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{layer.Digest}},
	})
	if err != nil {
		t.Fatal(err)
	}
	config := writeBlob(ocispec.MediaTypeImageConfig, configData)
	manifestData, err := json.Marshal(ocispec.Manifest{
		Versioned: imagespec.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
		Config: config, Layers: []ocispec.Descriptor{layer},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := writeBlob(ocispec.MediaTypeImageManifest, manifestData)
	service.mu.Lock()
	service.layer = &apitypes.Descriptor{MediaType: layer.MediaType, Digest: layer.Digest.String(), Size: layer.Size}
	service.image = &imagesapi.Image{
		Name:   "docker.io/library/synthetic:latest",
		Target: &apitypes.Descriptor{MediaType: manifest.MediaType, Digest: manifest.Digest.String(), Size: manifest.Size},
	}
	service.mu.Unlock()
	client, err := containerd.NewWithConn(runtimeClient.connection,
		containerd.WithDefaultNamespace(snapshotterTestNamespace),
		containerd.WithServices(
			containerd.WithContentStore(store),
			containerd.WithLeasesService(&snapshotterLeaseManager{}),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	runtimeClient.client = client
	return snapshotterImageFixture{store: store, config: config, layer: layer, manifest: manifest, manifestData: manifestData}
}
