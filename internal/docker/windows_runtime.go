package docker

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/windowscontainers"
)

type windowsRuntime struct {
	networkMu       sync.Mutex
	attachMu        sync.Mutex
	attachProcesses map[string]map[runtimes.Process]struct{}
	imageReferences *windowsImageReferences
	installation    windowscontainers.Installation
	checkNetworks   func(windowscontainers.Installation) error
	ensureNetwork   func(windowscontainers.Installation, string, string) (windowscontainers.Network, error)
	removeNetwork   func(windowscontainers.Installation, string) error
	resolveImage    func(context.Context, string, *RegistryAuth) (windowscontainers.ImagePlatform, string, error)
	checkBind       func(string) error
}

func NewWindowsManager(runner runtimes.Runner, installation windowscontainers.Installation) (*Manager, error) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		return nil, windowscontainers.ErrUnsupported
	}
	return newWindowsManager(runner, installation)
}

func newWindowsManager(runner runtimes.Runner, installation windowscontainers.Installation) (*Manager, error) {
	if err := installation.Host.Validate(); err != nil {
		return nil, err
	}
	manager := NewWithStateDir(runner, installation.Data())
	references, err := loadWindowsImageReferences(installation.Data())
	if err != nil {
		return nil, err
	}
	manager.windows = &windowsRuntime{
		imageReferences: references,
		installation:    installation, checkNetworks: windowscontainers.CheckNetworks,
		ensureNetwork: windowscontainers.EnsureNetwork, removeNetwork: windowscontainers.RemoveNetwork,
		checkBind: func(path string) error {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("resolve native Windows bind source: %w", err)
			}
			if windowsPathsOverlap(resolved, installation.Root) {
				return fmt.Errorf("%w: native binds cannot expose the protected runtime installation or any of its ancestors", windowscontainers.ErrOwnership)
			}
			info, err := os.Stat(path)
			if err != nil {
				return fmt.Errorf("native Windows bind source must already exist: %w", err)
			}
			if !info.IsDir() {
				return fmt.Errorf("%w: native Windows binds currently require directories", ErrUnsupported)
			}
			return nil
		},
	}
	manager.windows.resolveImage = manager.resolveWindowsImage
	manager.directCLI = true
	// Inventory is read-only. In particular, none of the Linux OCI, FIFO,
	// health, CRIU or runtime-helper operation interfaces escape this backend.
	manager.creationConnector = nil
	manager.operationsConnector = nil
	manager.execConnector = nil
	manager.networkConnector = nil
	manager.metricReader = nil
	return manager, nil
}

func (m *Manager) IsNativeWindows() bool { return m.windows != nil }

func (m *Manager) RuntimeOS() string {
	if m.windows != nil {
		return "windows"
	}

	return "linux"
}

func (m *Manager) defaultRuntimeName() string {
	if m.IsNativeWindows() {
		return windowscontainers.RuntimeName
	}
	return "io.containerd.runc.v2"
}

type windowsInventory struct{ containerRuntime }

func (windowsInventory) Capabilities(context.Context) ContainerCapabilities {
	return ContainerCapabilities{
		DirectInventory:   RuntimeCapability{Supported: true},
		LifecycleEvents:   RuntimeCapability{Supported: true},
		DirectCreation:    RuntimeCapability{Reason: "native Windows uses maintained nerdctl/runhcs, not Porto's Linux OCI creation"},
		TaskRecreation:    RuntimeCapability{Supported: true, Reason: "native stopped tasks are recreated by maintained nerdctl/runhcs, not the Linux OCI helper"},
		ExecLifecycle:     RuntimeCapability{Supported: true, Reason: "native Windows nerdctl exec with runhcs"},
		HealthUpdates:     RuntimeCapability{Reason: "native Windows health scheduling is not supported"},
		NetworkUpdates:    RuntimeCapability{Reason: "native Windows uses owned NAT at container creation; live network connect/disconnect is not supported"},
		CheckpointRestore: RuntimeCapability{Reason: "native Windows checkpoints and Linux CRIU helpers are not supported"},
	}
}

func windowsPathsOverlap(first, second string) bool {
	clean := func(value string) string {
		return strings.ToLower(strings.TrimRight(strings.ReplaceAll(value, "/", `\`), `\`))
	}
	first, second = clean(first), clean(second)
	return first == second || strings.HasPrefix(first, second+`\`) || strings.HasPrefix(second, first+`\`)
}

func windowsPath(path string) bool {
	return len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) &&
		path[1] == ':' && (path[2] == '\\' || path[2] == '/') &&
		!strings.ContainsAny(path, "\x00\r\n\"*?<>|")
}

func windowsVolumeParts(value string) (source, target, mode string, err error) {
	// Drive colons belong to paths, not to Docker's volume separators.
	var fields []string
	start := 0
	for n := 0; n < len(value); n++ {
		if value[n] != ':' || (n == start+1 && n+1 < len(value) && (value[n+1] == '\\' || value[n+1] == '/')) {
			continue
		}
		fields = append(fields, value[start:n])
		start = n + 1
	}
	fields = append(fields, value[start:])
	switch len(fields) {
	case 1:
		target = fields[0]
	case 2:
		source, target = fields[0], fields[1]
	case 3:
		source, target, mode = fields[0], fields[1], fields[2]
	default:
		return "", "", "", errors.New("invalid native Windows volume specification")
	}
	if !windowsPath(target) {
		return "", "", "", fmt.Errorf("native Windows mount target must be an absolute drive path, got %q", target)
	}
	if source != "" && !windowsPath(source) {
		if err := validateObjectID(source); err != nil || strings.ContainsAny(source, `\/:`) {
			return "", "", "", errors.New("native Windows bind sources must use native absolute drive paths; WSL and UNC mounts belong to the Linux endpoint")
		}
	}
	if mode != "" && mode != "ro" && mode != "rw" {
		return "", "", "", fmt.Errorf("%w: native Windows mount mode %s", ErrUnsupported, mode)
	}
	return source, target, mode, nil
}

func windowsStructuredMountArguments(mounts []dockerStructuredMount, tmpfs map[string]string) ([]string, []string, map[string]string, error) {
	if len(tmpfs) != 0 {
		return nil, nil, nil, fmt.Errorf("%w: Linux tmpfs on native Windows", ErrUnsupported)
	}
	var volumes, structured []string
	seen := make(map[string]bool)
	for _, mount := range mounts {
		if !windowsPath(mount.Target) {
			return nil, nil, nil, fmt.Errorf("invalid native Windows mount target %q", mount.Target)
		}
		key := strings.ToLower(strings.ReplaceAll(mount.Target, "/", `\`))
		if seen[key] {
			return nil, nil, nil, errors.New("duplicate native Windows mount target")
		}
		seen[key] = true
		if mount.BindOptions != nil && (mount.BindOptions.Propagation != "" || mount.BindOptions.NonRecursive ||
			mount.BindOptions.CreateMountpoint || mount.BindOptions.ReadOnlyNonRecursive || mount.BindOptions.ReadOnlyForceRecursive) {
			return nil, nil, nil, fmt.Errorf("%w: Linux recursive bind controls on native Windows", ErrUnsupported)
		}
		if mount.Consistency != "" && mount.Consistency != "consistent" && mount.Consistency != "default" {
			return nil, nil, nil, fmt.Errorf("%w: native Windows mount consistency", ErrUnsupported)
		}
		switch mount.Type {
		case "bind":
			if !windowsPath(mount.Source) || mount.VolumeOptions != nil || mount.TmpfsOptions != nil {
				return nil, nil, nil, errors.New("invalid native Windows bind mount")
			}
			fields := []string{"type=bind", "source=" + mount.Source, "target=" + mount.Target}
			if mount.ReadOnly {
				fields = append(fields, "readonly")
			}
			value, err := dockerCSV(fields)
			if err != nil {
				return nil, nil, nil, err
			}
			structured = append(structured, value)
		case "volume":
			value, err := structuredVolumeMount(mount)
			if err != nil {
				return nil, nil, nil, err
			}
			volumes = append(volumes, value)
		default:
			return nil, nil, nil, fmt.Errorf("%w: native Windows mount type %q", ErrUnsupported, mount.Type)
		}
	}
	return volumes, structured, nil, nil
}

func (m *Manager) validateWindowsContainer(ctx context.Context, request *CreateContainerRequest) (err error) {
	defer func() {
		if err != nil && !errors.Is(err, ErrUnsupported) && !errors.Is(err, windowscontainers.ErrOwnership) &&
			!errors.Is(err, ErrNotFound) && !errors.Is(err, ErrUnavailable) {
			err = fmt.Errorf("invalid native Windows container request: %w", err)
		}
	}()
	host := m.windows.installation.Host
	if request.TTY {
		if err := runtimes.ValidatePTYSize(request.ConsoleSize); err != nil {
			return err
		}
		if !runtimes.PTYSupported() {
			return fmt.Errorf("%w: native Windows ConPTY is unavailable", ErrUnsupported)
		}
		if _, ok := m.runner.(runtimes.PTYRunner); !ok {
			return fmt.Errorf("%w: native ConPTY process runner", ErrUnsupported)
		}
	}
	isolation, err := host.Isolation(request.Isolation)
	if err != nil {
		return err
	}
	request.Isolation = isolation
	if request.Platform != "" && request.Platform != "windows/amd64" {
		return errors.New("native Windows platform must be windows/amd64")
	}
	request.Platform = "windows/amd64"
	if request.Privileged || len(request.SecurityOpt)+len(request.CapAdd)+len(request.CapDrop)+len(request.Devices)+len(request.GPUs)+
		len(request.GroupAdd)+len(request.Ulimits)+len(request.Tmpfs)+len(request.Sysctls) != 0 ||
		request.Init || request.ShmSize != 0 || request.IPC != "" || request.PIDMode != "" || request.UTS != "" ||
		request.CgroupParent != "" || request.Cgroupns != "" || request.Userns != "" || request.ReadOnly {
		return fmt.Errorf("%w: Linux privileges, devices, namespaces, cgroups, security, or filesystem options on native Windows", ErrUnsupported)
	}
	if request.Healthcheck != nil && len(request.Healthcheck.Test) > 0 && request.Healthcheck.Test[0] != "NONE" {
		return fmt.Errorf("%w: native Windows health scheduling", ErrUnsupported)
	}
	request.Healthcheck = nil
	if request.Hostname != "" || len(request.DNS)+len(request.DNSOptions)+len(request.DNSSearch)+len(request.ExtraHosts) != 0 {
		return fmt.Errorf("%w: native nerdctl does not support custom Windows DNS, hostname, or add-host settings", ErrUnsupported)
	}
	if request.Restart != "" && request.Restart != "no" {
		return fmt.Errorf("%w: native Windows automatic restart policies", ErrUnsupported)
	}
	// Linux-only resource controls must never be translated to a different
	// Windows meaning. Memory and whole CPU counts use nerdctl's native flags.
	limits := request.Limits
	limits.Memory, limits.NanoCPUs = 0, 0
	encoded, _ := json.Marshal(limits)
	empty, _ := json.Marshal(ContainerUpdate{})
	if string(encoded) != string(empty) {
		return fmt.Errorf("%w: Linux resource limits on native Windows", ErrUnsupported)
	}
	if request.Limits.NanoCPUs%1_000_000_000 != 0 {
		return errors.New("native Windows CPU limits must be whole CPU counts")
	}
	for _, volume := range request.Volumes {
		source, _, _, err := windowsVolumeParts(volume)
		if err != nil {
			return err
		}
		if windowsPath(source) {
			if err := m.windows.checkBind(source); err != nil {
				return err
			}
		}
	}
	for _, mount := range request.Mounts {
		reader := csv.NewReader(strings.NewReader(mount))
		fields, err := reader.Read()
		if err != nil {
			return fmt.Errorf("invalid native Windows structured bind: %w", err)
		}
		var source, target string
		for _, field := range fields {
			name, value, _ := strings.Cut(field, "=")
			switch name {
			case "type":
				if value != "bind" {
					return fmt.Errorf("%w: native structured mount type", ErrUnsupported)
				}
			case "source", "src":
				source = value
			case "target", "dst", "destination":
				target = value
			case "readonly", "ro":
				if value != "" && value != "true" {
					return errors.New("invalid native bind readonly value")
				}
			default:
				return fmt.Errorf("%w: native bind option %s", ErrUnsupported, name)
			}
		}
		if !windowsPath(source) || !windowsPath(target) {
			return errors.New("native Windows structured bind paths must be absolute drive paths")
		}
		if err := m.windows.checkBind(source); err != nil {
			return err
		}
	}
	if len(request.Networks) == 0 {
		request.Networks = []ContainerNetwork{{Name: m.windows.installation.DefaultNetwork()}}
	}
	for n := range request.Networks {
		network := &request.Networks[n]
		if network.Name == "" {
			network.Name = "default"
		}
		resolved, err := m.windowsNetworkIdentifier(ctx, network.Name)
		if err != nil {
			return err
		}
		network.Name = resolved
		if network.Name != "none" {
			if err := windowscontainers.ValidateNetworkName(m.windows.installation, network.Name); err != nil {
				return err
			}
		}
		if len(network.Aliases) != 0 {
			return fmt.Errorf("%w: native NAT network aliases", ErrUnsupported)
		}
	}
	if len(request.Networks) != 1 {
		return fmt.Errorf("%w: native Windows supports one NAT network per container", ErrUnsupported)
	}
	image, id, err := m.localWindowsImage(ctx, request.Image)
	if err != nil {
		return err
	}
	if err := host.ValidateImage(image, request.Isolation); err != nil {
		return err
	}
	if id == "" {
		return errors.New("native Windows image inspection returned no immutable image ID; pull the image explicitly")
	}
	request.Image = id
	request.Labels = cloneStringMap(request.Labels)
	if request.Labels == nil {
		request.Labels = make(map[string]string)
	}
	request.Labels["io.porto.windows.isolation"] = isolation
	return nil
}

func (m *Manager) localWindowsImage(ctx context.Context, image string) (windowscontainers.ImagePlatform, string, error) {
	image = m.windowsImageReference(image)
	document, err := m.inspect(ctx, "image", normalizeNerdctlReference(image), "--platform", "windows/amd64")
	if err != nil {
		return windowscontainers.ImagePlatform{}, "", err
	}
	var inspected struct {
		ID           string `json:"Id"`
		OS           string `json:"Os"`
		Architecture string
		OSVersion    string `json:"OsVersion"`
	}
	if err := json.Unmarshal(document, &inspected); err != nil {
		return windowscontainers.ImagePlatform{}, "", err
	}
	return windowscontainers.ImagePlatform{OS: inspected.OS, Architecture: inspected.Architecture, OSVersion: inspected.OSVersion}, inspected.ID, nil
}

func (m *Manager) validateWindowsStart(ctx context.Context, id string) error {
	document, err := m.InspectContainer(ctx, id)
	if err != nil {
		return err
	}
	var inspected struct {
		Image  string
		Config struct {
			Image  string
			Labels map[string]string
		}
		HostConfig struct{ Isolation string }
	}
	if err := json.Unmarshal(document, &inspected); err != nil {
		return err
	}
	isolation := firstNonEmpty(inspected.Config.Labels["io.porto.windows.isolation"], inspected.HostConfig.Isolation)
	image, _, err := m.localWindowsImage(ctx, firstNonEmpty(inspected.Image, inspected.Config.Image))
	if err != nil {
		return err
	}
	return m.windows.installation.Host.ValidateImage(image, isolation)
}

func (m *Manager) windowsTerminalCommand(ctx context.Context, id, shell string, debug bool) (*exec.Cmd, error) {
	if debug {
		return nil, fmt.Errorf("%w: Linux debug sidecars on native Windows", ErrUnsupported)
	}
	if shell == "" {
		shell = "cmd.exe"
	}
	switch strings.ToLower(shell) {
	case "cmd", "cmd.exe", "powershell", "powershell.exe":
	default:
		return nil, errors.New("native Windows container shell must be cmd.exe or powershell.exe")
	}
	backend, err := m.backend(ctx)
	if err != nil {
		return nil, err
	}
	args := append(append([]string(nil), backend.prefix...), "exec", "--interactive", "--tty", id, shell)
	command := exec.CommandContext(ctx, backend.name, args...)
	command.Env = os.Environ()
	return command, nil
}

func nativeWindowsUnsupported(path, method string) bool {
	for _, part := range []string{"/build", "/grpc", "/session", "/commit", "/system/df", "/images/get", "/images/load"} {
		if path == part || strings.HasPrefix(path, part+"/") {
			return true
		}
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if method == "POST" && len(parts) == 2 && parts[1] == "prune" {
		return true
	}
	if len(parts) >= 3 && parts[0] == "containers" {
		switch parts[2] {
		case "archive", "export", "changes", "checkpoint", "checkpoints", "restore", "update":
			return true
		}
	}
	return false
}

func (m *Manager) pullWindowsImage(ctx context.Context, reference, platform string, auth *RegistryAuth) error {
	if platform != "" && platform != "windows/amd64" {
		return errors.New("invalid native Windows platform: pulls require windows/amd64; use the separate Linux endpoint for Linux images")
	}
	if err := m.windows.installation.Host.Validate(); err != nil {
		return err
	}
	var err error
	reference = normalizeNerdctlReference(reference)
	if auth == nil {
		auth, err = m.resolveRegistryAuth(ctx, reference)
		if err != nil {
			return err
		}
	}
	metadata, pinned, err := m.windows.resolveImage(ctx, reference, auth)
	if err != nil {
		return fmt.Errorf("invalid native Windows image preflight before pull: %w", err)
	}
	if err := validateWindowsPull(m.windows.installation.Host, metadata); err != nil {
		return fmt.Errorf("invalid native Windows image compatibility: %w", err)
	}
	backend, err := m.backend(ctx)
	if err != nil {
		return err
	}
	config, authenticated, err := registryDockerConfig(reference, auth)
	if err != nil {
		return err
	}
	args := []string{"pull", "--platform", "windows/amd64", pinned}
	if authenticated {
		_, err = m.runBackendWithDockerConfig(ctx, backend, 30*time.Minute, "pull native Windows image", config, args...)
	} else {
		_, err = m.runBackend(ctx, backend, 30*time.Minute, "pull native Windows image", nil, args...)
	}
	if err != nil {
		return err
	}
	if strings.Contains(reference, "@") {
		return m.recordWindowsImageReference(reference, pinned)
	}
	if reference != pinned {
		return m.TagImage(ctx, pinned, reference)
	}
	return nil
}

func validateWindowsPull(host windowscontainers.Host, image windowscontainers.ImagePlatform) error {
	return host.ValidateImage(image, "")
}

// compile-time assertion: the inventory adapter deliberately exposes no
// containerCreation, containerOperations, execOperations or helper interfaces.
var _ containerRuntime = windowsInventory{}
