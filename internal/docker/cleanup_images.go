package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/mbianchidev/porto/internal/app"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type imageMetadataStores struct {
	images     images.Store
	containers containers.Store
	content    content.Provider
}

// imageMetadata returns the namespace-scoped containerd stores. Cleanup reads
// image records directly because `nerdctl images` hides checkpoint images and
// reports a creation time that rebuilds don't update.
func (m *Manager) imageMetadata(ctx context.Context) (context.Context, imageMetadataStores, func() error, error) {
	if m.imageMetadataConnector != nil {
		return m.imageMetadataConnector(ctx)
	}
	runtimeClient, closeRuntime, err := m.storageRuntime(ctx)
	if err != nil {
		return ctx, imageMetadataStores{}, nil, err
	}
	backend, ok := runtimeClient.(*grpcContainerRuntime)
	if !ok || backend.client == nil {
		return ctx, imageMetadataStores{}, nil, errors.Join(fmt.Errorf("%w: image cleanup requires containerd metadata", ErrUnsupported), closeRuntime())
	}
	stores := imageMetadataStores{
		images: backend.client.ImageService(), containers: backend.client.ContainerService(), content: backend.client.ContentStore(),
	}
	return namespaces.WithNamespace(ctx, backend.namespace), stores, closeRuntime, nil
}

// pruneUnusedImages removes image references that no container uses and that
// were not created, pulled or rebuilt within the minimum age. Checkpoint
// images are kept because no container names them.
func (m *Manager) pruneUnusedImages(ctx context.Context, now time.Time) (step app.DockerCleanupStep, err error) {
	var output cleanupOutput
	defer func() { finishCleanupStep(&step, &output, err) }()
	ctx, stores, closeMetadata, err := m.imageMetadata(ctx)
	if err != nil {
		return step, fmt.Errorf("open image metadata: %w", err)
	}
	defer func() { err = errors.Join(err, closeMetadata()) }()
	records, err := stores.images.List(ctx)
	if err != nil {
		return step, fmt.Errorf("list images: %w", err)
	}
	restoreBases, err := checkpointBaseImages(ctx, stores.content, records)
	if err != nil {
		return step, err
	}
	containers, err := m.storageContainers(ctx)
	if err != nil {
		return step, fmt.Errorf("list image references: %w", err)
	}
	cutoff := now.Add(-app.DockerCleanupMinimumAge)
	kept := 0
	var failures []error
	for _, record := range records {
		if checkpointImage(record) || restoreBases[record.Name] || restoreBases[record.Target.Digest.String()] ||
			!imageRecordOlderThan(record, cutoff) || imageRecordInUse(record, containers) {
			kept++
			continue
		}
		if err := ctx.Err(); err != nil {
			return step, errors.Join(append(failures, err)...)
		}
		removed, err := removeUnusedImageRecord(ctx, stores.images, stores.containers, record, cutoff)
		if err != nil {
			failures = append(failures, fmt.Errorf("remove image %s: %w", record.Name, err))
			continue
		}
		if !removed {
			kept++
			continue
		}
		step.ItemsRemoved++
		output.line("Removed unused image reference %s", record.Name)
	}
	if kept > 0 {
		output.line("Kept %d image reference(s): used by a container, a container checkpoint or its restore base, or created, pulled or rebuilt less than %s ago.", kept, app.DockerCleanupMinimumAge)
	}
	return step, errors.Join(failures...)
}

// removeUnusedImageRecord deletes a reference only if it still points at the
// listed target, is still old enough and no container names it. Tags rebuilt
// or re-pulled during cleanup therefore survive.
func removeUnusedImageRecord(ctx context.Context, store images.Store, containerStore containers.Store, listed images.Image, cutoff time.Time) (bool, error) {
	current, err := store.Get(ctx, listed.Name)
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current.Target.Digest != listed.Target.Digest || checkpointImage(current) || !imageRecordOlderThan(current, cutoff) {
		return false, nil
	}
	users, err := containerStore.List(ctx, "image=="+strconv.Quote(current.Name))
	if err != nil {
		return false, fmt.Errorf("check image users: %w", err)
	}
	if len(users) > 0 {
		return false, nil
	}
	target := current.Target
	err = store.Delete(ctx, current.Name, images.DeleteTarget(&target), images.SynchronousDelete())
	if errdefs.IsNotFound(err) || errdefs.IsFailedPrecondition(err) {
		return false, nil
	}
	return err == nil, err
}

// checkpointBaseImages returns the image names and target digests that
// checkpoint restores load: containerd records the base image name in the
// checkpoint index annotations and its target among the index manifests.
func checkpointBaseImages(ctx context.Context, provider content.Provider, records []images.Image) (map[string]bool, error) {
	bases := make(map[string]bool)
	for _, record := range records {
		if !checkpointImage(record) {
			continue
		}
		data, err := content.ReadBlob(ctx, provider, record.Target)
		if err != nil {
			return nil, fmt.Errorf("read checkpoint %s to protect its restore image: %w", record.Name, err)
		}
		var index ocispec.Index
		if err := json.Unmarshal(data, &index); err != nil {
			return nil, fmt.Errorf("decode checkpoint %s to protect its restore image: %w", record.Name, err)
		}
		if name := index.Annotations[ocispec.AnnotationRefName]; name != "" {
			bases[name] = true
		}
		for _, manifest := range index.Manifests {
			bases[manifest.Digest.String()] = true
		}
	}
	return bases, nil
}

func checkpointImage(record images.Image) bool {
	return record.Labels[checkpointSourceIDLabel] != "" || strings.HasPrefix(record.Name, checkpointImagePrefix)
}

// imageRecordOlderThan uses the latest record time; containerd keeps the
// original creation time when a pull or build moves an existing tag.
func imageRecordOlderThan(record images.Image, cutoff time.Time) bool {
	latest := record.CreatedAt
	if record.UpdatedAt.After(latest) {
		latest = record.UpdatedAt
	}
	return !latest.IsZero() && latest.Before(cutoff)
}

func imageRecordInUse(record images.Image, containers []Container) bool {
	target := record.Target.Digest.String()
	for _, container := range containers {
		if container.ImageID == target || container.Labels[nerdctlImageDigestLabel] == target ||
			container.Image == record.Name || container.Image == target {
			return true
		}
		if named, err := reference.ParseDockerRef(container.Image); err == nil && named.String() == record.Name {
			return true
		}
	}
	return false
}

// checkpointSources lists the containers that saved checkpoints belong to.
func (m *Manager) checkpointSources(ctx context.Context) (sources map[string]bool, err error) {
	ctx, stores, closeMetadata, err := m.imageMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("open image metadata: %w", err)
	}
	defer func() { err = errors.Join(err, closeMetadata()) }()
	records, err := stores.images.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list container checkpoints: %w", err)
	}
	return checkpointedContainers(records), nil
}

// checkpointedContainers returns the source container IDs of checkpoint
// images, from their source label or Porto's default checkpoint name.
func checkpointedContainers(records []images.Image) map[string]bool {
	sources := make(map[string]bool)
	for _, record := range records {
		if id := record.Labels[checkpointSourceIDLabel]; id != "" {
			sources[id] = true
		} else if source, ok := strings.CutPrefix(record.Name, checkpointImagePrefix); ok {
			if id, _, found := strings.Cut(source, ":"); found && id != "" {
				sources[id] = true
			}
		}
	}
	return sources
}
