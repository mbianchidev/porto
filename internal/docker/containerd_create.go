package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	containerd "github.com/containerd/containerd/v2/client"
	corecontainers "github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	portoManagedLabel      = "io.porto.container.managed"
	portoLogPathLabel      = "io.porto.container.log-path"
	portoOpenStdinLabel    = "io.porto.container.open-stdin"
	portoTTYLabel          = "io.porto.container.tty"
	portoAutoRemoveLabel   = "io.porto.container.auto-remove"
	portoNetworkAliasLabel = "io.porto.container.network-aliases"
	portoNetworkStateLabel = "io.porto.container.network-state"
	portoNetworkPIDLabel   = "io.porto.container.network-pid"
	portoRuntimeVersion    = "1"
	portoRuntimeName       = "io.containerd.runc.v2"
	restartLogURILabel     = "containerd.io/restart.loguri"
	restartStoppedLabel    = "containerd.io/restart.explicitly-stopped"
	directNetworkNone      = "none"
	directNetworkHost      = "host"
)

func (r *grpcContainerRuntime) Create(ctx context.Context, request CreateContainerRequest) (string, error) {
	if r.client == nil {
		return "", fmt.Errorf("%w: high-level containerd client is unavailable", ErrUnavailable)
	}
	hostname, err := r.validateDirectCreateRequest(request)
	if err != nil {
		return "", err
	}
	if r.containerNameMu != nil {
		r.containerNameMu.Lock()
		defer r.containerNameMu.Unlock()
	}
	if err := r.ensureContainerNameAvailable(ctx, request.Name, ""); err != nil {
		return "", err
	}
	id, err := randomResourceName()
	if err != nil {
		return "", err
	}
	platform, err := directContainerPlatform(request.Platform, r.lima != "")
	if err != nil {
		return "", err
	}
	parsedPlatform, err := platforms.Parse(platform)
	if err != nil {
		return "", fmt.Errorf("parse direct container platform %q: %w", platform, err)
	}
	if parsedPlatform.OS != "linux" {
		return "", fmt.Errorf("%w: direct container creation currently supports Linux targets", ErrUnsupported)
	}
	image, err := r.resolveContainerImage(ctx, request.Image, platform)
	if err != nil {
		return "", err
	}
	imageSpec, err := image.Spec(withContainerdNamespace(ctx, r.namespace))
	if err != nil {
		return "", fmt.Errorf("read container image configuration: %w", err)
	}
	if r.lima != "" && firstNonEmpty(request.User, imageSpec.Config.User) != "" {
		return "", fmt.Errorf(
			"%w: direct Lima creation requires backend-local image user resolution",
			ErrUnsupported,
		)
	}
	labels, err := r.directContainerLabels(request, image, hostname, id)
	if err != nil {
		return "", err
	}
	specOptions := directContainerSpecOptions(
		request,
		image,
		hostname,
		platform,
		imageSpec.Config,
		r.lima != "",
	)
	snapshotKey := "porto-" + id
	container, err := r.client.NewContainer(
		withContainerdNamespace(ctx, r.namespace),
		id,
		containerd.WithImage(image),
		containerd.WithContainerLabels(labels),
		containerd.WithRuntime(portoRuntimeName, nil),
		containerd.WithNewSnapshot(snapshotKey, image),
		containerd.WithNewSpec(specOptions...),
	)
	if err != nil {
		cleanupErr := r.cleanupDirectSnapshot(ctx, snapshotKey)
		return "", errors.Join(fmt.Errorf("create direct container metadata: %w", err), cleanupErr)
	}
	if container == nil {
		cleanupErr := r.cleanupDirectSnapshot(ctx, snapshotKey)
		return "", errors.Join(errors.New("containerd returned an empty container"), cleanupErr)
	}
	return container.ID(), nil
}

func (r *grpcContainerRuntime) validateDirectCreateRequest(request CreateContainerRequest) (string, error) {
	if err := validateObjectID(request.Image); err != nil {
		return "", fmt.Errorf("image: %w", err)
	}
	if request.Name != "" {
		if err := validateObjectID(request.Name); err != nil {
			return "", fmt.Errorf("container name: %w", err)
		}
	}
	hostname, err := containerHostname(request)
	if err != nil {
		return "", err
	}
	if request.StopTimeout != nil && *request.StopTimeout < 0 {
		return "", errors.New("container stop timeout cannot be negative")
	}
	if err := validateHealthcheck(request.Healthcheck); err != nil {
		return "", err
	}
	if len(request.Volumes) > 0 {
		return "", fmt.Errorf("%w: direct container creation does not yet own volume lifecycle", ErrUnsupported)
	}
	if len(request.Publish) > 0 {
		return "", fmt.Errorf("%w: direct container creation requires direct CNI port mapping", ErrUnsupported)
	}
	if len(request.SecurityOpt) > 0 {
		return "", fmt.Errorf("%w: direct container creation does not yet translate security options", ErrUnsupported)
	}
	if len(request.Devices) > 0 {
		return "", fmt.Errorf("%w: direct device resolution must run beside containerd", ErrUnsupported)
	}
	if request.Init {
		return "", fmt.Errorf("%w: direct init binary injection is unavailable", ErrUnsupported)
	}
	if request.Remove {
		return "", fmt.Errorf("%w: direct auto-remove lifecycle is not available", ErrUnsupported)
	}
	if request.Interactive {
		return "", fmt.Errorf("%w: direct detached OpenStdin lifecycle is not available", ErrUnsupported)
	}
	if runtime.GOOS == "windows" && r.lima == "" {
		return "", fmt.Errorf("%w: native Windows direct creation requires a Windows runtime implementation", ErrUnsupported)
	}
	if request.Privileged && (runtime.GOOS != "linux" || r.lima != "") {
		return "", fmt.Errorf("%w: privileged direct creation requires local Linux containerd", ErrUnsupported)
	}
	if request.Cgroupns != "" && request.Cgroupns != "private" && request.Cgroupns != "host" {
		return "", fmt.Errorf("%w: cgroup namespace mode %q", ErrUnsupported, request.Cgroupns)
	}
	if request.Userns != "" && request.Userns != "host" {
		return "", fmt.Errorf("%w: user namespace mode %q", ErrUnsupported, request.Userns)
	}
	if len(request.Networks) != 1 {
		return "", fmt.Errorf("%w: direct creation requires an explicit host or none network until CNI probing succeeds", ErrUnsupported)
	}
	network := request.Networks[0]
	if len(network.Aliases) > 0 || (network.Name != directNetworkNone && network.Name != directNetworkHost) {
		return "", fmt.Errorf("%w: direct network %q requires CNI endpoint lifecycle support", ErrUnsupported, network.Name)
	}
	switch request.Restart {
	case "", "no", "always", "unless-stopped":
	case "on-failure":
	default:
		if !strings.HasPrefix(request.Restart, "on-failure:") {
			return "", fmt.Errorf("%w: restart policy %q", ErrUnsupported, request.Restart)
		}
		if retries, parseErr := strconv.Atoi(strings.TrimPrefix(request.Restart, "on-failure:")); parseErr != nil || retries < 0 {
			return "", fmt.Errorf("invalid restart policy %q", request.Restart)
		}
	}
	return hostname, nil
}

func (r *grpcContainerRuntime) ensureContainerNameAvailable(
	ctx context.Context,
	name,
	excludedID string,
) error {
	if name == "" {
		return nil
	}
	response, err := r.containers.List(
		withContainerdNamespace(ctx, r.namespace),
		&containersapi.ListContainersRequest{},
	)
	if err != nil {
		return fmt.Errorf("list container names before direct create: %w", err)
	}
	for _, record := range response.GetContainers() {
		if record.GetID() == excludedID {
			continue
		}
		if record.GetLabels()[nerdctlNameLabel] == name {
			return fmt.Errorf("%w: container name %q is already in use", ErrConflict, name)
		}
	}
	return nil
}

func (r *grpcContainerRuntime) resolveContainerImage(
	ctx context.Context,
	requestedReference,
	platform string,
) (containerd.Image, error) {
	named, err := reference.ParseDockerRef(normalizeNerdctlReference(requestedReference))
	if err != nil {
		return nil, fmt.Errorf("normalize container image %q: %w", requestedReference, err)
	}
	imageReference := named.String()
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	image, err := r.client.GetImage(namespacedContext, imageReference)
	if errdefs.IsNotFound(err) {
		options := []containerd.RemoteOpt{
			containerd.WithPullUnpack,
			containerd.WithPlatform(platform),
		}
		image, err = r.client.Pull(namespacedContext, imageReference, options...)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve container image %q: %w", requestedReference, err)
	}
	parsedPlatform, err := platforms.Parse(platform)
	if err != nil {
		return nil, fmt.Errorf("parse container platform %q: %w", platform, err)
	}
	image = containerd.NewImageWithPlatform(
		r.client,
		image.Metadata(),
		platforms.OnlyStrict(parsedPlatform),
	)
	unpacked, err := image.IsUnpacked(namespacedContext, "")
	if err != nil {
		return nil, fmt.Errorf("inspect image unpack state %q: %w", imageReference, err)
	}
	if !unpacked {
		if err := image.Unpack(namespacedContext, ""); err != nil {
			return nil, fmt.Errorf("unpack container image %q: %w", imageReference, err)
		}
	}
	return image, nil
}

func (r *grpcContainerRuntime) directContainerLabels(
	request CreateContainerRequest,
	image containerd.Image,
	hostname,
	id string,
) (map[string]string, error) {
	labels := cloneStringMap(request.Labels)
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[portoManagedLabel] = portoRuntimeVersion
	labels[portoOpenStdinLabel] = strconv.FormatBool(request.Interactive)
	labels[portoTTYLabel] = strconv.FormatBool(request.TTY)
	labels[portoAutoRemoveLabel] = strconv.FormatBool(request.Remove)
	labels[nerdctlNameLabel] = firstNonEmpty(request.Name, id)
	labels[nerdctlImageDigestLabel] = image.Target().Digest.String()
	networkDocument, err := json.Marshal([]string{request.Networks[0].Name})
	if err != nil {
		return nil, fmt.Errorf("encode direct container networks: %w", err)
	}
	labels[nerdctlNetworksLabel] = string(networkDocument)
	if request.StopTimeout != nil {
		labels[nerdctlStopTimeoutLabel] = strconv.Itoa(*request.StopTimeout)
	}
	if request.Healthcheck != nil {
		healthDocument, err := json.Marshal(request.Healthcheck)
		if err != nil {
			return nil, fmt.Errorf("encode direct healthcheck: %w", err)
		}
		labels[nerdctlHealthcheckLabel] = string(healthDocument)
		labels[nerdctlHealthStateLabel] = `{"Status":"starting","FailingStreak":0}`
	}
	if request.Restart != "" && request.Restart != "no" {
		labels[restartPolicyLabel] = request.Restart
	}
	logPath, err := r.directContainerLogPath(id)
	if err != nil {
		return nil, err
	}
	labels[portoLogPathLabel] = logPath
	if request.Restart != "" && request.Restart != "no" {
		labels[restartStatusLabel] = "stopped"
		labels[restartStoppedLabel] = "true"
		labels[restartLogURILabel] = (&url.URL{
			Scheme: "file",
			Path:   filepath.ToSlash(logPath),
		}).String()
	}
	if hostname != "" {
		labels["io.porto.container.hostname"] = hostname
	}
	return labels, nil
}

func directContainerSpecOptions(
	request CreateContainerRequest,
	image containerd.Image,
	hostname,
	platform string,
	imageConfig ocispec.ImageConfig,
	backendLocalRootFS bool,
) []oci.SpecOpts {
	options := []oci.SpecOpts{oci.WithDefaultSpecForPlatform(platform)}
	if backendLocalRootFS {
		options = append(options, func(
			_ context.Context,
			_ oci.Client,
			_ *corecontainers.Container,
			spec *oci.Spec,
		) error {
			if spec.Process == nil {
				spec.Process = &specs.Process{}
			}
			entrypoint := imageConfig.Entrypoint
			if len(request.Entrypoint) > 0 {
				entrypoint = request.Entrypoint
			}
			command := imageConfig.Cmd
			if len(request.Command) > 0 {
				command = request.Command
			}
			spec.Process.Args = append(append([]string(nil), entrypoint...), command...)
			spec.Process.Env = mergeExecEnvironment(imageConfig.Env, request.Environment)
			spec.Process.Cwd = firstNonEmpty(request.WorkingDir, imageConfig.WorkingDir, "/")
			return nil
		})
	} else {
		options = append(options, oci.WithImageConfigArgs(image, request.Command))
		if len(request.Entrypoint) > 0 {
			command := request.Command
			if len(command) == 0 {
				command = imageConfig.Cmd
			}
			args := append(append([]string(nil), request.Entrypoint...), command...)
			options = append(options, oci.WithProcessArgs(args...))
		}
		if len(request.Environment) > 0 {
			options = append(options, oci.WithEnv(request.Environment))
		}
		if request.WorkingDir != "" {
			options = append(options, oci.WithProcessCwd(request.WorkingDir))
		}
		if request.User != "" {
			options = append(options, oci.WithUser(request.User))
		}
	}
	if hostname != "" {
		options = append(options, oci.WithHostname(hostname))
	}
	if request.Privileged {
		options = append(options, oci.WithPrivileged)
	}
	if request.ShmSize > 0 {
		options = append(options, oci.WithDevShmSize(request.ShmSize/1024))
	}
	if request.Networks[0].Name == directNetworkHost {
		options = append(options, oci.WithHostNamespace(specs.NetworkNamespace))
	}
	if request.Cgroupns == "host" {
		options = append(options, oci.WithHostNamespace(specs.CgroupNamespace))
	}
	if len(request.Tmpfs) > 0 {
		mounts := make([]specs.Mount, 0, len(request.Tmpfs))
		for _, target := range sortedStringKeys(request.Tmpfs) {
			mountOptions := []string{"nosuid", "nodev"}
			if value := strings.TrimSpace(request.Tmpfs[target]); value != "" {
				mountOptions = append(mountOptions, strings.Split(value, ",")...)
			}
			mounts = append(mounts, specs.Mount{
				Destination: target,
				Type:        "tmpfs",
				Source:      "tmpfs",
				Options:     mountOptions,
			})
		}
		options = append(options, oci.WithMounts(mounts))
	}
	stopSignal := firstNonEmpty(request.StopSignal, imageConfig.StopSignal)
	if len(request.Sysctls) > 0 || request.TTY || stopSignal != "" {
		options = append(options, func(
			_ context.Context,
			_ oci.Client,
			_ *corecontainers.Container,
			spec *oci.Spec,
		) error {
			if spec.Process == nil {
				spec.Process = &specs.Process{}
			}
			spec.Process.Terminal = request.TTY
			if len(request.Sysctls) > 0 {
				if spec.Linux == nil {
					spec.Linux = &specs.Linux{}
				}
				if spec.Linux.Sysctl == nil {
					spec.Linux.Sysctl = make(map[string]string)
				}
				for key, value := range request.Sysctls {
					spec.Linux.Sysctl[key] = value
				}
			}
			if stopSignal != "" {
				if spec.Annotations == nil {
					spec.Annotations = make(map[string]string)
				}
				spec.Annotations["org.opencontainers.image.stopSignal"] = stopSignal
			}
			return nil
		})
	}
	return options
}

func directContainerPlatform(requested string, lima bool) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		osName := runtime.GOOS
		if lima || osName == "darwin" {
			osName = "linux"
		}
		return platforms.Format(ocispec.Platform{
			OS:           osName,
			Architecture: runtime.GOARCH,
		}), nil
	}
	platform, err := platforms.Parse(requested)
	if err != nil {
		return "", fmt.Errorf("invalid container platform %q: %w", requested, err)
	}
	return platforms.Format(platform), nil
}

func (r *grpcContainerRuntime) directContainerLogPath(id string) (string, error) {
	if r.lima != "" {
		return filepath.Join("/tmp", "porto-container-logs", id+".log"), nil
	}
	if r.logDir == "" {
		return "", fmt.Errorf("%w: direct container log directory is unavailable", ErrUnsupported)
	}
	return filepath.Join(r.logDir, id+".log"), nil
}

func (r *grpcContainerRuntime) cleanupDirectSnapshot(ctx context.Context, snapshotKey string) error {
	if r.client == nil || snapshotKey == "" {
		return nil
	}
	err := r.client.SnapshotService("").Remove(withContainerdNamespace(ctx, r.namespace), snapshotKey)
	if err == nil || errdefs.IsNotFound(err) {
		return nil
	}
	return fmt.Errorf("clean up direct container snapshot %q: %w", snapshotKey, err)
}
