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
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/runtimes"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	migrationSourceLabel = "io.porto.migration.source"
	migrationPlanLabel   = "io.porto.migration.plan"
)

type MigrationContext struct {
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Desktop   bool   `json:"desktop"`
	Supported bool   `json:"supported"`
	Message   string `json:"message,omitempty"`
}

type MigrationObject struct {
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	ID             string `json:"id"`
	Size           int64  `json:"size,omitempty"`
	Supported      bool   `json:"supported"`
	Message        string `json:"message,omitempty"`
	ComposeProject string `json:"composeProject,omitempty"`
	ComposeService string `json:"composeService,omitempty"`
	PlanDigest     string `json:"planDigest,omitempty"`
}

type MigrationInventory struct {
	Context  MigrationContext  `json:"context"`
	Objects  []MigrationObject `json:"objects"`
	Warnings []string          `json:"warnings"`
}

type MigrationPreview struct {
	Request       dataops.Request   `json:"request"`
	Objects       []MigrationObject `json:"objects"`
	Conflicts     []string          `json:"conflicts"`
	Warnings      []string          `json:"warnings"`
	Token         string            `json:"token"`
	SourcePolicy  string            `json:"sourcePolicy"`
	SourceHelpers []string          `json:"sourceHelpers"`
}

type migrationClient struct {
	client  *http.Client
	context MigrationContext
}

func (m *Manager) MigrationContexts(ctx context.Context) ([]MigrationContext, error) {
	output, err := m.runDockerCLI(ctx, "discover source Docker contexts without changing the active context", "context", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(bytes.TrimSpace(output), []byte{'\n'})
	contexts := make([]MigrationContext, 0, len(lines))
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var item struct{ Name, Description, DockerEndpoint string }
		if err := json.Unmarshal(line, &item); err != nil {
			return nil, fmt.Errorf("decode Docker context metadata: %w", err)
		}
		if item.Name == "porto" {
			continue
		}
		source := MigrationContext{Name: item.Name, Endpoint: item.DockerEndpoint, Desktop: strings.Contains(strings.ToLower(item.Name+" "+item.Description), "desktop")}
		source.Supported = strings.HasPrefix(source.Endpoint, "unix://") || strings.HasPrefix(source.Endpoint, "npipe://")
		if !source.Supported {
			source.Message = "Only local Unix sockets and Windows named pipes are supported. Export remote data locally first; credentials and SSH/TLS context material are not imported."
		}
		contexts = append(contexts, source)
	}
	return contexts, nil
}

func (m *Manager) migrationSource(ctx context.Context, name string) (*migrationClient, error) {
	if err := validateObjectID(name); err != nil {
		return nil, err
	}
	if m.migrationSourceFactory != nil {
		return m.migrationSourceFactory(ctx, name)
	}
	contexts, err := m.MigrationContexts(ctx)
	if err != nil {
		return nil, err
	}
	var selected *MigrationContext
	for _, source := range contexts {
		if source.Name == name {
			copy := source
			selected = &copy
			break
		}
	}
	if selected == nil || !selected.Supported {
		return nil, fmt.Errorf("%w: select a supported, local, non-Porto source context", ErrUnsupported)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialMigrationSource(ctx, selected.Endpoint)
		}}
	source := &migrationClient{client: &http.Client{Transport: transport}, context: *selected}
	var version struct{ Platform struct{ Name string } }
	if err := source.get(ctx, "/version", nil, &version); err != nil {
		return nil, err
	}
	if strings.Contains(strings.ToLower(version.Platform.Name), "porto") {
		return nil, fmt.Errorf("%w: source context points at Porto; migration cannot copy the destination into itself", ErrConflict)
	}
	return source, nil
}

func (c *migrationClient) request(ctx context.Context, method, resource string, query url.Values, body io.Reader) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+resource, body)
	if err != nil {
		return nil, err
	}
	request.URL.RawQuery = query.Encode()
	if body != nil {
		contentType := "application/json"
		if resource == "/images/load" {
			contentType = "application/x-tar"
		}
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("source runtime request failed: %w", err)
	}
	if response.StatusCode >= 300 {
		defer response.Body.Close()
		var message struct{ Message string }
		if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&message); err != nil {
			return nil, &sourceAPIError{status: response.StatusCode, message: "source runtime rejected this request"}
		}
		return nil, &sourceAPIError{status: response.StatusCode, message: message.Message}
	}
	return response, nil
}

func (c *migrationClient) get(ctx context.Context, resource string, query url.Values, value any) error {
	response, err := c.request(ctx, http.MethodGet, resource, query, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := json.NewDecoder(io.LimitReader(response.Body, 16*1024*1024)).Decode(value); err != nil {
		return fmt.Errorf("decode source runtime metadata: %w", err)
	}
	return nil
}

type migrationContainer struct {
	ID      string `json:"Id"`
	Name    string
	Image   string
	Created string
	State   struct{ Running, Paused, Restarting bool }
	Config  struct {
		Image, User, WorkingDir, Hostname, StopSignal string
		StopTimeout                                   *int
		Cmd, Entrypoint, Env                          []string
		Labels                                        map[string]string
		ExposedPorts                                  map[string]json.RawMessage
		Tty, OpenStdin                                bool
		Healthcheck                                   *ContainerHealthcheck
	}
	HostConfig struct {
		Privileged, AutoRemove, ReadonlyRootfs                                    bool
		Init                                                                      *bool
		PidMode, IpcMode, UTSMode, UsernsMode, CgroupnsMode, Runtime, NetworkMode string
		Binds, VolumesFrom, Links, DNS, DNSSearch, DNSOptions, CapAdd, CapDrop    []string
		Devices                                                                   []json.RawMessage
		Mounts                                                                    []dockerStructuredMount
		SecurityOpt                                                               []string
		Sysctls                                                                   map[string]string
		Tmpfs                                                                     map[string]string
		Memory, MemorySwap, NanoCpus, ShmSize                                     int64
		CpuShares, CpuPeriod, CpuQuota, CpuRealtimePeriod, CpuRealtimeRuntime     int64
		CpusetCpus, CpusetMems                                                    string
		PidsLimit                                                                 *int64
		ExtraHosts, GroupAdd                                                      []string
		Ulimits                                                                   []json.RawMessage
		LogConfig                                                                 struct {
			Type   string
			Config map[string]string
		}
		RestartPolicy struct {
			Name              string
			MaximumRetryCount int
		}
		PortBindings map[string][]struct{ HostIP, HostPort string }
	}
	Mounts []struct {
		Type, Name, Source, Destination string
		RW                              bool
	}
	NetworkSettings struct {
		Networks map[string]struct {
			Aliases            []string
			IPAddress, Gateway string
			IPPrefixLen        int
			DriverOpts         map[string]string
			IPAMConfig         *struct {
				IPv4Address, IPv6Address string
				LinkLocalIPs             []string
			}
		}
	}
}

var sensitiveMigrationValue = regexp.MustCompile(`(?i)password|secret|token|credential|authorization|private.?key|api.?key`)

func migrationContainerRequest(document []byte, includeSensitive bool) (CreateContainerRequest, ContainerUpdate, error) {
	var source migrationContainer
	if err := json.Unmarshal(document, &source); err != nil {
		return CreateContainerRequest{}, ContainerUpdate{}, err
	}
	if source.ID == "" || source.Config.Image == "" || source.State.Running || source.State.Paused || source.State.Restarting {
		return CreateContainerRequest{}, ContainerUpdate{}, fmt.Errorf("%w: only compatible stopped source containers can be recreated", ErrUnsupported)
	}
	host := source.HostConfig
	if host.PidMode != "" || host.UTSMode != "" || host.IpcMode != "" && host.IpcMode != "private" ||
		len(host.Binds) > 0 && len(source.Mounts) == 0 || len(host.VolumesFrom) > 0 || len(host.Links) > 0 ||
		len(host.DNS)+len(host.DNSSearch)+len(host.DNSOptions)+len(host.CapAdd)+len(host.CapDrop)+len(host.Devices) > 0 ||
		host.ReadonlyRootfs || host.UsernsMode != "" && host.UsernsMode != "host" ||
		host.Runtime != "" && host.Runtime != "runc" || host.AutoRemove {
		return CreateContainerRequest{}, ContainerUpdate{}, fmt.Errorf("%w: source has unsupported host paths, namespace, device, DNS, capability, runtime or rootfs options; no destination container was created", ErrUnsupported)
	}
	_, tmpfs, err := structuredMountArguments(host.Mounts, host.Tmpfs)
	if err != nil {
		return CreateContainerRequest{}, ContainerUpdate{}, err
	}
	host.Tmpfs = tmpfs
	for _, binding := range host.Binds {
		parts := strings.Split(binding, ":")
		if len(parts) > 3 || len(parts) < 2 {
			return CreateContainerRequest{}, ContainerUpdate{}, fmt.Errorf("%w: source volume binding cannot be represented", ErrUnsupported)
		}
		if len(parts) == 3 {
			for _, option := range strings.Split(parts[2], ",") {
				if option != "" && option != "ro" && option != "rw" && option != "nocopy" {
					return CreateContainerRequest{}, ContainerUpdate{}, fmt.Errorf("%w: source volume binding option %s", ErrUnsupported, option)
				}
			}
		}
	}
	if host.CpuShares != 0 || host.CpuPeriod != 0 || host.CpuQuota != 0 ||
		host.CpuRealtimePeriod != 0 || host.CpuRealtimeRuntime != 0 || host.CpusetCpus != "" || host.CpusetMems != "" ||
		host.PidsLimit != nil && *host.PidsLimit != 0 || len(host.Ulimits) > 0 ||
		host.Memory < 0 || host.MemorySwap < 0 || host.NanoCpus < 0 {
		return CreateContainerRequest{}, ContainerUpdate{}, fmt.Errorf("%w: CPU scheduling, PID/ulimit or unlimited swap settings cannot be preserved; no destination container was created", ErrUnsupported)
	}
	if len(host.ExtraHosts)+len(host.GroupAdd) > 0 || len(host.LogConfig.Config) > 0 ||
		host.LogConfig.Type != "" && host.LogConfig.Type != "json-file" {
		return CreateContainerRequest{}, ContainerUpdate{}, fmt.Errorf("%w: additional hosts/groups or custom logging settings cannot be preserved; no destination container was created", ErrUnsupported)
	}
	for _, environment := range source.Config.Env {
		key, _, _ := strings.Cut(environment, "=")
		if sensitiveMigrationValue.MatchString(key) && !includeSensitive {
			return CreateContainerRequest{}, ContainerUpdate{}, errors.New("sensitive container environment requires explicit local-transfer consent; registry credentials are never copied")
		}
	}
	for key := range source.Config.Labels {
		if sensitiveMigrationValue.MatchString(key) && !includeSensitive {
			return CreateContainerRequest{}, ContainerUpdate{}, errors.New("sensitive container labels require explicit local-transfer consent; source credentials are never copied implicitly")
		}
	}
	restart := host.RestartPolicy.Name
	if restart == "on-failure" && host.RestartPolicy.MaximumRetryCount > 0 {
		restart = fmt.Sprintf("on-failure:%d", host.RestartPolicy.MaximumRetryCount)
	}
	request := CreateContainerRequest{
		Name: strings.TrimPrefix(source.Name, "/"), Image: source.Config.Image,
		Command: source.Config.Cmd, Entrypoint: source.Config.Entrypoint, Environment: source.Config.Env,
		Labels: source.Config.Labels, WorkingDir: source.Config.WorkingDir, User: source.Config.User,
		Hostname: source.Config.Hostname, StopSignal: source.Config.StopSignal, StopTimeout: source.Config.StopTimeout,
		Healthcheck: source.Config.Healthcheck, Privileged: host.Privileged, SecurityOpt: host.SecurityOpt,
		Tmpfs: host.Tmpfs, Sysctls: host.Sysctls, ShmSize: host.ShmSize, Userns: host.UsernsMode,
		Cgroupns: host.CgroupnsMode, Restart: restart, TTY: source.Config.Tty, Interactive: source.Config.OpenStdin,
		Expose: slices.Sorted(maps.Keys(source.Config.ExposedPorts)),
	}
	if host.Init != nil {
		request.Init = *host.Init
	}
	for _, mount := range source.Mounts {
		if mount.Type == "tmpfs" {
			if _, ok := request.Tmpfs[mount.Destination]; ok {
				continue
			}
		}
		if mount.Type != "volume" || mount.Name == "" {
			return request, ContainerUpdate{}, fmt.Errorf("%w: only selected named-volume mounts can be recreated safely", ErrUnsupported)
		}
		mode := "rw"
		if !mount.RW {
			mode = "ro"
		}
		request.Volumes = append(request.Volumes, mount.Name+":"+mount.Destination+":"+mode+",nocopy")
	}
	for _, port := range slices.Sorted(maps.Keys(host.PortBindings)) {
		bindings := host.PortBindings[port]
		for _, binding := range bindings {
			if binding.HostPort == "" || binding.HostPort == "0" {
				return request, ContainerUpdate{}, fmt.Errorf("%w: unallocated published ports cannot be preserved; configure an explicit source host port before migration", ErrUnsupported)
			}
			hostIP := binding.HostIP
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			request.Publish = append(request.Publish, net.JoinHostPort(hostIP, binding.HostPort)+":"+port)
		}
	}
	for _, network := range slices.Sorted(maps.Keys(source.NetworkSettings.Networks)) {
		endpoint := source.NetworkSettings.Networks[network]
		if len(endpoint.DriverOpts) > 0 || endpoint.IPAMConfig != nil &&
			(endpoint.IPAMConfig.IPv4Address != "" || endpoint.IPAMConfig.IPv6Address != "" || len(endpoint.IPAMConfig.LinkLocalIPs) > 0) {
			return request, ContainerUpdate{}, fmt.Errorf("%w: static network addresses and endpoint driver options cannot be preserved", ErrUnsupported)
		}
		aliases := make([]string, 0)
		for _, alias := range endpoint.Aliases {
			if alias != source.ID && alias != source.ID[:min(12, len(source.ID))] {
				aliases = append(aliases, alias)
			}
		}
		request.Networks = append(request.Networks, ContainerNetwork{Name: network, Aliases: aliases})
	}
	if len(request.Networks) == 0 {
		mode := host.NetworkMode
		if mode == "" || mode == "default" {
			mode = "bridge"
		}
		request.Networks = []ContainerNetwork{{Name: mode}}
	}
	if err := errors.Join(validateHealthcheck(request.Healthcheck), validateRestartPolicy(request.Restart)); err != nil {
		return request, ContainerUpdate{}, err
	}
	if _, err := compatibilityContainerHostname(request); err != nil {
		return request, ContainerUpdate{}, err
	}
	if _, err := appendHealthcheckArgs(nil, request.Healthcheck); err != nil {
		return request, ContainerUpdate{}, err
	}
	return request, ContainerUpdate{Memory: host.Memory, MemorySwap: host.MemorySwap, NanoCPUs: host.NanoCpus}, nil
}

func (source *migrationClient) inspect(ctx context.Context, selection dataops.Selection) (json.RawMessage, error) {
	resourcePath := map[string]string{"image": "/images/", "volume": "/volumes/", "network": "/networks/", "container": "/containers/"}[selection.Kind]
	if resourcePath == "" {
		return nil, datafiles.ErrInvalid
	}
	identifier := selection.ID
	if selection.Kind == "volume" {
		identifier = selection.Name
	}
	suffix := ""
	if selection.Kind == "container" || selection.Kind == "image" {
		suffix = "/json"
	}
	var metadata json.RawMessage
	err := source.get(ctx, resourcePath+url.PathEscape(identifier)+suffix, nil, &metadata)
	return metadata, err
}

func migrationPlanDigest(metadata []byte) (string, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", fmt.Errorf("decode migration plan metadata: %w", err)
	}
	if fields, ok := value.(map[string]any); ok {
		canonicalMigrationMetadata(fields)
		if source, ok := fields["Source"].(map[string]any); ok {
			canonicalMigrationMetadata(source)
		}
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalMigrationMetadata(fields map[string]any) {
	for _, field := range []string{"RepoTags", "RepoDigests"} {
		if values, ok := fields[field].([]any); ok {
			stringsOnly := make([]string, 0, len(values))
			for _, value := range values {
				if text, ok := value.(string); ok {
					stringsOnly = append(stringsOnly, text)
				}
			}
			if len(stringsOnly) == len(values) {
				slices.Sort(stringsOnly)
				fields[field] = stringsOnly
			}
		}
	}
	mounts, ok := fields["Mounts"].([]any)
	if !ok {
		return
	}
	for _, entry := range mounts {
		mounted, ok := entry.(map[string]any)
		if !ok {
			return
		}
		if _, ok := mounted["Destination"].(string); !ok {
			return
		}
	}
	// Docker builds this observed list from a map; its order does not change mount relationships.
	slices.SortStableFunc(mounts, func(left, right any) int {
		return strings.Compare(left.(map[string]any)["Destination"].(string), right.(map[string]any)["Destination"].(string))
	})
}

func migrationReplayPlan(metadata json.RawMessage, request dataops.Request, kind string) (string, error) {
	if kind != "container" {
		return migrationPlanDigest(metadata)
	}
	selections := slices.Clone(request.Selections)
	for index := range selections {
		selections[index].Destination = firstNonEmpty(selections[index].Destination, selections[index].Name)
	}
	slices.SortFunc(selections, func(left, right dataops.Selection) int {
		return strings.Compare(left.Kind+"\x00"+left.ID, right.Kind+"\x00"+right.ID)
	})
	document, err := json.Marshal(struct {
		Source     json.RawMessage
		Selections []dataops.Selection
	}{metadata, selections})
	if err != nil {
		return "", err
	}
	return migrationPlanDigest(document)
}

func migrationContainerImage(request dataops.Request, source migrationContainer) (string, error) {
	for _, selection := range request.Selections {
		if selection.Kind == "image" && source.Image != "" && selection.ID == source.Image {
			if isImageDigest(selection.Name) {
				return "", fmt.Errorf("%w: container recreation needs a selected named reference for its exact image; tag the original image explicitly before retrying", ErrUnsupported)
			}
			return selection.Name, nil
		}
	}
	return "", fmt.Errorf("%w: select the container's exact original image %s; migration never pulls a replacement", ErrUnsupported, source.Image)
}

func (m *Manager) MigrationInventory(ctx context.Context, contextName string) (MigrationInventory, error) {
	source, err := m.migrationSource(ctx, contextName)
	if err != nil {
		return MigrationInventory{}, err
	}
	inventory := MigrationInventory{Context: source.context, Objects: make([]MigrationObject, 0), Warnings: []string{
		"Source contexts, registry credentials, original images, containers and volumes are never deleted or reconfigured.",
		"Running volume writers are not stopped; transfers are crash-consistent, not database-consistent.",
	}}
	var images []struct {
		ID       string `json:"Id"`
		RepoTags []string
		Size     int64
	}
	if err := source.get(ctx, "/images/json", url.Values{"all": {"true"}}, &images); err != nil {
		return inventory, err
	}
	for _, image := range images {
		name := image.ID
		tags := slices.DeleteFunc(slices.Clone(image.RepoTags), func(tag string) bool { return tag == "" || tag == "<none>:<none>" })
		slices.Sort(tags)
		if len(tags) > 0 {
			name = tags[0]
		}
		item := MigrationObject{Kind: "image", Name: name, ID: image.ID, Size: image.Size, Supported: len(tags) > 0}
		if !item.Supported {
			item.Message = "Tag this source image explicitly before migration; an unnamed import must not overwrite another unnamed destination image."
		}
		inventory.Objects = append(inventory.Objects, item)
	}
	var containers []struct {
		ID     string `json:"Id"`
		Names  []string
		State  string
		Labels map[string]string
	}
	if err := source.get(ctx, "/containers/json", url.Values{"all": {"true"}}, &containers); err != nil {
		return inventory, err
	}
	for _, container := range containers {
		name := container.ID
		if len(container.Names) > 0 {
			name = strings.TrimPrefix(container.Names[0], "/")
		}
		item := MigrationObject{Kind: "container", Name: name, ID: container.ID, Supported: container.State != "running" && container.State != "paused",
			ComposeProject: container.Labels["com.docker.compose.project"], ComposeService: container.Labels["com.docker.compose.service"]}
		if !item.Supported {
			item.Message = "Stop the source container yourself before recreating it; Porto never stops a source workload."
		}
		inventory.Objects = append(inventory.Objects, item)
	}
	var volumes struct {
		Volumes []struct {
			Name, Driver, CreatedAt, Mountpoint string
			Labels                              map[string]string
			Options                             map[string]string
		}
	}
	if err := source.get(ctx, "/volumes", nil, &volumes); err != nil {
		return inventory, err
	}
	for _, volume := range volumes.Volumes {
		digest := sha256.Sum256([]byte(volume.Name + "\x00" + volume.CreatedAt + "\x00" + volume.Mountpoint))
		item := MigrationObject{Kind: "volume", Name: volume.Name, ID: hex.EncodeToString(digest[:]), Supported: volume.Driver == "local" && len(volume.Options) == 0}
		if !item.Supported {
			item.Message = "External volume drivers/options require an administrator-managed local archive."
		}
		inventory.Objects = append(inventory.Objects, item)
	}
	var networks []struct {
		ID           string `json:"Id"`
		Name, Driver string
		Internal     bool
	}
	if err := source.get(ctx, "/networks", nil, &networks); err != nil {
		return inventory, err
	}
	for _, network := range networks {
		if slices.Contains([]string{"bridge", "host", "none"}, network.Name) {
			continue
		}
		inventory.Objects = append(inventory.Objects, MigrationObject{Kind: "network", Name: network.Name, ID: network.ID, Supported: network.Driver == "bridge"})
	}
	return inventory, nil
}

func (m *Manager) PreviewMigration(ctx context.Context, request dataops.Request) (MigrationPreview, error) {
	preview := MigrationPreview{Request: request, Objects: make([]MigrationObject, 0), Conflicts: make([]string, 0), Warnings: make([]string, 0),
		SourceHelpers: make([]string, 0),
		SourcePolicy:  "Original source volumes, images, containers, contexts and credentials remain unchanged. No source workload is started/stopped. Explicit helper consent permits only an empty temporary image and a never-started container with a read-only/no-copy volume mount; owned helpers are removed after transfer."}
	if request.Context == "" || len(request.Selections) == 0 {
		return preview, fmt.Errorf("%w: choose a source context and individual resources", datafiles.ErrInvalid)
	}
	sources, destinations := make(map[string]bool), make(map[string]bool)
	for _, selection := range request.Selections {
		destination := firstNonEmpty(selection.Destination, selection.Name)
		sourceKey, destinationKey := selection.Kind+"\x00"+selection.ID, selection.Kind+"\x00"+destination
		if sources[sourceKey] {
			preview.Conflicts = append(preview.Conflicts, selection.Kind+" "+selection.Name+" is selected more than once")
		}
		if destinations[destinationKey] {
			preview.Conflicts = append(preview.Conflicts, "multiple selected "+selection.Kind+" objects target destination "+destination)
		}
		sources[sourceKey], destinations[destinationKey] = true, true
		if err := validateObjectID(destination); err != nil {
			preview.Conflicts = append(preview.Conflicts, "invalid destination for "+selection.Kind+" "+selection.Name+": "+err.Error())
		}
		if selection.Kind == "volume" && !volumeNamePattern.MatchString(destination) {
			preview.Conflicts = append(preview.Conflicts, "invalid destination volume name "+destination)
		}
		if selection.Kind == "image" && destination != selection.Name {
			preview.Conflicts = append(preview.Conflicts, "image "+selection.Name+": archive tags are preserved; rename the image explicitly after migration")
		}
	}
	inventory, err := m.MigrationInventory(ctx, request.Context)
	if err != nil {
		return preview, err
	}
	source, err := m.migrationSource(ctx, request.Context)
	if err != nil {
		return preview, err
	}
	targetVolumes, err := m.Volumes(ctx)
	if err != nil {
		return preview, err
	}
	targetNetworks, err := m.Networks(ctx)
	if err != nil {
		return preview, err
	}
	targetContainers, err := m.Containers(ctx)
	if err != nil {
		return preview, err
	}
	backend, err := m.backend(ctx)
	if err != nil {
		return preview, err
	}
	rootless := backend.limaInstance != "" || runtime.GOOS == "linux" && os.Geteuid() != 0
	for _, selection := range request.Selections {
		found := false
		for _, object := range inventory.Objects {
			if selection.Kind != object.Kind || selection.ID != object.ID || selection.Name != object.Name {
				continue
			}
			found = true
			destination := firstNonEmpty(selection.Destination, selection.Name)
			if !object.Supported {
				preview.Conflicts = append(preview.Conflicts, object.Kind+" "+object.Name+": "+object.Message)
			}
			metadata, err := source.inspect(ctx, selection)
			if err != nil {
				return preview, err
			}
			object.PlanDigest, err = migrationPlanDigest(metadata)
			if err != nil {
				return preview, err
			}
			replayPlan, err := migrationReplayPlan(metadata, request, object.Kind)
			if err != nil {
				return preview, err
			}
			switch object.Kind {
			case "image":
				var image struct {
					ID       string `json:"Id"`
					RepoTags []string
				}
				if err := json.Unmarshal(metadata, &image); err != nil {
					return preview, err
				}
				if image.ID != object.ID {
					return preview, datafiles.ErrConflict
				}
				for _, tag := range image.RepoTags {
					if tag == "<none>:<none>" {
						continue
					}
					matches, inspectErr := m.migrationImageMatches(ctx, tag, metadata)
					if inspectErr != nil {
						if missingDockerObject(inspectErr, "image") {
							continue
						}
						return preview, inspectErr
					}
					if !matches {
						preview.Conflicts = append(preview.Conflicts, "image archive tag "+tag+" conflicts with an existing destination digest")
					}
				}
			case "volume":
				var volume struct{ Name, Driver, CreatedAt, Mountpoint string }
				if err := json.Unmarshal(metadata, &volume); err != nil {
					return preview, err
				}
				container, _, err := source.findVolumeContainer(ctx, object.Name)
				if err != nil {
					return preview, err
				}
				if container == "" {
					preview.SourceHelpers = append(preview.SourceHelpers, object.Name)
					if !request.AllowSourceHelper {
						preview.Conflicts = append(preview.Conflicts, object.Name+": enable explicit read-only temporary source-helper access to migrate this unattached volume")
					} else {
						preview.Warnings = append(preview.Warnings, object.Name+": temporary empty image and stopped helper container will be created, never started, mounted read-only/no-copy, and removed after transfer")
					}
				}
				for _, target := range targetVolumes {
					if target.Name != destination {
						continue
					}
					known := false
					if m.migrationVolumeKnown != nil {
						descriptor, err := m.FileDescriptor(ctx, "volume", destination)
						if err != nil {
							return preview, err
						}
						known, err = m.migrationVolumeKnown(ctx, request.Context, object.ID, destination, descriptor.Resource.Fingerprint())
						if err != nil {
							return preview, err
						}
					}
					if !known {
						preview.Conflicts = append(preview.Conflicts, "destination volume "+destination+" already exists and is not a verified migration of this source")
					}
				}
			case "network":
				for _, target := range targetNetworks {
					if target.Name == destination && (target.Labels[migrationSourceLabel] != migrationIdentity(request.Context, "network", object.ID) || target.Labels[migrationPlanLabel] != replayPlan) {
						preview.Conflicts = append(preview.Conflicts, "destination network "+destination+" already exists without this verified source configuration")
					}
				}
			case "container":
				for _, target := range targetContainers {
					if strings.TrimPrefix(target.Name, "/") == destination && (target.Labels[migrationSourceLabel] != migrationIdentity(request.Context, "container", object.ID) || target.Labels[migrationPlanLabel] != replayPlan) {
						preview.Conflicts = append(preview.Conflicts, "destination container "+destination+" already exists without this verified source configuration and dependency selection")
					}
				}
			}
			if object.Kind == "container" {
				configuration, _, err := migrationContainerRequest(metadata, request.IncludeSensitive)
				if err != nil {
					preview.Conflicts = append(preview.Conflicts, object.Name+": "+err.Error())
				} else {
					var inspected migrationContainer
					if err := json.Unmarshal(metadata, &inspected); err != nil {
						return preview, err
					}
					if _, err := migrationContainerImage(request, inspected); err != nil {
						preview.Conflicts = append(preview.Conflicts, object.Name+": "+err.Error())
					}
					for _, mounted := range configuration.Volumes {
						name, _, _ := strings.Cut(mounted, ":")
						if !slices.ContainsFunc(request.Selections, func(item dataops.Selection) bool { return item.Kind == "volume" && item.Name == name }) {
							preview.Conflicts = append(preview.Conflicts, object.Name+": select its named volume "+name+" first")
						}
					}
					for _, network := range configuration.Networks {
						if slices.Contains([]string{"bridge", "host", "none"}, network.Name) {
							continue
						}
						index := slices.IndexFunc(request.Selections, func(item dataops.Selection) bool { return item.Kind == "network" && item.Name == network.Name })
						if index < 0 {
							preview.Conflicts = append(preview.Conflicts, object.Name+": select its custom network "+network.Name+" first")
						} else if rootless && len(configuration.Publish) > 0 {
							var metadata struct{ Internal bool }
							if err := source.get(ctx, "/networks/"+url.PathEscape(request.Selections[index].ID), nil, &metadata); err != nil {
								return preview, err
							}
							if metadata.Internal {
								preview.Conflicts = append(preview.Conflicts, object.Name+": rootless published ports on internal networks cannot be preserved; select a supported non-internal network configuration explicitly before migration")
							}
						}
					}
				}
			}
			preview.Objects = append(preview.Objects, object)
		}
		if !found {
			return preview, fmt.Errorf("%w: source identity changed for %s", datafiles.ErrConflict, selection.Name)
		}
	}
	preview.Warnings = append(preview.Warnings, inventory.Warnings...)
	signature, err := json.Marshal(struct {
		Context           string
		Objects           []MigrationObject
		Selections        []dataops.Selection
		Sensitive, Helper bool
	}{
		request.Context, preview.Objects, request.Selections, request.IncludeSensitive, request.AllowSourceHelper})
	if err != nil {
		return preview, err
	}
	hash := sha256.Sum256(signature)
	preview.Token = hex.EncodeToString(hash[:])
	preview.Request.Action, preview.Request.Preview = "migration", preview.Token
	return preview, nil
}

func migrationIdentity(contextName, kind, id string) string {
	hash := sha256.Sum256([]byte(contextName + "\x00" + kind + "\x00" + id))
	return hex.EncodeToString(hash[:])
}

func (m *Manager) Migrate(ctx context.Context, request dataops.Request, directory string, progress func(string, int64) error, checkpoint func(dataops.Result) error) (result dataops.Result, err error) {
	if !request.Confirm || request.Preview == "" {
		return result, datafiles.ErrInvalid
	}
	if checkpoint == nil {
		return result, errors.New("migration checkpoint persistence is unavailable; nothing was transferred")
	}
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	preview, err := m.PreviewMigration(ctx, request)
	if err != nil {
		return result, err
	}
	if preview.Token != request.Preview {
		return result, datafiles.ErrConflict
	}
	if len(preview.Conflicts) > 0 {
		return result, fmt.Errorf("%w: resolve the dry-run conflicts before migration", ErrUnsupported)
	}
	source, err := m.migrationSource(ctx, request.Context)
	if err != nil {
		return result, err
	}
	selections := append([]dataops.Selection(nil), request.Selections...)
	order := map[string]int{"image": 0, "volume": 1, "network": 2, "container": 3}
	slices.SortStableFunc(selections, func(left, right dataops.Selection) int { return order[left.Kind] - order[right.Kind] })
	result.Message = "Successful destination resources remain; originals in the source are unchanged. Review a new dry run to resume pending, failed or interrupted objects."
	result.Steps = make([]dataops.Step, len(selections))
	plans := make(map[string]string, len(preview.Objects))
	for _, object := range preview.Objects {
		plans[object.Kind+"\x00"+object.ID] = object.PlanDigest
	}
	for index, selection := range selections {
		destination := firstNonEmpty(selection.Destination, selection.Name)
		result.Steps[index] = dataops.Step{Kind: selection.Kind, Source: selection.Name, Destination: destination, ID: selection.ID, Status: "pending"}
	}
	if err := checkpoint(result); err != nil {
		return result, fmt.Errorf("persist migration plan before transfer: %w", err)
	}
	for index, selection := range selections {
		step := &result.Steps[index]
		step.Status = "running"
		if err := checkpoint(result); err != nil {
			step.Status, step.Message = "pending", "Transfer was not started because its checkpoint could not be saved."
			return result, fmt.Errorf("persist migration object before transfer: %w", err)
		}
		err = ctx.Err()
		if err == nil {
			err = progress("Migrating "+selection.Kind+" "+selection.Name, 0)
		}
		var metadata json.RawMessage
		if err == nil {
			metadata, err = source.inspect(ctx, selection)
			if err == nil {
				var current string
				current, err = migrationPlanDigest(metadata)
				if err == nil && current != plans[selection.Kind+"\x00"+selection.ID] {
					err = fmt.Errorf("%w: source configuration changed after confirmation; review a new dry run", datafiles.ErrConflict)
				}
			}
		}
		if err == nil {
			switch selection.Kind {
			case "image":
				err = m.migrateImage(ctx, source, selection, directory, progress)
			case "volume":
				err = m.migrateVolume(ctx, source, request, selection, step.Destination, directory, progress)
			case "network":
				err = m.migrateNetwork(ctx, source, selection, step.Destination, metadata)
			case "container":
				err = m.migrateContainer(ctx, source, request, selection, step.Destination, metadata)
			default:
				err = datafiles.ErrInvalid
			}
		}
		step.Status = "succeeded"
		if err != nil {
			step.Status, step.Message = "failed", err.Error()
			if errors.Is(err, context.Canceled) {
				step.Status = "cancelled"
			}
		}
		persistErr := checkpoint(result)
		if err != nil || persistErr != nil {
			if persistErr != nil {
				persistErr = fmt.Errorf("persist migration object outcome: %w", persistErr)
			}
			return result, errors.Join(err, persistErr)
		}
	}
	result.Message = "Selected local data migrated and validated. Source context and original resources are unchanged; remove the source runtime yourself only after validation."
	return result, checkpoint(result)
}

func (m *Manager) migrateImage(ctx context.Context, source *migrationClient, selection dataops.Selection, directory string, progress func(string, int64) error) (err error) {
	var sourceDocument json.RawMessage
	if err := source.get(ctx, "/images/"+url.PathEscape(selection.ID)+"/json", nil, &sourceDocument); err != nil {
		return err
	}
	var metadata struct {
		ID                        string `json:"Id"`
		RepoTags                  []string
		Architecture, Os, Variant string
	}
	if err := json.Unmarshal(sourceDocument, &metadata); err != nil {
		return err
	}
	if metadata.ID != selection.ID {
		return datafiles.ErrConflict
	}
	tags := slices.DeleteFunc(slices.Clone(metadata.RepoTags), func(tag string) bool { return tag == "" || tag == "<none>:<none>" })
	if len(tags) == 0 {
		return fmt.Errorf("%w: tag the source image explicitly before migration", ErrUnsupported)
	}
	existing, inspectErr := m.migrationImageMatches(ctx, selection.Name, sourceDocument)
	if inspectErr == nil {
		if existing {
			for _, tag := range tags {
				matches, err := m.migrationImageMatches(ctx, tag, sourceDocument)
				if missingDockerObject(err, "image") {
					if err := m.TagImage(ctx, selection.Name, tag); err != nil {
						return err
					}
				} else if err != nil {
					return err
				} else if !matches {
					return fmt.Errorf("%w: destination image alias %s has another digest", ErrConflict, tag)
				}
			}
			return nil
		}
		return fmt.Errorf("%w: destination image name already has a different digest", ErrConflict)
	}
	if !missingDockerObject(inspectErr, "image") {
		return inspectErr
	}
	file, err := os.CreateTemp(directory, "migration-image-*.tar")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close(), os.Remove(file.Name())) }()
	response, err := source.request(ctx, http.MethodGet, "/images/get", url.Values{"names": tags}, nil)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(&transferWriter{ctx: ctx, output: file, progress: progress, phase: "Copying source image archive"}, response.Body)
	if err := errors.Join(copyErr, response.Body.Close()); err != nil {
		return err
	}
	expectedConfig, err := imageArchiveConfigDigest(file, selection.ID, ocispec.Platform{OS: metadata.Os, Architecture: metadata.Architecture, Variant: metadata.Variant})
	if err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := m.StreamLoadImages(ctx, file, true, func(chunk runtimes.OutputChunk) error { return progress("Loading checksum-validated image content", 0) }); err != nil {
		return err
	}
	for _, tag := range tags {
		loaded, err := m.migrationImageConfigDigest(ctx, tag)
		if err != nil {
			return err
		}
		if loaded != expectedConfig {
			return fmt.Errorf("destination image config digest %s does not match verified source config %s", loaded, expectedConfig)
		}
	}
	return nil
}

func (m *Manager) migrationImageMatches(ctx context.Context, name string, source json.RawMessage) (bool, error) {
	var expected struct {
		ID                        string `json:"Id"`
		Descriptor                ocispec.Descriptor
		Architecture, Os, Variant string
	}
	if err := json.Unmarshal(source, &expected); err != nil {
		return false, err
	}
	if expected.Descriptor.Digest == "" {
		digest, err := m.migrationImageConfigDigest(ctx, name)
		return digest == expected.ID, err
	}
	if !m.directCLI {
		if expected.Architecture == "" || expected.Os == "" || expected.Descriptor.MediaType == "" {
			return false, errors.New("source OCI image identity metadata is incomplete")
		}
		// Inspectors can report unpacked layer digests instead of original OCI diff IDs.
		// Compare config bytes pinned by the source manifest and the current image.
		return m.migrationOCIImageMatches(ctx, name, expected.Descriptor, ocispec.Platform{
			OS: expected.Os, Architecture: expected.Architecture, Variant: expected.Variant,
		})
	}
	current, err := m.InspectImage(ctx, name, "")
	if err != nil {
		return false, err
	}
	type content struct {
		Architecture, Os, Variant, Author string
		Created                           time.Time
		Config                            map[string]any
		RootFS                            struct {
			Type   string
			Layers []string
		}
	}
	var left, right content
	if err := json.Unmarshal(source, &left); err != nil {
		return false, err
	}
	if err := json.Unmarshal(current, &right); err != nil {
		return false, err
	}
	if left.Architecture == "" || left.Os == "" || left.RootFS.Type != "layers" || left.Config == nil {
		return false, errors.New("source image content metadata is incomplete")
	}
	for _, config := range []map[string]any{left.Config, right.Config} {
		// Inspector-only parent-image hints are not part of the OCI execution configuration.
		delete(config, "Image")
		for key, value := range config {
			empty := value == nil
			switch typed := value.(type) {
			case bool:
				empty = !typed
			case string:
				empty = typed == ""
			case float64:
				empty = typed == 0
			case []any:
				empty = len(typed) == 0
			case map[string]any:
				empty = len(typed) == 0
			}
			if empty {
				delete(config, key)
			}
		}
	}
	if !left.Created.Equal(right.Created) {
		return false, nil
	}
	left.Created, right.Created = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right), nil
}

func (m *Manager) migrationOCIImageMatches(ctx context.Context, name string, source ocispec.Descriptor, platform ocispec.Platform) (matches bool, err error) {
	client, closeRuntime, err := m.storageRuntime(ctx)
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, closeRuntime()) }()
	backend, ok := client.(*grpcContainerRuntime)
	if !ok || backend.client == nil {
		return false, ErrUnsupported
	}
	named, err := reference.ParseDockerRef(normalizeNerdctlReference(name))
	if err != nil {
		return false, err
	}
	ctx = namespaces.WithNamespace(ctx, backend.namespace)
	image, err := backend.client.GetImage(ctx, named.String())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, fmt.Errorf("%w: image %q: %v", ErrNotFound, name, err)
		}
		return false, err
	}
	current, err := image.Config(ctx)
	if err != nil {
		return false, err
	}
	expected, err := images.Config(ctx, backend.client.ContentStore(), source, platforms.Only(platform))
	if err != nil {
		return false, fmt.Errorf("resolve pinned source OCI image configuration: %w", err)
	}
	return current.Digest == expected.Digest, nil
}

func (m *Manager) migrationImageConfigDigest(ctx context.Context, name string) (digest string, err error) {
	if m.directCLI {
		document, err := m.InspectImage(ctx, name, "")
		if err != nil {
			return "", err
		}
		var image struct {
			ID string `json:"Id"`
		}
		if err := json.Unmarshal(document, &image); err != nil {
			return "", err
		}
		return image.ID, nil
	}
	client, closeRuntime, err := m.storageRuntime(ctx)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, closeRuntime()) }()
	backend, ok := client.(*grpcContainerRuntime)
	if !ok || backend.client == nil {
		return "", ErrUnsupported
	}
	named, err := reference.ParseDockerRef(normalizeNerdctlReference(name))
	if err != nil {
		return "", err
	}
	ctx = namespaces.WithNamespace(ctx, backend.namespace)
	image, err := backend.client.GetImage(ctx, named.String())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return "", fmt.Errorf("%w: image %q: %v", ErrNotFound, name, err)
		}
		return "", err
	}
	// Docker identifies image configuration bytes; containerd's target identifies its manifest.
	config, err := image.Config(ctx)
	if err != nil {
		return "", err
	}
	return config.Digest.String(), nil
}

func (m *Manager) migrateNetwork(ctx context.Context, source *migrationClient, selection dataops.Selection, destination string, document json.RawMessage) error {
	identity := migrationIdentity(source.context.Name, "network", selection.ID)
	plan, err := migrationPlanDigest(document)
	if err != nil {
		return err
	}
	existing, err := m.Networks(ctx)
	if err != nil {
		return err
	}
	for _, network := range existing {
		if network.Name != destination {
			continue
		}
		document, err := m.InspectNetwork(ctx, destination)
		if err != nil {
			return err
		}
		var metadata struct{ Labels map[string]string }
		if err := json.Unmarshal(document, &metadata); err != nil {
			return err
		}
		if metadata.Labels[migrationSourceLabel] == identity && metadata.Labels[migrationPlanLabel] == plan {
			return nil
		}
		return fmt.Errorf("%w: destination network %s exists", ErrConflict, destination)
	}
	var network struct {
		ID                   string `json:"Id"`
		Name, Driver         string
		Internal, EnableIPv6 bool
		Labels, Options      map[string]string
		IPAM                 struct {
			Driver string
			Config []struct{ Subnet, Gateway string }
		}
	}
	if err := json.Unmarshal(document, &network); err != nil {
		return err
	}
	if network.ID != selection.ID || network.Driver != "bridge" || network.IPAM.Driver != "" && network.IPAM.Driver != "default" {
		return ErrUnsupported
	}
	labels := cloneStringMap(network.Labels)
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[migrationSourceLabel] = identity
	labels[migrationPlanLabel] = plan
	request := CreateNetworkRequest{Name: destination, Driver: "bridge", Internal: network.Internal, EnableIPv6: network.EnableIPv6, Labels: labels, Options: network.Options}
	for _, config := range network.IPAM.Config {
		request.Subnets, request.Gateways = append(request.Subnets, config.Subnet), append(request.Gateways, config.Gateway)
	}
	_, err = m.CreateNetwork(ctx, request)
	return err
}

func (m *Manager) migrateContainer(ctx context.Context, source *migrationClient, request dataops.Request, selection dataops.Selection, destination string, document json.RawMessage) error {
	identity := migrationIdentity(source.context.Name, "container", selection.ID)
	plan, err := migrationReplayPlan(document, request, "container")
	if err != nil {
		return err
	}
	var inspected migrationContainer
	if err := json.Unmarshal(document, &inspected); err != nil {
		return err
	}
	if inspected.ID != selection.ID {
		return datafiles.ErrConflict
	}
	containers, err := m.Containers(ctx)
	if err != nil {
		return err
	}
	for _, container := range containers {
		if strings.TrimPrefix(container.Name, "/") == destination {
			if container.Labels[migrationSourceLabel] == identity && container.Labels[migrationPlanLabel] == plan {
				if inspected.HostConfig.Memory != 0 || inspected.HostConfig.MemorySwap != 0 || inspected.HostConfig.NanoCpus != 0 {
					document, err := m.InspectContainer(ctx, container.ID)
					if err != nil {
						return err
					}
					var current migrationContainer
					if err := json.Unmarshal(document, &current); err != nil {
						return err
					}
					cpuMatches := current.HostConfig.NanoCpus == inspected.HostConfig.NanoCpus
					period := current.HostConfig.CpuPeriod
					if !cpuMatches && current.HostConfig.NanoCpus == 0 && inspected.HostConfig.NanoCpus > 0 && period > 0 && period <= 1_000_000 {
						nano := inspected.HostConfig.NanoCpus
						expectedQuota := (nano/1_000_000_000)*period + (nano%1_000_000_000)*period/1_000_000_000
						cpuMatches = current.HostConfig.CpuQuota == expectedQuota
					}
					if current.HostConfig.Memory != inspected.HostConfig.Memory ||
						current.HostConfig.MemorySwap != inspected.HostConfig.MemorySwap || !cpuMatches {
						return fmt.Errorf("%w: migrated container resource limits are incomplete or changed; inspect the destination explicitly before retrying", datafiles.ErrConflict)
					}
				}
				return nil
			}
			return fmt.Errorf("%w: destination container name exists", ErrConflict)
		}
	}
	configuration, update, err := migrationContainerRequest(document, request.IncludeSensitive)
	if err != nil {
		return err
	}
	configuration.Image, err = migrationContainerImage(request, inspected)
	if err != nil {
		return err
	}
	var sourceImage json.RawMessage
	if err := source.get(ctx, "/images/"+url.PathEscape(inspected.Image)+"/json", nil, &sourceImage); err != nil {
		return err
	}
	currentImage, err := m.migrationImageMatches(ctx, configuration.Image, sourceImage)
	if err != nil {
		return err
	}
	if !currentImage {
		return fmt.Errorf("%w: destination image no longer matches the source container; no container was created", datafiles.ErrConflict)
	}
	configuration.Name = destination
	if configuration.Labels == nil {
		configuration.Labels = make(map[string]string)
	}
	configuration.Labels[migrationSourceLabel] = identity
	configuration.Labels[migrationPlanLabel] = plan
	for index, mounted := range configuration.Volumes {
		sourceName, suffix, _ := strings.Cut(mounted, ":")
		for _, choice := range request.Selections {
			if choice.Kind == "volume" && choice.Name == sourceName {
				configuration.Volumes[index] = firstNonEmpty(choice.Destination, choice.Name) + ":" + suffix
			}
		}
	}
	for index, network := range configuration.Networks {
		for _, choice := range request.Selections {
			if choice.Kind == "network" && choice.Name == network.Name {
				configuration.Networks[index].Name = firstNonEmpty(choice.Destination, choice.Name)
			}
		}
	}
	id, err := m.CreateContainer(ctx, configuration)
	if err != nil {
		return err
	}
	if update.Memory != 0 || update.MemorySwap != 0 || update.NanoCPUs != 0 {
		if err := m.UpdateContainer(ctx, id, update); err != nil {
			cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			return errors.Join(err, m.ContainerAction(cleanupContext, id, "remove"))
		}
	}
	return nil
}

func (m *Manager) migrateVolume(ctx context.Context, source *migrationClient, request dataops.Request, selection dataops.Selection, destination, directory string, progress func(string, int64) error) (err error) {
	document, existingErr := m.InspectVolume(ctx, destination)
	if existingErr == nil {
		var existing struct{ Labels map[string]string }
		if err := json.Unmarshal(document, &existing); err != nil {
			return err
		}
		if m.migrationVolumeKnown != nil {
			descriptor, err := m.FileDescriptor(ctx, "volume", destination)
			if err != nil {
				return err
			}
			known, err := m.migrationVolumeKnown(ctx, source.context.Name, selection.ID, destination, descriptor.Resource.Fingerprint())
			if err != nil {
				return err
			}
			if known {
				return nil
			}
		}
		return fmt.Errorf("%w: destination volume exists", ErrConflict)
	}
	if !missingDockerObject(existingErr, "volume") {
		return existingErr
	}
	if m.migrationVolumeRecorded == nil {
		return errors.New("migration volume ledger is unavailable; no destination volume was created")
	}
	var volume struct {
		Name, Driver, CreatedAt, Mountpoint string
		Labels                              map[string]string
	}
	if err := source.get(ctx, "/volumes/"+url.PathEscape(selection.Name), nil, &volume); err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(volume.Name + "\x00" + volume.CreatedAt + "\x00" + volume.Mountpoint))
	if hex.EncodeToString(hash[:]) != selection.ID {
		return datafiles.ErrConflict
	}
	resource := datafiles.Resource{Kind: "volume", Name: selection.Name, ID: selection.ID, CreatedAt: volume.CreatedAt, Backend: source.context.Name}
	file, err := os.CreateTemp(directory, "migration-volume-*.tar")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close(), os.Remove(file.Name())) }()
	stream, cleanup, err := source.openVolumeArchive(ctx, request, selection.Name, selection.ID, m.migrationTemporaryReserve, m.migrationTemporaryClear)
	if err != nil {
		return err
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		err = errors.Join(err, cleanup(cleanupContext))
	}()
	_, sealErr := datafiles.SealTar(ctx, stream, &transferWriter{ctx: ctx, output: file, progress: progress, phase: "Copying read-only source volume archive"}, resource)
	if err := errors.Join(sealErr, stream.Close()); err != nil {
		return err
	}
	archive, err := InspectVolumeArchive(ctx, file.Name())
	if err != nil {
		return err
	}
	_, err = m.importVolume(ctx, destination, file.Name(), archive.SHA256, progress, volume.Labels, func(resource datafiles.Resource) error {
		return m.migrationVolumeRecorded(ctx, source.context.Name, selection.ID, destination, resource.Fingerprint())
	})
	return err
}

func (m *Manager) SetMigrationVolumeLedger(
	record func(context.Context, string, string, string, string) error,
	known func(context.Context, string, string, string, string) (bool, error),
) {
	m.migrationVolumeRecorded, m.migrationVolumeKnown = record, known
}
