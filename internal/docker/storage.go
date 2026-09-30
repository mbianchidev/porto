package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	controlapi "github.com/moby/buildkit/api/services/control"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
)

type StorageOwner struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	State          string `json:"state"`
	ComposeProject string `json:"composeProject,omitempty"`
	ComposeService string `json:"composeService,omitempty"`
}

type StorageResource struct {
	Resource       datafiles.Resource `json:"resource"`
	Identity       string             `json:"identity"`
	LogicalBytes   *int64             `json:"logicalBytes,omitempty"`
	AllocatedBytes *int64             `json:"allocatedBytes,omitempty"`
	SharedBytes    *int64             `json:"sharedBytes,omitempty"`
	InUse          bool               `json:"inUse"`
	Dangling       bool               `json:"dangling"`
	Protected      bool               `json:"protected"`
	Reason         string             `json:"reason,omitempty"`
	Owners         []StorageOwner     `json:"owners"`
	Labels         map[string]string  `json:"labels,omitempty"`
}

type StorageUsage struct {
	Resources       []StorageResource `json:"resources"`
	ContentBytes    int64             `json:"contentBytes"`
	SnapshotBytes   int64             `json:"snapshotBytes"`
	WritableBytes   int64             `json:"writableBytes"`
	VolumeBytes     *int64            `json:"volumeBytes,omitempty"`
	BuildCacheBytes *int64            `json:"buildCacheBytes,omitempty"`
	MetadataBytes   *int64            `json:"metadataBytes,omitempty"`
	Warnings        []string          `json:"warnings"`
	Accounting      string            `json:"accounting"`
}

type PrunePreview struct {
	Request    dataops.Request   `json:"request"`
	Candidates []StorageResource `json:"candidates"`
	Excluded   []StorageResource `json:"excluded"`
	UpperBound *int64            `json:"upperBoundBytes,omitempty"`
	Token      string            `json:"token"`
	Message    string            `json:"message"`
}

func (m *Manager) StorageUsage(ctx context.Context) (usage StorageUsage, err error) {
	if m.storageReader != nil {
		return m.storageReader(ctx)
	}
	usage = StorageUsage{
		Resources: make([]StorageResource, 0), Warnings: make([]string, 0),
		Accounting: "Content counts each visible containerd blob once. Snapshot Usage excludes parents and counts each snapshot once. Volume allocated bytes exclude duplicate hardlinks within a volume. Image logical sizes overlap and must not be summed. BuildKit shared records and containerd's shared metadata database are not added to a fabricated grand total.",
	}
	runtimeClient, err := m.runtimeConnector(ctx)
	if err != nil {
		return usage, err
	}
	defer func() { err = errors.Join(err, runtimeClient.Close()) }()
	backend, ok := runtimeClient.(*grpcContainerRuntime)
	if !ok || backend.client == nil {
		return usage, fmt.Errorf("%w: exact storage accounting requires containerd metadata", ErrUnsupported)
	}
	ctx = namespaces.WithNamespace(ctx, backend.namespace)
	containers, err := m.Containers(ctx)
	if err != nil {
		return usage, err
	}
	if snapshot := m.ContainerSnapshot(); snapshot.Stale {
		return usage, fmt.Errorf("%w: storage ownership inventory is stale; reconnect before cleanup", ErrUnavailable)
	}
	blobSizes := make(map[string]int64)
	if err := backend.client.ContentStore().Walk(ctx, func(info content.Info) error {
		if info.Size < 0 {
			return errors.New("containerd reported negative content usage")
		}
		blobSizes[info.Digest.String()] = info.Size
		usage.ContentBytes += info.Size
		return nil
	}); err != nil {
		return usage, err
	}
	nativeImages, err := backend.client.ListImages(ctx)
	if err != nil {
		return usage, err
	}
	imageBlobs := make(map[string][]string)
	references := make(map[string]int)
	for _, image := range nativeImages {
		seen := make(map[string]bool)
		if err := images.Walk(ctx, images.HandlerFunc(func(ctx context.Context, descriptor ocispec.Descriptor) ([]ocispec.Descriptor, error) {
			key := descriptor.Digest.String()
			if !seen[key] {
				seen[key] = true
				imageBlobs[image.Name()] = append(imageBlobs[image.Name()], key)
				references[key]++
			}
			return images.Children(ctx, backend.client.ContentStore(), descriptor)
		}), image.Target()); err != nil {
			return usage, fmt.Errorf("walk image content references: %w", err)
		}
	}
	for _, image := range nativeImages {
		var logical, shared int64
		for _, blob := range imageBlobs[image.Name()] {
			logical += blobSizes[blob]
			if references[blob] > 1 {
				shared += blobSizes[blob]
			}
		}
		resource := datafiles.Resource{
			Kind: "image", Name: image.Name(), ID: image.Target().Digest.String(),
			CreatedAt: image.Metadata().CreatedAt.UTC().Format(time.RFC3339Nano), ReadOnly: true,
			Backend: backend.backend + "/" + backend.namespace,
		}
		item := StorageResource{Resource: resource, LogicalBytes: &logical, SharedBytes: &shared, Owners: make([]StorageOwner, 0), Labels: image.Labels()}
		for _, container := range containers {
			if normalizeNerdctlReference(container.Image) == image.Name() || container.ImageID == resource.ID ||
				container.Labels[nerdctlImageDigestLabel] == resource.ID {
				item.Owners = append(item.Owners, storageOwner(container))
			}
		}
		item.InUse = len(item.Owners) > 0
		item.Dangling = strings.Contains(image.Name(), "@") || strings.HasPrefix(image.Name(), "sha256:")
		finishStorageResource(m, &item)
		usage.Resources = append(usage.Resources, item)
	}
	if err := backend.client.SnapshotService("").Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		value, err := backend.client.SnapshotService("").Usage(ctx, info.Name)
		if err != nil {
			return err
		}
		usage.SnapshotBytes += value.Size
		if info.Kind == snapshots.KindActive {
			usage.WritableBytes += value.Size
		}
		return nil
	}); err != nil {
		return usage, fmt.Errorf("read snapshot allocation: %w", err)
	}
	for _, container := range containers {
		resource := datafiles.Resource{Kind: "container", Name: container.Name, ID: container.ID, CreatedAt: container.CreatedAt, Backend: backend.backend + "/" + backend.namespace}
		item := StorageResource{Resource: resource, Owners: make([]StorageOwner, 0), InUse: container.TaskPresent && containerdStateActive(container.State), Labels: container.Labels}
		item.Protected = managedClusterContainer(container)
		if item.Protected {
			item.Reason = "Managed Kubernetes node; remove it through the cluster lifecycle."
		}
		native, err := backend.client.LoadContainer(ctx, container.ID)
		if err != nil {
			return usage, err
		}
		info, err := native.Info(ctx)
		if err != nil {
			return usage, err
		}
		if info.SnapshotKey != "" {
			allocation, err := backend.client.SnapshotService(info.Snapshotter).Usage(ctx, info.SnapshotKey)
			if err != nil {
				return usage, err
			}
			size := allocation.Size
			item.AllocatedBytes = &size
		}
		finishStorageResource(m, &item)
		usage.Resources = append(usage.Resources, item)
	}
	volumes, err := m.Volumes(ctx)
	if err != nil {
		return usage, err
	}
	var volumeBytes int64
	volumesKnown := true
	for _, volume := range volumes {
		descriptor, err := m.FileDescriptor(ctx, "volume", volume.Name)
		item := StorageResource{Resource: datafiles.Resource{Kind: "volume", Name: volume.Name, ID: volume.Name}, Owners: make([]StorageOwner, 0), Labels: volume.Labels}
		if err != nil {
			item.Protected, item.Reason = true, err.Error()
			volumesKnown = false
		} else {
			item.Resource = descriptor.Resource
			for _, container := range containers {
				if containerUsesVolume(container, volume.Name, descriptor.RootPath) {
					item.Owners = append(item.Owners, storageOwner(container))
				}
			}
			item.InUse = len(item.Owners) > 0
			var result bytes.Buffer
			err = m.RunFileRequest(ctx, descriptor, datafiles.Request{Action: "usage", Identity: descriptor.Resource.Fingerprint()}, nil, &result)
			var measured struct {
				LogicalBytes   int64  `json:"logicalBytes"`
				AllocatedBytes *int64 `json:"allocatedBytes"`
			}
			if err == nil {
				err = json.Unmarshal(result.Bytes(), &measured)
			}
			if err != nil {
				item.Protected, item.Reason = true, err.Error()
				volumesKnown = false
			} else {
				item.LogicalBytes, item.AllocatedBytes = &measured.LogicalBytes, measured.AllocatedBytes
				if measured.AllocatedBytes == nil {
					volumesKnown = false
				} else {
					volumeBytes += *measured.AllocatedBytes
				}
			}
		}
		finishStorageResource(m, &item)
		usage.Resources = append(usage.Resources, item)
	}
	if volumesKnown {
		usage.VolumeBytes = &volumeBytes
	}
	networks, err := m.Networks(ctx)
	if err != nil {
		return usage, err
	}
	for _, network := range networks {
		item := StorageResource{
			Resource: datafiles.Resource{Kind: "network", Name: network.Name, ID: network.ID, CreatedAt: network.Created, Backend: backend.backend + "/" + backend.namespace},
			Owners:   make([]StorageOwner, 0), Labels: network.Labels,
		}
		item.Protected = slices.Contains([]string{"bridge", "host", "none"}, network.Name)
		for _, container := range containers {
			if slices.Contains(strings.Split(container.Networks, ","), network.Name) || slices.ContainsFunc(container.NetworkDetails, func(value ContainerNetworkState) bool { return value.Name == network.Name }) {
				item.Owners = append(item.Owners, storageOwner(container))
			}
		}
		item.InUse = len(item.Owners) > 0
		finishStorageResource(m, &item)
		usage.Resources = append(usage.Resources, item)
	}
	cache, cacheErr := m.BuildCacheUsage(ctx)
	if cacheErr != nil {
		usage.Warnings = append(usage.Warnings, "Build cache unavailable: "+cacheErr.Error())
	} else {
		var cacheBytes int64
		known := true
		for _, item := range cache {
			if item.AllocatedBytes == nil || item.SharedBytes != nil && *item.SharedBytes > 0 {
				known = false
			} else {
				cacheBytes += *item.AllocatedBytes
			}
		}
		if known {
			usage.BuildCacheBytes = &cacheBytes
		}
		usage.Resources = append(usage.Resources, cache...)
	}
	usage.Warnings = append(usage.Warnings, "Containerd metadata is shared across namespaces; its database allocation is not attributed to Porto.")
	return usage, nil
}

func finishStorageResource(manager *Manager, resource *StorageResource) {
	resource.Identity = resource.Resource.Fingerprint()
	if manager.nativeGuard != nil {
		if err := manager.nativeGuard(resource.Resource.Kind, resource.Resource.Name); err != nil {
			resource.Protected, resource.Reason = true, err.Error()
		}
	}
}

func storageOwner(container Container) StorageOwner {
	return StorageOwner{ID: container.ID, Name: container.Name, State: container.State, ComposeProject: container.ComposeProject, ComposeService: container.ComposeService}
}

func containerdStateActive(state string) bool {
	return slices.Contains([]string{"running", "paused", "pausing", "restarting", "created"}, strings.ToLower(state))
}

func managedClusterContainer(container Container) bool {
	return container.Labels["io.x-k8s.kind.cluster"] != "" || container.Labels["io.porto.kubernetes.cluster"] != ""
}

func containerUsesVolume(container Container, name, mountpoint string) bool {
	for _, mounted := range container.MountDetails {
		if mounted.Source == name || mountpoint != "" && (mounted.Source == mountpoint || strings.HasPrefix(mounted.Source, strings.TrimSuffix(mountpoint, "/")+"/")) {
			return true
		}
	}
	return slices.Contains(strings.Split(container.Mounts, ","), name)
}

func (m *Manager) BuildCacheUsage(ctx context.Context) ([]StorageResource, error) {
	connection, err := m.storageBuildKitClient(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	response, err := controlapi.NewControlClient(connection).DiskUsage(ctx, &controlapi.DiskUsageRequest{})
	if err != nil {
		return nil, err
	}
	result := make([]StorageResource, 0, len(response.Record))
	for _, record := range response.Record {
		created := ""
		if record.CreatedAt != nil {
			created = record.CreatedAt.AsTime().UTC().Format(time.RFC3339Nano)
		}
		resource := datafiles.Resource{Kind: "cache", Name: record.ID, ID: record.ID, CreatedAt: created, Backend: "Porto BuildKit"}
		item := StorageResource{Resource: resource, InUse: record.InUse, Owners: make([]StorageOwner, 0)}
		if record.Size >= 0 {
			size := record.Size
			item.LogicalBytes = &size
			if record.Shared {
				item.SharedBytes = &size
			} else {
				item.AllocatedBytes = &size
			}
		}
		if record.InUse {
			item.Protected, item.Reason = true, "BuildKit reports this record in use."
		}
		finishStorageResource(m, &item)
		result = append(result, item)
	}
	return result, nil
}

func (m *Manager) storageBuildKitClient(ctx context.Context) (*grpc.ClientConn, error) {
	backend, err := m.backend(ctx)
	if err != nil {
		return nil, err
	}
	return newBuildKitControlConnection(func(ctx context.Context) (net.Conn, error) { return m.dialBuildKitBackend(ctx, backend) })
}

func (m *Manager) PreviewPrune(ctx context.Context, request dataops.Request) (PrunePreview, error) {
	if len(request.Categories) == 0 && len(request.Selections) == 0 {
		return PrunePreview{}, fmt.Errorf("%w: choose resource categories or individual objects; broad cleanup is not a default", datafiles.ErrInvalid)
	}
	for _, category := range request.Categories {
		if !slices.Contains([]string{"image", "container", "volume", "network", "cache"}, category) {
			return PrunePreview{}, fmt.Errorf("%w: unknown prune category %q", datafiles.ErrInvalid, category)
		}
	}
	usage, err := m.StorageUsage(ctx)
	if err != nil {
		return PrunePreview{}, err
	}
	preview := PrunePreview{
		Request: request, Candidates: make([]StorageResource, 0), Excluded: make([]StorageResource, 0),
		Message: "Only the exact unused objects below can be removed. Shared image/cache layers make byte release uncertain; measured allocations are an upper bound, not a promised reclaimed total.",
	}
	var upper int64
	known := true
	for _, item := range usage.Resources {
		selected := slices.Contains(request.Categories, item.Resource.Kind)
		for _, choice := range request.Selections {
			if choice.Kind == item.Resource.Kind && choice.Name == item.Resource.Name && choice.ID == item.Resource.ID {
				selected = true
			}
		}
		if !selected {
			continue
		}
		if item.InUse || item.Protected {
			preview.Excluded = append(preview.Excluded, item)
		} else {
			preview.Candidates = append(preview.Candidates, item)
			if item.AllocatedBytes == nil {
				known = false
			} else {
				upper += *item.AllocatedBytes
			}
		}
	}
	slices.SortFunc(preview.Candidates, func(left, right StorageResource) int {
		return strings.Compare(left.Resource.Kind+"\x00"+left.Resource.Name, right.Resource.Kind+"\x00"+right.Resource.Name)
	})
	if known {
		preview.UpperBound = &upper
	}
	identities := make([]string, 0, len(preview.Candidates))
	for _, item := range preview.Candidates {
		identities = append(identities, item.Identity)
	}
	document, err := json.Marshal(struct {
		Action     string
		Identities []string
	}{"prune", identities})
	if err != nil {
		return preview, err
	}
	hash := sha256.Sum256(document)
	preview.Token = hex.EncodeToString(hash[:])
	return preview, nil
}

func (m *Manager) Prune(ctx context.Context, request dataops.Request, progress func(string, int64) error) (result dataops.Result, err error) {
	if !request.Confirm || request.Preview == "" {
		return result, fmt.Errorf("%w: scoped cleanup requires a confirmed preview", datafiles.ErrInvalid)
	}
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	if !m.cleanupMu.TryLock() {
		return result, ErrConflict
	}
	defer m.cleanupMu.Unlock()
	preview, err := m.PreviewPrune(ctx, request)
	if err != nil {
		return result, err
	}
	if preview.Token != request.Preview {
		return result, fmt.Errorf("%w: prune candidates changed; preview again", datafiles.ErrConflict)
	}
	connection, err := m.storageBuildKitClient(ctx)
	if err != nil {
		return result, err
	}
	defer connection.Close()
	client := controlapi.NewControlClient(connection)
	if err := ensureBuildKitIdle(ctx, client); err != nil {
		return result, err
	}
	result.Steps = make([]dataops.Step, 0, len(preview.Candidates))
	for _, item := range preview.Candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := progress("Removing "+item.Resource.Kind+" "+item.Resource.Name, 0); err != nil {
			return result, err
		}
		if err := ensureBuildKitIdle(ctx, client); err != nil {
			return result, err
		}
		var removeErr error
		switch item.Resource.Kind {
		case "image":
			removeErr = m.removePreviewedImage(ctx, item.Resource)
		case "container":
			removeErr = m.ContainerAction(ctx, item.Resource.ID, "remove")
		case "volume":
			removeErr = m.RemoveVolume(ctx, item.Resource.Name, false)
		case "network":
			removeErr = m.RemoveNetwork(ctx, item.Resource.Name)
		case "cache":
			stream, err := client.Prune(ctx, &controlapi.PruneRequest{All: true, Filter: []string{"id==" + item.Resource.ID}})
			if err != nil {
				removeErr = err
				break
			}
			for {
				record, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					removeErr = err
					break
				}
				if record.ID != item.Resource.ID || record.InUse {
					removeErr = errors.New("BuildKit prune violated the selected-record boundary")
					break
				}
			}
		}
		step := dataops.Step{Kind: item.Resource.Kind, Source: item.Resource.Name, Status: "succeeded", ID: item.Resource.ID}
		if removeErr != nil {
			step.Status, step.Message = "failed", removeErr.Error()
		}
		result.Steps = append(result.Steps, step)
		if removeErr != nil {
			return result, removeErr
		}
	}
	result.Message = "Removed only previewed unused resources. Actual released bytes were not reported consistently by all stores."
	return result, nil
}

type dataTransactionKey struct{}

func (m *Manager) SetManagedContainerGuard(guard func(context.Context, string) error) {
	m.managedContainerGuard = guard
}

func (m *Manager) removePreviewedImage(ctx context.Context, resource datafiles.Resource) (err error) {
	client, err := m.runtimeConnector(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	backend, ok := client.(*grpcContainerRuntime)
	if !ok || backend.client == nil {
		return ErrUnsupported
	}
	ctx = namespaces.WithNamespace(ctx, backend.namespace)
	image, err := backend.client.GetImage(ctx, resource.Name)
	if err != nil {
		return err
	}
	target := image.Target()
	if target.Digest.String() != resource.ID {
		return datafiles.ErrConflict
	}
	return backend.client.ImageService().Delete(ctx, resource.Name, images.DeleteTarget(&target), images.SynchronousDelete())
}

func (m *Manager) BeginDataTransaction(ctx context.Context) (context.Context, func(), error) {
	if ctx.Value(dataTransactionKey{}) == m {
		return ctx, func() {}, nil
	}
	if !m.dataMu.TryLock() {
		return ctx, nil, fmt.Errorf("%w: a storage transfer or cleanup is already running", ErrConflict)
	}
	return context.WithValue(ctx, dataTransactionKey{}, m), m.dataMu.Unlock, nil
}

func (m *Manager) dataReadGate(ctx context.Context) (context.Context, func(), error) {
	if ctx.Value(dataTransactionKey{}) == m {
		return ctx, func() {}, nil
	}
	if !m.dataMu.TryRLock() {
		return ctx, nil, fmt.Errorf("%w: a staged storage operation owns runtime lifecycle; retry after it completes", ErrConflict)
	}
	return context.WithValue(ctx, dataTransactionKey{}, m), m.dataMu.RUnlock, nil
}
