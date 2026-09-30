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
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/platforms"
	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimefiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func (m *Manager) FileDescriptor(ctx context.Context, kind, name string) (descriptor runtimefiles.Descriptor, err error) {
	if err := validateObjectID(name); err != nil {
		return descriptor, err
	}
	if kind != "container" && kind != "image" && kind != "volume" {
		return descriptor, fmt.Errorf("%w: file resource must be a container, image or volume", datafiles.ErrInvalid)
	}
	if m.fileDescriptorReader != nil {
		return m.fileDescriptorReader(ctx, kind, name)
	}
	client, err := m.runtimeConnector(ctx)
	if err != nil {
		return descriptor, err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	backend, ok := client.(*grpcContainerRuntime)
	if !ok || backend.client == nil || backend.address == "" {
		return descriptor, fmt.Errorf("%w: backend-local snapshot files require the packaged Linux runtime helper", datafiles.ErrUnsupported)
	}
	descriptor.Address, descriptor.Namespace = backend.address, backend.namespace
	owner := sha256.Sum256([]byte(m.stateDir + "\x00" + backend.namespace))
	descriptor.Owner = hex.EncodeToString(owner[:16])
	descriptor.Resource = datafiles.Resource{Kind: kind, Name: name, Backend: backend.backend + "/" + backend.namespace}
	ctx = namespaces.WithNamespace(ctx, backend.namespace)
	switch kind {
	case "container":
		id, err := backend.resolveContainerID(ctx, name)
		if err != nil {
			return descriptor, err
		}
		container, err := backend.client.LoadContainer(ctx, id)
		if err != nil {
			return descriptor, err
		}
		info, err := container.Info(ctx)
		if err != nil {
			return descriptor, err
		}
		spec, err := container.Spec(ctx)
		if err != nil || spec.Root == nil {
			return descriptor, errors.Join(datafiles.ErrUnsupported, err)
		}
		descriptor.Resource.ID = info.ID
		descriptor.Resource.Name = firstNonEmpty(info.Labels[nerdctlNameLabel], info.ID)
		descriptor.Resource.CreatedAt = info.CreatedAt.UTC().Format(time.RFC3339Nano)
		descriptor.Resource.ReadOnly = spec.Root.Readonly
		descriptor.Snapshotter, descriptor.SnapshotKey, descriptor.Mounts = info.Snapshotter, info.SnapshotKey, spec.Mounts
		process, err := backend.getTask(ctx, id)
		if err != nil && !containerRemovalComplete(err) {
			return descriptor, err
		}
		if process != nil && containerdTaskActive(process.GetStatus()) {
			descriptor.PID = process.GetPid()
		}
		for _, mounted := range spec.Mounts {
			kind := mounted.Type
			if mounted.Type == "bind" && strings.Contains(mounted.Source, "/volumes/") {
				kind = "volume"
			}
			descriptor.Display = append(descriptor.Display, datafiles.Mount{
				Path: mounted.Destination, Kind: kind, Source: mounted.Source,
				ReadOnly: spec.Root.Readonly || slices.Contains(mounted.Options, "ro"),
			})
		}
	case "image":
		images, err := m.Images(ctx)
		if err != nil {
			return descriptor, err
		}
		var matched *Image
		for _, image := range images {
			if imageMatchesIdentifier(image, name) {
				if matched != nil && matched.Platform != image.Platform {
					return descriptor, fmt.Errorf("%w: select an exact image reference and platform", datafiles.ErrConflict)
				}
				candidate := image
				matched = &candidate
			}
		}
		if matched == nil {
			return descriptor, ErrNotFound
		}
		reference := matched.Name
		if reference == "" {
			reference = matched.Repository + ":" + matched.Tag
		}
		image, err := backend.client.GetImage(ctx, normalizeNerdctlReference(reference))
		if err != nil {
			return descriptor, err
		}
		descriptor.Platform = firstNonEmpty(matched.Platform, "linux/"+runtime.GOARCH)
		parsed, err := platforms.Parse(descriptor.Platform)
		if err != nil || parsed.OS != "linux" {
			return descriptor, errors.Join(datafiles.ErrUnsupported, err)
		}
		image = containerd.NewImageWithPlatform(backend.client, image.Metadata(), platforms.OnlyStrict(parsed))
		descriptor.Resource.Name, descriptor.Resource.ID = image.Name(), image.Target().Digest.String()
		descriptor.Resource.ReadOnly = true
		descriptor.Resource.CreatedAt = image.Metadata().CreatedAt.UTC().Format(time.RFC3339Nano)
	case "volume":
		document, err := m.InspectVolume(ctx, name)
		if err != nil {
			return descriptor, err
		}
		var volume struct {
			Name       string
			Driver     string
			Mountpoint string
			CreatedAt  string
			Options    map[string]string
		}
		if err := json.Unmarshal(document, &volume); err != nil {
			return descriptor, err
		}
		if volume.Name != name || volume.Driver != "local" || !path.IsAbs(volume.Mountpoint) || volume.CreatedAt == "" || len(volume.Options) != 0 {
			return descriptor, fmt.Errorf("%w: file access requires an identity-bearing local volume without external driver options", datafiles.ErrUnsupported)
		}
		descriptor.RootPath = volume.Mountpoint
		descriptor.Resource.Name, descriptor.Resource.CreatedAt = volume.Name, volume.CreatedAt
		hash := sha256.Sum256([]byte(volume.Name + "\x00" + volume.CreatedAt + "\x00" + volume.Mountpoint))
		descriptor.Resource.ID = hex.EncodeToString(hash[:])
	}
	return descriptor, nil
}

func (m *Manager) FileRequest(ctx context.Context, kind, name string, request datafiles.Request, input io.Reader, output io.Writer) error {
	if err := datafiles.ValidatePath(firstNonEmpty(request.Path, ".")); err != nil {
		return err
	}
	descriptor, err := m.FileDescriptor(ctx, kind, name)
	if err != nil {
		return err
	}
	if request.Identity == "" || request.Identity != descriptor.Resource.Fingerprint() {
		return datafiles.ErrConflict
	}
	return m.RunFileRequest(ctx, descriptor, request, input, output)
}

func (m *Manager) RunFileRequest(ctx context.Context, descriptor runtimefiles.Descriptor, request datafiles.Request, input io.Reader, output io.Writer) error {
	header, err := json.Marshal(runtimefiles.Envelope{Descriptor: descriptor, Request: request})
	if err != nil {
		return err
	}
	header = append(header, '\n')
	if input == nil {
		input = bytes.NewReader(nil)
	}
	runner, ok := m.runner.(streamingRunner)
	if !ok {
		return fmt.Errorf("%w: the runtime cannot stream file transfers", datafiles.ErrUnsupported)
	}
	backend, err := m.backend(ctx)
	if err != nil {
		return err
	}
	command := runtimes.Command{StdinReader: io.MultiReader(bytes.NewReader(header), input)}
	if backend.limaInstance != "" {
		command.Name = backend.name
		command.Args = []string{
			"shell", "--workdir=/", backend.limaInstance, "--", "sh", "-c",
			`exec sudo -n -- "$HOME/.local/bin/porto-runtime-helper" files`,
		}
	} else {
		if runtime.GOOS != "linux" {
			return fmt.Errorf("%w: local snapshot files require Linux; use the Porto-managed Lima engine", datafiles.ErrUnsupported)
		}
		helper, err := m.runtimeHelperPath()
		if err != nil {
			return err
		}
		command.Name, command.Args = helper, []string{"files"}
		if os.Geteuid() != 0 {
			command.Name, command.Args = "sudo", []string{"-n", "--", helper, "files"}
		}
	}
	var diagnostics strings.Builder
	_, runErr := runner.RunStreaming(ctx, command, func(chunk runtimes.OutputChunk) error {
		if chunk.Stream == "stdout" {
			_, err := output.Write(chunk.Data)
			return err
		}
		remaining := 4096 - diagnostics.Len()
		if remaining > 0 {
			diagnostics.Write(chunk.Data[:min(len(chunk.Data), remaining)])
		}
		return nil
	})
	if runErr != nil {
		// Binary stdout may contain secrets. Only helper stderr, never captured
		// transfer contents, may enter a diagnostic or an operation result.
		return errors.Join(ctx.Err(), fmt.Errorf("runtime filesystem operation failed: %w: %s", runErr, strings.TrimSpace(diagnostics.String())))
	}
	return nil
}
