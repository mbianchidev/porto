package docker

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
)

const volumeRootModeTimeout = 20 * time.Second

// applyDefaultVolumeRootMode gives a never-used volume Docker's 0755 data root.
// nerdctl creates every volume root as 0700, which non-root container users
// cannot traverse; image copy-up still sets the root from the image directory.
// It runs only beside the daemon's container inventory, whose live connection
// keeps volume and container creation from waiting on a new runtime dial.
func (m *Manager) applyDefaultVolumeRootMode(ctx context.Context, name string) error {
	if m.activeContainerInventory() == nil {
		return fmt.Errorf("%w: the container inventory is not running", ErrUnavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, volumeRootModeTimeout)
	defer cancel()
	descriptor, err := m.FileDescriptor(ctx, "volume", name)
	if err != nil {
		return err
	}
	var output bytes.Buffer
	return m.RunFileRequest(ctx, descriptor, datafiles.Request{
		Action: "default-root-mode", Identity: descriptor.Resource.Fingerprint(),
	}, nil, &output)
}

// applyContainerVolumeRootModes covers the volumes nerdctl creates while
// creating a container: named volumes it has not seen yet, `-v /path` mounts
// and image VOLUME declarations, as recorded in nerdctl's mount labels.
// Failures are logged; the container stays usable.
func (m *Manager) applyContainerVolumeRootModes(ctx context.Context, id string) {
	inventory := m.activeContainerInventory()
	if inventory == nil {
		return
	}
	container, err := findSnapshotContainer(inventory.snapshotValue(), id)
	if err != nil {
		log.Printf("set default volume permissions for container %s: %v", id, err)
		return
	}
	mounts, err := containerMountMetadata(container.Labels)
	if err != nil {
		log.Printf("set default volume permissions for container %s: %v", id, err)
		return
	}
	names := make([]string, 0, len(mounts))
	for _, mount := range mounts {
		if mount.Type != "volume" || mount.Name == "" || slices.Contains(names, mount.Name) {
			continue
		}
		names = append(names, mount.Name)
		if err := m.applyDefaultVolumeRootMode(ctx, mount.Name); err != nil {
			log.Printf("set default permissions on volume %s for container %s: %v", mount.Name, id, err)
		}
	}
}
