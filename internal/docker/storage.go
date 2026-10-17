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
	"maps"
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
	"github.com/mbianchidev/porto/internal/runtimefiles"
	controlapi "github.com/moby/buildkit/api/services/control"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
)

type StorageOwner struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	State          string            `json:"state"`
	ComposeProject string            `json:"composeProject,omitempty"`
	ComposeService string            `json:"composeService,omitempty"`
	Managed        bool              `json:"managed"`
	Mounts         []datafiles.Mount `json:"mounts,omitempty"`
	Protection     string            `json:"protection,omitempty"`
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
	Description    string             `json:"description,omitempty"`
	CacheType      string             `json:"cacheType,omitempty"`
	LastUsedAt     string             `json:"lastUsedAt,omitempty"`
	UsageCount     int64              `json:"usageCount,omitempty"`
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
	runtimeClient, closeRuntime, err := m.storageRuntime(ctx)
	if err != nil {
		return usage, err
	}
	defer func() { err = errors.Join(err, closeRuntime()) }()
	backend, ok := runtimeClient.(*grpcContainerRuntime)
	if !ok || backend.client == nil {
		return usage, fmt.Errorf("%w: exact storage accounting requires containerd metadata", ErrUnsupported)
	}
	ctx = namespaces.WithNamespace(ctx, backend.namespace)
	containers, err := backend.Snapshot(ctx)
	if err != nil {
		return usage, err
	}
	for _, container := range containers {
		if container.InventoryError != "" {
			return usage, fmt.Errorf("%w: cannot establish storage ownership for %s: %s", ErrUnavailable, container.Name, container.InventoryError)
		}
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
	incompleteImages := make(map[string]bool)
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
			if _, stored := blobSizes[key]; !stored {
				incompleteImages[image.Name()] = true
				return nil, nil
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
		if incompleteImages[image.Name()] {
			item.Protected = true
			item.Reason = "Some image platforms are not downloaded. Sizes include only local packed blobs; incomplete content references are protected from cleanup."
		}
		for _, container := range containers {
			if normalizeNerdctlReference(container.Image) == image.Name() || container.ImageID == resource.ID ||
				container.Labels[nerdctlImageDigestLabel] == resource.ID ||
				slices.Contains(imageBlobs[image.Name()], container.ImageID) ||
				slices.Contains(imageBlobs[image.Name()], container.Labels[nerdctlImageDigestLabel]) {
				item.Owners = append(item.Owners, storageOwner(container))
			}
		}
		item.InUse = len(item.Owners) > 0
		item.Dangling = strings.HasPrefix(image.Name(), buildKitMobyDanglingPrefix+"@") ||
			strings.HasPrefix(image.Name(), "docker.io/library/"+buildKitMobyDanglingPrefix+"@") ||
			strings.HasPrefix(image.Name(), "sha256:")
		finishStorageResource(m, &item)
		usage.Resources = append(usage.Resources, item)
	}
	type containerSnapshot struct{ snapshotter, key string }
	containerSnapshots := make(map[string]containerSnapshot)
	snapshotters := map[string]bool{backend.snapshotter: true}
	for _, container := range containers {
		native, err := backend.client.LoadContainer(ctx, container.ID)
		if err != nil {
			return usage, err
		}
		info, err := native.Info(ctx)
		if err != nil {
			return usage, err
		}
		if info.SnapshotKey != "" {
			snapshotter := firstNonEmpty(info.Snapshotter, backend.snapshotter)
			snapshotters[snapshotter] = true
			containerSnapshots[container.ID] = containerSnapshot{snapshotter, info.SnapshotKey}
		}
	}
	allocations := make(map[string]map[string]int64)
	for _, snapshotter := range slices.Sorted(maps.Keys(snapshotters)) {
		store := backend.client.SnapshotService(snapshotter)
		allocations[snapshotter] = make(map[string]int64)
		if err := store.Walk(ctx, func(_ context.Context, info snapshots.Info) error {
			value, err := store.Usage(ctx, info.Name)
			if err != nil {
				return err
			}
			if value.Size < 0 {
				return errors.New("containerd reported negative snapshot allocation")
			}
			allocations[snapshotter][info.Name] = value.Size
			usage.SnapshotBytes += value.Size
			if info.Kind == snapshots.KindActive {
				usage.WritableBytes += value.Size
			}
			return nil
		}); err != nil {
			return usage, fmt.Errorf("read %s snapshot allocation: %w", snapshotter, err)
		}
	}
	for _, container := range containers {
		resource := datafiles.Resource{Kind: "container", Name: container.Name, ID: container.ID, CreatedAt: container.CreatedAt, Backend: backend.backend + "/" + backend.namespace}
		item := StorageResource{Resource: resource, Owners: make([]StorageOwner, 0), InUse: container.TaskPresent && containerdStateActive(container.State), Labels: container.Labels}
		item.Protected = managedClusterContainer(container)
		if item.Protected {
			item.Reason = "Managed Kubernetes node; remove it through the cluster lifecycle."
		}
		if snapshot, exists := containerSnapshots[container.ID]; exists {
			size, exists := allocations[snapshot.snapshotter][snapshot.key]
			if !exists {
				return usage, fmt.Errorf("%w: container snapshot changed during storage measurement", ErrConflict)
			}
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
			item, err = m.measureVolumeStorage(ctx, descriptor, containers)
			item.Labels = volume.Labels
			if err != nil {
				item.Protected, item.Reason = true, err.Error()
				volumesKnown = false
			} else {
				if item.AllocatedBytes == nil {
					volumesKnown = false
				} else {
					volumeBytes += *item.AllocatedBytes
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
			if item.SharedBytes != nil {
				continue
			}
			if item.AllocatedBytes == nil {
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
	descriptor := runtimefiles.Descriptor{
		Resource: datafiles.Resource{Kind: "runtime", Name: "containerd-metadata", ID: backend.address, ReadOnly: true},
		Address:  backend.address, Namespace: backend.namespace,
	}
	var metadata bytes.Buffer
	metadataErr := m.RunFileRequest(ctx, descriptor, datafiles.Request{Action: "metadata-usage", Identity: descriptor.Resource.Fingerprint()}, nil, &metadata)
	if metadataErr == nil {
		var measured struct {
			AllocatedBytes *int64 `json:"allocatedBytes"`
		}
		metadataErr = json.Unmarshal(metadata.Bytes(), &measured)
		if metadataErr == nil {
			usage.MetadataBytes = measured.AllocatedBytes
		}
	}
	if metadataErr != nil {
		usage.Warnings = append(usage.Warnings, "Runtime metadata allocation unavailable: "+metadataErr.Error())
	}
	usage.Warnings = append(usage.Warnings, "Containerd metadata allocation covers all namespaces, excludes content and snapshots, and is not added to namespace-specific totals.")
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

func (m *Manager) storageRuntime(ctx context.Context) (containerRuntime, func() error, error) {
	if inventory := m.activeContainerInventory(); inventory != nil {
		if client := inventory.activeRuntime(); client != nil {
			return client, func() error { return nil }, nil
		}
	}
	client, err := m.runtimeConnector(ctx)
	if err != nil {
		return nil, nil, err
	}
	return client, client.Close, nil
}

func storageOwner(container Container) StorageOwner {
	return StorageOwner{ID: container.ID, Name: container.Name, State: container.State, ComposeProject: container.ComposeProject, ComposeService: container.ComposeService, Managed: managedClusterContainer(container)}
}

func (m *Manager) storageContainers(ctx context.Context) ([]Container, error) {
	if inventory := m.activeContainerInventory(); inventory != nil {
		client := inventory.activeRuntime()
		if client == nil {
			return nil, fmt.Errorf("%w: current container ownership is unavailable", ErrUnavailable)
		}
		containers, err := client.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		for _, container := range containers {
			if container.InventoryError != "" {
				return nil, fmt.Errorf("%w: ownership of %s could not be established: %s", ErrUnavailable, container.Name, container.InventoryError)
			}
		}
		return containers, nil
	}
	return m.Containers(ctx)
}

func (m *Manager) volumeOwners(ctx context.Context, descriptor runtimefiles.Descriptor) ([]StorageOwner, error) {
	containers, err := m.storageContainers(ctx)
	if err != nil {
		return nil, err
	}
	return m.volumeOwnersFromContainers(ctx, descriptor, containers), nil
}

func (m *Manager) volumeOwnersFromContainers(ctx context.Context, descriptor runtimefiles.Descriptor, containers []Container) []StorageOwner {
	owners := make([]StorageOwner, 0)
	for _, container := range containers {
		if !containerUsesVolume(container, descriptor.Resource.Name, descriptor.RootPath) {
			continue
		}
		owner := storageOwner(container)
		if m.managedContainerGuard != nil {
			if err := m.managedContainerGuard(ctx, container.Name); err != nil {
				owner.Managed, owner.Protection = true, err.Error()
			}
		}
		for _, mounted := range container.MountDetails {
			if mounted.Source == descriptor.Resource.Name || mounted.Source == descriptor.RootPath ||
				strings.HasPrefix(mounted.Source, strings.TrimSuffix(descriptor.RootPath, "/")+"/") {
				owner.Mounts = append(owner.Mounts, datafiles.Mount{
					Path: mounted.Destination, Kind: "volume", Source: mounted.Source, ReadOnly: slices.Contains(mounted.Options, "ro"),
				})
			}
		}
		owners = append(owners, owner)
	}
	return owners
}

func (m *Manager) VolumeStorage(ctx context.Context, name string) (StorageResource, error) {
	descriptor, err := m.FileDescriptor(ctx, "volume", name)
	if err != nil {
		return StorageResource{}, err
	}
	containers, err := m.storageContainers(ctx)
	if err != nil {
		return StorageResource{}, err
	}
	return m.measureVolumeStorage(ctx, descriptor, containers)
}

func (m *Manager) measureVolumeStorage(ctx context.Context, descriptor runtimefiles.Descriptor, containers []Container) (StorageResource, error) {
	item := StorageResource{Resource: descriptor.Resource, Owners: m.volumeOwnersFromContainers(ctx, descriptor, containers)}
	item.InUse = len(item.Owners) > 0
	for _, owner := range item.Owners {
		if owner.Managed {
			item.Protected, item.Reason = true, firstNonEmpty(owner.Protection, "Managed Kubernetes node data; use its cluster lifecycle.")
		}
	}
	finishStorageResource(m, &item)
	var result bytes.Buffer
	if err := m.RunFileRequest(ctx, descriptor, datafiles.Request{Action: "usage", Identity: item.Identity}, nil, &result); err != nil {
		return item, err
	}
	var measured struct {
		LogicalBytes   int64  `json:"logicalBytes"`
		AllocatedBytes *int64 `json:"allocatedBytes"`
	}
	if err := json.Unmarshal(result.Bytes(), &measured); err != nil {
		return item, err
	}
	if measured.LogicalBytes < 0 || measured.AllocatedBytes != nil && *measured.AllocatedBytes < 0 {
		return item, errors.New("runtime helper reported negative volume allocation")
	}
	item.LogicalBytes, item.AllocatedBytes = &measured.LogicalBytes, measured.AllocatedBytes
	return item, nil
}

func (m *Manager) validateVolumeMutation(ctx context.Context, descriptor runtimefiles.Descriptor, remove bool) error {
	if m.nativeGuard != nil {
		if err := m.nativeGuard("volume", descriptor.Resource.Name); err != nil {
			return err
		}
	}
	owners, err := m.volumeOwners(ctx, descriptor)
	if err != nil {
		return err
	}
	for _, owner := range owners {
		if owner.Managed {
			return fmt.Errorf("%w: volume belongs to managed Kubernetes node %s; %s", ErrConflict, owner.Name, firstNonEmpty(owner.Protection, "use its cluster lifecycle"))
		}
		if remove || containerdStateActive(owner.State) {
			return fmt.Errorf("%w: volume is referenced by %s (%s); stop writers before replacing data, or detach all mounts before removal", ErrConflict, owner.Name, owner.State)
		}
	}
	return nil
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
		item := StorageResource{
			Resource: resource, InUse: record.InUse, Owners: make([]StorageOwner, 0),
			Description: record.Description, CacheType: record.RecordType, UsageCount: record.UsageCount,
		}
		if record.LastUsedAt != nil {
			item.LastUsedAt = record.LastUsedAt.AsTime().UTC().Format(time.RFC3339Nano)
		}
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
	for _, choice := range request.Selections {
		if !slices.Contains([]string{"image", "container", "volume", "network", "cache"}, choice.Kind) ||
			choice.Name == "" || choice.ID == "" {
			return PrunePreview{}, fmt.Errorf("%w: cleanup selections require a supported kind, name and exact ID", datafiles.ErrInvalid)
		}
	}
	usage, err := m.StorageUsage(ctx)
	if err != nil {
		return PrunePreview{}, err
	}
	for _, choice := range request.Selections {
		if !slices.ContainsFunc(usage.Resources, func(item StorageResource) bool {
			return choice.Kind == item.Resource.Kind && choice.Name == item.Resource.Name && choice.ID == item.Resource.ID
		}) {
			return PrunePreview{}, fmt.Errorf("%w: selected %s %s no longer has the previewed identity", datafiles.ErrConflict, choice.Kind, choice.Name)
		}
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
	preview.Request.Categories = nil
	preview.Request.Selections = make([]dataops.Selection, 0, len(preview.Candidates))
	for _, item := range preview.Candidates {
		preview.Request.Selections = append(preview.Request.Selections, dataops.Selection{
			Kind: item.Resource.Kind, Name: item.Resource.Name, ID: item.Resource.ID,
		})
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
	preview.Request.Preview = preview.Token
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
		current, err := m.PreviewPrune(ctx, dataops.Request{Action: "prune", Selections: []dataops.Selection{{
			Kind: item.Resource.Kind, Name: item.Resource.Name, ID: item.Resource.ID,
		}}})
		if err != nil {
			return result, err
		}
		if len(current.Candidates) != 1 || current.Candidates[0].Identity != item.Identity {
			return result, fmt.Errorf("%w: %s %s acquired a reference or protection; no further objects were removed", ErrConflict, item.Resource.Kind, item.Resource.Name)
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
			removed := false
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
				removed = true
			}
			if removeErr == nil && !removed {
				removeErr = fmt.Errorf("%w: BuildKit did not remove the selected cache record; refresh its references before retrying", ErrConflict)
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
	client, closeRuntime, err := m.storageRuntime(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeRuntime()) }()
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
