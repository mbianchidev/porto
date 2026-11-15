package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/app"
)

const hostDiskTrimTimeout = 5 * time.Minute

type cleanupOutput struct {
	builder   strings.Builder
	truncated bool
}

func (o *cleanupOutput) line(format string, args ...any) {
	if o.truncated {
		return
	}
	line := fmt.Sprintf(format, args...) + "\n"
	if o.builder.Len()+len(line) > maxCleanupOutput {
		o.truncated = true
		o.builder.WriteString("[Output truncated; reported counts include every removal.]\n")
		return
	}
	o.builder.WriteString(line)
}

func (o *cleanupOutput) String() string { return strings.TrimSpace(o.builder.String()) }

func finishCleanupStep(step *app.DockerCleanupStep, output *cleanupOutput, err error) {
	step.Output = output.String()
	step.Status = app.CleanupSucceeded
	if err != nil {
		step.Status, step.Error = app.CleanupFailed, err.Error()
	}
}

// pruneStoppedContainers removes standalone containers that stopped before the
// minimum age. Compose services, Kubernetes nodes, Resource Saver workloads and
// Porto helpers are kept so their owners can start or remove them.
func (m *Manager) pruneStoppedContainers(ctx context.Context, now time.Time) (step app.DockerCleanupStep, err error) {
	var output cleanupOutput
	defer func() { finishCleanupStep(&step, &output, err) }()
	containers, err := m.storageContainers(ctx)
	if err != nil {
		return step, fmt.Errorf("list stopped containers: %w", err)
	}
	images, err := m.imagesWithTimeout(ctx, max(m.timeout, imageTransferTimeout))
	if err != nil {
		return step, fmt.Errorf("list container checkpoints: %w", err)
	}
	retained := m.resourceSaverMembers()
	for id := range checkpointedContainers(images) {
		retained[id] = true
	}
	cutoff := now.Add(-app.DockerCleanupMinimumAge)
	kept := 0
	var failures []error
	for _, container := range containers {
		if !stoppedContainer(container) {
			continue
		}
		if !m.removableStoppedContainer(ctx, container, retained, cutoff) {
			kept++
			continue
		}
		if err := ctx.Err(); err != nil {
			return step, errors.Join(append(failures, err)...)
		}
		name := strings.TrimPrefix(container.Name, "/")
		if err := m.ContainerAction(ctx, container.ID, "remove"); err != nil {
			failures = append(failures, fmt.Errorf("remove stopped container %s: %w", name, err))
			continue
		}
		step.ItemsRemoved++
		output.line("Removed stopped container %s (%s)", name, shortContainerID(container.ID))
	}
	if kept > 0 {
		output.line("Kept %d stopped container(s): Compose services, Kubernetes nodes, Resource Saver workloads, Porto helpers, checkpointed containers, and containers stopped less than %s ago are never removed.", kept, app.DockerCleanupMinimumAge)
	}
	return step, errors.Join(failures...)
}

func stoppedContainer(container Container) bool {
	switch strings.ToLower(container.State) {
	case "exited", "stopped", "dead":
		return true
	}
	return false
}

// removableStoppedContainer reports whether cleanup may remove a stopped
// container. retained holds Resource Saver members and checkpointed containers.
func (m *Manager) removableStoppedContainer(ctx context.Context, container Container, retained map[string]bool, cutoff time.Time) bool {
	if container.ComposeProject != "" || container.Labels["com.docker.compose.project"] != "" ||
		kubernetesNodeContainer(container) ||
		retained[container.ID] || retained[strings.TrimPrefix(container.Name, "/")] ||
		container.Labels[resourceSaverOwnerLabel] != "" || container.Labels[transferOwnerLabel] != "" {
		return false
	}
	if !containerLastActiveBefore(container, cutoff) {
		return false
	}
	if m.managedContainerGuard != nil && m.managedContainerGuard(ctx, container.Name) != nil {
		return false
	}
	return m.nativeGuard == nil || m.nativeGuard("container", container.Name) == nil
}

// kubernetesNodeContainer recognizes node containers of kind, Porto, k3d and
// minikube clusters, whose state would be lost if a stopped node were removed.
func kubernetesNodeContainer(container Container) bool {
	return managedClusterContainer(container) || container.Labels["k3d.cluster"] != "" ||
		container.Labels["name.minikube.sigs.k8s.io"] != "" || container.Labels["created_by.minikube.sigs.k8s.io"] != ""
}

// containerLastActiveBefore uses the latest exit, update or lifecycle time.
// Containers without any of them are kept because their stop time is unknown.
func containerLastActiveBefore(container Container, cutoff time.Time) bool {
	var latest time.Time
	for _, value := range []string{container.ExitAt, container.UpdatedAt, container.LastTransitionAt} {
		if parsed := parseDockerTimestamp(value); parsed.After(latest) {
			latest = parsed
		}
	}
	return !latest.IsZero() && latest.Before(cutoff)
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// checkpointedContainers returns the IDs of containers with saved checkpoints,
// so cleanup keeps the containers that restoring those checkpoints needs.
func checkpointedContainers(images []Image) map[string]bool {
	sources := make(map[string]bool)
	for _, image := range images {
		if source, ok := strings.CutPrefix(image.Name, checkpointImagePrefix); ok {
			if id, _, found := strings.Cut(source, ":"); found && id != "" {
				sources[id] = true
			}
		}
	}
	return sources
}

func (m *Manager) resourceSaverMembers() map[string]bool {
	members := make(map[string]bool)
	saver := m.ResourceSaver()
	if saver == nil {
		return members
	}
	for _, entry := range saver.State().Entries {
		for _, member := range entry.Application.Members {
			for _, identity := range []string{member.ID, strings.TrimPrefix(member.Name, "/")} {
				if identity != "" {
					members[identity] = true
				}
			}
		}
	}
	return members
}

// pruneAnonymousVolumes removes anonymous volumes no container references.
// Named volumes, including Compose volumes of stopped projects, are never removed.
func (m *Manager) pruneAnonymousVolumes(ctx context.Context) (step app.DockerCleanupStep, err error) {
	var output cleanupOutput
	defer func() { finishCleanupStep(&step, &output, err) }()
	volumes, err := m.Volumes(ctx)
	if err != nil {
		return step, fmt.Errorf("list volumes: %w", err)
	}
	containers, err := m.storageContainers(ctx)
	if err != nil {
		return step, fmt.Errorf("list volume references: %w", err)
	}
	named, attached := 0, 0
	var failures []error
	for _, volume := range volumes {
		if !anonymousVolume(volume.Name, volume.Labels) || volume.Labels[transferOwnerLabel] != "" {
			named++
			continue
		}
		if volumeReferenced(containers, volume) || m.nativeGuard != nil && m.nativeGuard("volume", volume.Name) != nil {
			attached++
			continue
		}
		if err := ctx.Err(); err != nil {
			return step, errors.Join(append(failures, err)...)
		}
		if err := m.RemoveVolume(ctx, volume.Name, false); err != nil {
			failures = append(failures, fmt.Errorf("remove anonymous volume %s: %w", volume.Name, err))
			continue
		}
		step.ItemsRemoved++
		output.line("Removed unused anonymous volume %s", volume.Name)
	}
	if named > 0 {
		output.line("Kept %d named volume(s); automatic cleanup never removes named volumes.", named)
	}
	if attached > 0 {
		output.line("Kept %d anonymous volume(s) still attached to containers or in use by Porto.", attached)
	}
	return step, errors.Join(failures...)
}

type hostDiskTarget struct {
	instance string
	path     string
	before   int64
	skip     string
	err      error
}

// measureHostDisk records the host allocation of Porto's own Lima engine disk.
func (m *Manager) measureHostDisk(ctx context.Context, backend commandBackend) hostDiskTarget {
	const notOwned = "Host disk reclamation applies only to the Lima engine Porto owns."
	if backend.limaInstance == "" {
		return hostDiskTarget{skip: notOwned}
	}
	state, err := m.readEngineState()
	if errors.Is(err, os.ErrNotExist) || err == nil && (state.Mode != "lima" || state.Instance != backend.limaInstance) {
		return hostDiskTarget{skip: notOwned}
	}
	if err != nil {
		return hostDiskTarget{err: fmt.Errorf("read Porto engine ownership: %w", err)}
	}
	target := hostDiskTarget{instance: state.Instance}
	configuration, err := m.engineConfiguration(ctx, state.Instance)
	if err != nil {
		target.err = fmt.Errorf("locate Porto engine disk: %w", err)
		return target
	}
	if target.path, err = limaDiskPath(configuration.Directory); err != nil {
		target.err = err
		return target
	}
	if target.before, err = allocatedFileBytes(target.path); errors.Is(err, ErrUnsupported) {
		return hostDiskTarget{skip: "Host disk allocation cannot be measured on this platform."}
	} else if err != nil {
		target.err = fmt.Errorf("measure Porto engine disk: %w", err)
	}
	return target
}

func limaDiskPath(directory string) (string, error) {
	// Lima 2 names the writable disk "disk"; earlier releases used "diffdisk".
	for _, name := range []string{"disk", "diffdisk"} {
		path := filepath.Join(directory, name)
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() {
			return path, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect Porto engine disk: %w", err)
		}
	}
	return "", fmt.Errorf("Porto engine disk was not found in %s", directory)
}

// reclaimHostDisk discards the VM's free blocks so the sparse host disk file
// shrinks, then reports the host allocation change across the whole cleanup.
func (m *Manager) reclaimHostDisk(ctx context.Context, target hostDiskTarget) (step app.DockerCleanupStep, err error) {
	if target.skip != "" {
		return app.DockerCleanupStep{Status: app.CleanupSkipped, Output: target.skip}, nil
	}
	var output cleanupOutput
	unsupported := false
	defer func() {
		finishCleanupStep(&step, &output, err)
		if unsupported {
			step.Status = app.CleanupSkipped
		}
	}()
	if target.err != nil {
		return step, target.err
	}
	trimmed, trimErr := m.runStdoutCommand(ctx, hostDiskTrimTimeout, "trim Porto engine disk",
		"limactl", "shell", "--workdir=/", target.instance, "--", "sudo", "-n", "fstrim", "--all", "--verbose")
	for _, line := range strings.Split(strings.TrimSpace(string(trimmed)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			output.line("%s", line)
		}
	}
	if trimErr != nil {
		if strings.Contains(strings.ToLower(trimErr.Error()), "not supported") {
			output.line("The engine VM disk does not support discard, so freed space stays allocated on the host.")
			unsupported = true
			return step, nil
		}
		return step, fmt.Errorf("trim Porto engine disk: %w", trimErr)
	}
	after, err := allocatedFileBytes(target.path)
	if err != nil {
		return step, fmt.Errorf("measure Porto engine disk after trimming: %w", err)
	}
	output.line("Host disk file %s: %d bytes allocated before cleanup, %d bytes after.", target.path, target.before, after)
	if after <= target.before {
		reclaimed := target.before - after
		step.BytesReclaimed = &reclaimed
	} else {
		output.line("Running workloads wrote more data than cleanup released, so no reclaimed total is reported.")
	}
	return step, nil
}
