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
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/runtimes"
)

const migrationSourceLabel = "io.porto.migration.source"

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
	Request      dataops.Request   `json:"request"`
	Objects      []MigrationObject `json:"objects"`
	Conflicts    []string          `json:"conflicts"`
	Warnings     []string          `json:"warnings"`
	Token        string            `json:"token"`
	SourcePolicy string            `json:"sourcePolicy"`
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
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("source runtime request failed: %w", err)
	}
	if response.StatusCode >= 300 {
		defer response.Body.Close()
		var message struct{ Message string }
		if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&message); err != nil {
			return nil, fmt.Errorf("source runtime rejected request with HTTP %d", response.StatusCode)
		}
		return nil, fmt.Errorf("source runtime: %s", message.Message)
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
	Created string
	State   struct{ Running, Paused, Restarting bool }
	Config  struct {
		Image, User, WorkingDir, Hostname, StopSignal string
		StopTimeout                                   *int
		Cmd, Entrypoint, Env                          []string
		Labels                                        map[string]string
		Tty, OpenStdin                                bool
		Healthcheck                                   *ContainerHealthcheck
	}
	HostConfig struct {
		Privileged, AutoRemove, ReadonlyRootfs                                    bool
		Init                                                                      *bool
		PidMode, IpcMode, UTSMode, UsernsMode, CgroupnsMode, Runtime, NetworkMode string
		Binds, VolumesFrom, Links, DNS, DNSSearch, DNSOptions, CapAdd, CapDrop    []string
		Devices                                                                   []json.RawMessage
		SecurityOpt                                                               []string
		Sysctls                                                                   map[string]string
		Tmpfs                                                                     map[string]string
		Memory, MemorySwap, NanoCpus, ShmSize                                     int64
		RestartPolicy                                                             struct {
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
			Aliases                         []string
			IPAddress, IPPrefixLen, Gateway string
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
		len(host.Binds) > 0 || len(host.VolumesFrom) > 0 || len(host.Links) > 0 ||
		len(host.DNS)+len(host.DNSSearch)+len(host.DNSOptions)+len(host.CapAdd)+len(host.CapDrop)+len(host.Devices) > 0 ||
		host.ReadonlyRootfs || host.UsernsMode != "" && host.UsernsMode != "host" ||
		host.Runtime != "" && host.Runtime != "runc" || host.AutoRemove {
		return CreateContainerRequest{}, ContainerUpdate{}, fmt.Errorf("%w: source has unsupported host paths, namespace, device, DNS, capability, runtime or rootfs options; no destination container was created", ErrUnsupported)
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
	}
	if host.Init != nil {
		request.Init = *host.Init
	}
	for _, mount := range source.Mounts {
		if mount.Type != "volume" || mount.Name == "" {
			return request, ContainerUpdate{}, fmt.Errorf("%w: only selected named-volume mounts can be recreated safely", ErrUnsupported)
		}
		mode := "rw"
		if !mount.RW {
			mode = "ro"
		}
		request.Volumes = append(request.Volumes, mount.Name+":"+mount.Destination+":"+mode)
	}
	for port, bindings := range host.PortBindings {
		for _, binding := range bindings {
			hostIP := binding.HostIP
			if hostIP == "" {
				hostIP = "0.0.0.0"
			}
			request.Publish = append(request.Publish, hostIP+":"+binding.HostPort+":"+port)
		}
	}
	for network, endpoint := range source.NetworkSettings.Networks {
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
	return request, ContainerUpdate{Memory: host.Memory, MemorySwap: host.MemorySwap, NanoCPUs: host.NanoCpus}, nil
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
		if len(image.RepoTags) > 0 {
			name = image.RepoTags[0]
		}
		inventory.Objects = append(inventory.Objects, MigrationObject{Kind: "image", Name: name, ID: image.ID, Size: image.Size, Supported: true})
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
		SourcePolicy: "Read-only source inventory and image/container archive APIs. No source context switch, workload start/stop, registry credential import, or original resource deletion. Optional temporary read-only helpers require separate explicit consent."}
	if request.Context == "" || len(request.Selections) == 0 {
		return preview, fmt.Errorf("%w: choose a source context and individual resources", datafiles.ErrInvalid)
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
			var metadata json.RawMessage
			resourcePath := map[string]string{"image": "/images/", "volume": "/volumes/", "network": "/networks/", "container": "/containers/"}[object.Kind]
			if resourcePath == "" {
				return preview, datafiles.ErrInvalid
			}
			identifier := object.ID
			if object.Kind == "volume" {
				identifier = object.Name
			}
			suffix := ""
			if object.Kind == "container" || object.Kind == "image" {
				suffix = "/json"
			}
			if err := source.get(ctx, resourcePath+url.PathEscape(identifier)+suffix, nil, &metadata); err != nil {
				return preview, err
			}
			digest := sha256.Sum256(metadata)
			object.PlanDigest = hex.EncodeToString(digest[:])
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
					document, inspectErr := m.InspectImage(ctx, tag, "")
					if inspectErr != nil {
						if missingDockerObject(inspectErr, "image") {
							continue
						}
						return preview, inspectErr
					}
					var current struct {
						ID string `json:"Id"`
					}
					if err := json.Unmarshal(document, &current); err != nil {
						return preview, err
					}
					if current.ID != object.ID {
						preview.Conflicts = append(preview.Conflicts, "image archive tag "+tag+" conflicts with an existing destination digest")
					}
				}
			case "volume":
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
					if target.Name == destination && target.Labels[migrationSourceLabel] != migrationIdentity(request.Context, "network", object.ID) {
						preview.Conflicts = append(preview.Conflicts, "destination network "+destination+" already exists")
					}
				}
			case "container":
				for _, target := range targetContainers {
					if strings.TrimPrefix(target.Name, "/") == destination && target.Labels[migrationSourceLabel] != migrationIdentity(request.Context, "container", object.ID) {
						preview.Conflicts = append(preview.Conflicts, "destination container "+destination+" already exists")
					}
				}
			}
			if object.Kind == "container" {
				configuration, _, err := migrationContainerRequest(metadata, request.IncludeSensitive)
				if err != nil {
					preview.Conflicts = append(preview.Conflicts, object.Name+": "+err.Error())
				} else {
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
						if !slices.ContainsFunc(request.Selections, func(item dataops.Selection) bool { return item.Kind == "network" && item.Name == network.Name }) {
							preview.Conflicts = append(preview.Conflicts, object.Name+": select its custom network "+network.Name+" first")
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

func (m *Manager) Migrate(ctx context.Context, request dataops.Request, directory string, progress func(string, int64) error) (result dataops.Result, err error) {
	if !request.Confirm || request.Preview == "" {
		return result, datafiles.ErrInvalid
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
	result.Steps = make([]dataops.Step, 0, len(selections))
	for _, selection := range selections {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := progress("Migrating "+selection.Kind+" "+selection.Name, 0); err != nil {
			return result, err
		}
		destination := firstNonEmpty(selection.Destination, selection.Name)
		step := dataops.Step{Kind: selection.Kind, Source: selection.Name, Destination: destination, Status: "succeeded"}
		switch selection.Kind {
		case "image":
			err = m.migrateImage(ctx, source, selection, directory, progress)
		case "volume":
			err = m.migrateVolume(ctx, source, request, selection, destination, directory, progress)
		case "network":
			err = m.migrateNetwork(ctx, source, selection, destination)
		case "container":
			err = m.migrateContainer(ctx, source, request, selection, destination)
		default:
			err = datafiles.ErrInvalid
		}
		if err != nil {
			step.Status, step.Message = "failed", err.Error()
			result.Steps = append(result.Steps, step)
			result.Message = "Partial migration is resumable. Successful destination resources remain; originals in the source are unchanged."
			return result, err
		}
		result.Steps = append(result.Steps, step)
	}
	result.Message = "Selected local data migrated and validated. Source context and original resources are unchanged; remove the source runtime yourself only after validation."
	return result, nil
}

func (m *Manager) migrateImage(ctx context.Context, source *migrationClient, selection dataops.Selection, directory string, progress func(string, int64) error) (err error) {
	document, inspectErr := m.InspectImage(ctx, selection.Name, "")
	if inspectErr == nil {
		var existing struct {
			ID string `json:"Id"`
		}
		if err := json.Unmarshal(document, &existing); err != nil {
			return err
		}
		if existing.ID == selection.ID {
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
	response, err := source.request(ctx, http.MethodGet, "/images/get", url.Values{"names": {selection.ID}}, nil)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(&transferWriter{ctx: ctx, output: file, progress: progress, phase: "Copying source image archive"}, response.Body)
	if err := errors.Join(copyErr, response.Body.Close()); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := m.StreamLoadImages(ctx, file, true, func(chunk runtimes.OutputChunk) error { return progress("Loading checksum-validated image content", 0) }); err != nil {
		return err
	}
	document, err = m.InspectImage(ctx, selection.ID, "")
	if err != nil {
		return err
	}
	var loaded struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(document, &loaded); err != nil {
		return err
	}
	if loaded.ID != selection.ID {
		return errors.New("destination image config digest does not match the source")
	}
	return nil
}

func (m *Manager) migrateNetwork(ctx context.Context, source *migrationClient, selection dataops.Selection, destination string) error {
	identity := migrationIdentity(source.context.Name, "network", selection.ID)
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
		if metadata.Labels[migrationSourceLabel] == identity {
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
	if err := source.get(ctx, "/networks/"+url.PathEscape(selection.ID), nil, &network); err != nil {
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
	request := CreateNetworkRequest{Name: destination, Driver: "bridge", Internal: network.Internal, EnableIPv6: network.EnableIPv6, Labels: labels, Options: network.Options}
	for _, config := range network.IPAM.Config {
		request.Subnets, request.Gateways = append(request.Subnets, config.Subnet), append(request.Gateways, config.Gateway)
	}
	_, err = m.CreateNetwork(ctx, request)
	return err
}

func (m *Manager) migrateContainer(ctx context.Context, source *migrationClient, request dataops.Request, selection dataops.Selection, destination string) error {
	identity := migrationIdentity(source.context.Name, "container", selection.ID)
	containers, err := m.Containers(ctx)
	if err != nil {
		return err
	}
	for _, container := range containers {
		if strings.TrimPrefix(container.Name, "/") == destination {
			if container.Labels[migrationSourceLabel] == identity {
				return nil
			}
			return fmt.Errorf("%w: destination container name exists", ErrConflict)
		}
	}
	var document json.RawMessage
	if err := source.get(ctx, "/containers/"+url.PathEscape(selection.ID)+"/json", nil, &document); err != nil {
		return err
	}
	var inspected migrationContainer
	if err := json.Unmarshal(document, &inspected); err != nil {
		return err
	}
	if inspected.ID != selection.ID {
		return datafiles.ErrConflict
	}
	configuration, update, err := migrationContainerRequest(document, request.IncludeSensitive)
	if err != nil {
		return err
	}
	configuration.Name = destination
	if configuration.Labels == nil {
		configuration.Labels = make(map[string]string)
	}
	configuration.Labels[migrationSourceLabel] = identity
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
	identity := migrationIdentity(source.context.Name, "volume", selection.ID)
	document, existingErr := m.InspectVolume(ctx, destination)
	if existingErr == nil {
		var existing struct{ Labels map[string]string }
		if err := json.Unmarshal(document, &existing); err != nil {
			return err
		}
		if existing.Labels[migrationSourceLabel] == identity {
			return nil
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
	var containers []struct {
		ID string `json:"Id"`
	}
	if err := source.get(ctx, "/containers/json", url.Values{"all": {"true"}}, &containers); err != nil {
		return err
	}
	containerID, mountedPath := "", ""
	for _, container := range containers {
		var inspected migrationContainer
		if err := source.get(ctx, "/containers/"+url.PathEscape(container.ID)+"/json", nil, &inspected); err != nil {
			return err
		}
		for _, mounted := range inspected.Mounts {
			if mounted.Type == "volume" && mounted.Name == selection.Name {
				containerID, mountedPath = inspected.ID, mounted.Destination
				break
			}
		}
		if containerID != "" {
			break
		}
	}
	if containerID == "" {
		if source.context.Endpoint != "" && strings.HasPrefix(source.context.Endpoint, "unix://") && filepath.IsAbs(volume.Mountpoint) {
			if info, statErr := os.Stat(volume.Mountpoint); statErr == nil && info.IsDir() {
				_, err = datafiles.Export(ctx, file, volume.Mountpoint, resource)
				if err != nil {
					return err
				}
			} else {
				return fmt.Errorf("%w: unreferenced source volume is not host-accessible; attach it to an existing source container yourself or export a local archive. Porto does not create source resources implicitly.", ErrUnsupported)
			}
		} else {
			return fmt.Errorf("%w: attach this volume to a source container or export a local archive first", ErrUnsupported)
		}
	} else {
		response, err := source.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(containerID)+"/archive", url.Values{"path": {path.Join(mountedPath, ".") + "/."}}, nil)
		if err != nil {
			return err
		}
		_, sealErr := datafiles.SealTar(ctx, response.Body, &transferWriter{ctx: ctx, output: file, progress: progress, phase: "Copying read-only source volume archive"}, resource)
		if err := errors.Join(sealErr, response.Body.Close()); err != nil {
			return err
		}
	}
	archive, err := InspectVolumeArchive(ctx, file.Name())
	if err != nil {
		return err
	}
	_, err = m.ImportVolume(ctx, destination, file.Name(), archive.SHA256, progress)
	if err != nil {
		return err
	}
	descriptor, err := m.FileDescriptor(ctx, "volume", destination)
	if err != nil {
		return err
	}
	if m.migrationVolumeRecorded == nil {
		return errors.New("migration volume ledger is unavailable; verified destination was retained")
	}
	return m.migrationVolumeRecorded(ctx, source.context.Name, selection.ID, destination, descriptor.Resource.Fingerprint())
}

func (m *Manager) SetMigrationVolumeLedger(
	record func(context.Context, string, string, string, string) error,
	known func(context.Context, string, string, string, string) (bool, error),
) {
	m.migrationVolumeRecorded, m.migrationVolumeKnown = record, known
}
