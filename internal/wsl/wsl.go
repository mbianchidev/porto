package wsl

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/filelock"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/wslbridge"
)

var stateMu sync.Mutex

type Distribution struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Version int    `json:"version"`
	Running bool   `json:"running"`
	Enabled bool   `json:"enabled"`
	Message string `json:"message,omitempty"`
}

type Share struct {
	LinuxPath   string `json:"linuxPath"`
	WindowsPath string `json:"windowsPath"`
	GuestPath   string `json:"guestPath"`
}

type Integration struct {
	Distribution string           `json:"distribution"`
	ID           string           `json:"id"`
	Owner        string           `json:"owner"`
	Architecture string           `json:"architecture"`
	Home         string           `json:"home"`
	Shares       []Share          `json:"shares"`
	Bridge       wslbridge.Config `json:"bridge"`
}

type Status struct {
	Supported     bool           `json:"supported"`
	Distributions []Distribution `json:"distributions"`
	Integrations  []Integration  `json:"integrations"`
	Message       string         `json:"message,omitempty"`
}

type Manager struct {
	root       string
	runner     runtimes.Runner
	platform   string
	discover   func(context.Context) ([]Distribution, error)
	executable func() (string, error)
	home       func() (string, error)
	runtimeDir string
}

func (m *Manager) Supported() bool { return m.platform == "windows" }

func New(root string, runner runtimes.Runner) *Manager {
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	manager := &Manager{
		root: root, runner: runner, platform: runtime.GOOS,
		executable: os.Executable, home: os.UserHomeDir,
		runtimeDir: os.Getenv("PORTO_WSL_RUNTIME_DIR"),
	}
	manager.discover = func(ctx context.Context) ([]Distribution, error) { return platformDistributions(ctx, runner) }
	return manager
}

func (m *Manager) state() ([]Integration, error) {
	data, err := os.ReadFile(filepath.Join(m.root, "wsl-integrations.json"))
	if errors.Is(err, os.ErrNotExist) {
		return []Integration{}, nil
	}
	if err != nil {
		return nil, err
	}
	var integrations []Integration
	if err := json.Unmarshal(data, &integrations); err != nil {
		return nil, fmt.Errorf("read selected WSL integrations: %w", err)
	}
	seen := map[string]bool{}
	for _, integration := range integrations {
		owner, err := hex.DecodeString(integration.Owner)
		if !validDistribution(integration.Distribution) || integration.ID == "" || err != nil || len(owner) != 16 || seen[integration.Distribution] {
			return nil, errors.New("invalid selected WSL integration identity; repair its owned metadata before mutation")
		}
		seen[integration.Distribution] = true
		for _, share := range integration.Shares {
			expectedWindows, pathErr := windowsSharePath(integration.Distribution, share.LinuxPath, share.WindowsPath)
			sum := sha256.Sum256([]byte(integration.ID + "\x00" + share.LinuxPath))
			if !path.IsAbs(share.LinuxPath) || path.Clean(share.LinuxPath) != share.LinuxPath ||
				share.LinuxPath == "/" || pathErr != nil || share.WindowsPath != expectedWindows ||
				share.GuestPath != "/mnt/porto-wsl/"+hex.EncodeToString(sum[:8]) {
				return nil, errors.New("selected WSL share metadata no longer matches its distribution identity")
			}
		}
	}
	return integrations, nil
}

func (m *Manager) save(integrations []Integration) error {
	return writeJSON(m.root, "wsl-integrations.json", integrations)
}

func writeJSON(root, name string, value any) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(root, ".porto-wsl-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(root, name))
}

func (m *Manager) Snapshot(ctx context.Context) (Status, error) {
	status := Status{Supported: m.platform == "windows", Distributions: []Distribution{}, Integrations: []Integration{}}
	if !status.Supported {
		status.Message = "Selected-distribution WSL integration requires Windows; existing Linux and Lima engines are unchanged"
		return status, nil
	}
	integrations, err := m.state()
	if err != nil {
		return status, err
	}
	distributions, err := m.discover(ctx)
	if err != nil {
		return status, err
	}
	for index := range distributions {
		distribution := &distributions[index]
		for _, integration := range integrations {
			if integration.Distribution == distribution.Name {
				distribution.Enabled = integration.ID == distribution.ID
				if !distribution.Enabled {
					distribution.Message = "Distribution identity changed; Porto will not modify the replacement distribution"
				} else if !distribution.Running {
					distribution.Message = "Distribution is stopped; start it before using its development tools"
				}
			}
		}
		if distribution.Version != 2 {
			distribution.Message = "WSL 2 is required; Porto does not convert distributions automatically"
		}
	}
	status.Distributions, status.Integrations = distributions, integrations
	return status, nil
}

func validDistribution(name string) bool {
	return name != "" && !strings.HasPrefix(name, "-") && !strings.ContainsAny(name, "\x00\r\n/\\")
}

func (m *Manager) selected(ctx context.Context, name string) (Distribution, error) {
	if m.platform != "windows" {
		return Distribution{}, errors.New("WSL integration is available only on Windows")
	}
	if !validDistribution(name) {
		return Distribution{}, errors.New("invalid WSL distribution name")
	}
	distributions, err := m.discover(ctx)
	if err != nil {
		return Distribution{}, err
	}
	for _, distribution := range distributions {
		if distribution.Name == name {
			if distribution.Version != 2 {
				return Distribution{}, errors.New("selected distribution must use WSL 2; no distribution was converted")
			}
			return distribution, nil
		}
	}
	return Distribution{}, fmt.Errorf("WSL distribution %q is not registered for this Windows user", name)
}

func (m *Manager) command(ctx context.Context, name, script string, input io.Reader, arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	args := []string{"--distribution", name, "--exec", "sh", "-c", script, "porto-wsl"}
	args = append(args, arguments...)
	output, err := m.runner.Run(ctx, runtimes.Command{Name: "wsl.exe", Args: args, StdinReader: input})
	if err != nil {
		return nil, runtimes.CommandError("configure selected WSL distribution "+name, output, err)
	}
	return output, nil
}

const probeScript = `set -eu
command -v wslpath >/dev/null || { echo 'WSL interoperability is disabled; enable Windows executable interop for this distribution' >&2; exit 1; }
printf '%s\000' "$(uname -m)" "$HOME" "$(wslpath -u -- "$1")"
shift
for root in "$HOME" "$(wslpath -u -- "$1")"; do
  root="$(realpath -e -- "$root")"
  [ -d "$root" ] || { echo 'WSL mount root must be a directory' >&2; exit 1; }
  printf '%s\000%s\000' "$root" "$(wslpath -w -- "$root")"
done
shift
for root do
  root="$(realpath -e -- "$root")"
  [ -d "$root" ] || { echo 'WSL mount root must be a directory' >&2; exit 1; }
  printf '%s\000%s\000' "$root" "$(wslpath -w -- "$root")"
done
`

func (m *Manager) Enable(ctx context.Context, name string, roots []string) (integration Integration, err error) {
	stateMu.Lock()
	defer stateMu.Unlock()
	distribution, err := m.selected(ctx, name)
	if err != nil {
		return integration, err
	}
	lockContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	lease, err := filelock.Acquire(lockContext, filepath.Join(m.root, "wsl-integrations.lock"))
	if err != nil {
		return integration, fmt.Errorf("lock WSL integration: %w", err)
	}
	defer func() { err = errors.Join(err, lease.Close()) }()
	previous, err := m.state()
	if err != nil {
		return integration, err
	}
	var owner [16]byte
	if _, err := rand.Read(owner[:]); err != nil {
		return integration, err
	}
	integration = Integration{Distribution: name, ID: distribution.ID, Owner: hex.EncodeToString(owner[:])}
	for _, existing := range previous {
		if existing.Distribution == name {
			if existing.ID != distribution.ID {
				return integration, errors.New("selected distribution was replaced; refusing to overwrite its integration")
			}
			integration.Owner = existing.Owner
			if len(roots) == 0 {
				for _, share := range existing.Shares {
					roots = append(roots, share.LinuxPath)
				}
			}
		}
	}
	executable, err := m.executable()
	if err != nil {
		return integration, err
	}
	windowsHome, err := m.home()
	if err != nil {
		return integration, err
	}
	for _, root := range roots {
		if !path.IsAbs(root) || path.Clean(root) == "/" || strings.ContainsAny(root, "\x00\r\n") {
			return integration, errors.New("additional WSL mount roots must be explicit absolute directories, not the distribution root")
		}
	}
	output, err := m.command(ctx, name, probeScript, nil, append([]string{executable, windowsHome}, roots...)...)
	if err != nil {
		return integration, err
	}
	fields := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if len(fields) < 7 || len(fields)%2 != 1 {
		return integration, errors.New("selected WSL distribution returned invalid interoperability paths")
	}
	switch fields[0] {
	case "x86_64":
		integration.Architecture = "amd64"
	case "aarch64", "arm64":
		integration.Architecture = "arm64"
	default:
		return integration, fmt.Errorf("WSL architecture %q has no bundled native development tools", fields[0])
	}
	integration.Home = fields[1]
	windowsConfigurationHome, err := filepath.Abs(m.root)
	if err != nil {
		return integration, err
	}
	integration.Bridge = wslbridge.Config{
		Distribution: name, Owner: integration.Owner, WindowsExecutable: fields[2],
		WindowsHome: windowsConfigurationHome, Socket: path.Join("/tmp", "porto-wsl-"+integration.Owner, "docker.sock"),
	}
	if !path.IsAbs(integration.Home) || !path.IsAbs(fields[2]) {
		return integration, errors.New("WSL returned non-absolute home or executable paths")
	}
	seen := map[string]bool{}
	for index := 3; index < len(fields); index += 2 {
		linuxPath, windowsPath := path.Clean(fields[index]), fields[index+1]
		if !path.IsAbs(linuxPath) || linuxPath == "/" || windowsPath == "" || strings.ContainsAny(windowsPath, "\x00\r\n") {
			return integration, errors.New("WSL returned an invalid shared directory")
		}
		if seen[linuxPath] {
			continue
		}
		seen[linuxPath] = true
		windowsPath, err = windowsSharePath(name, linuxPath, windowsPath)
		if err != nil {
			return integration, err
		}
		sum := sha256.Sum256([]byte(distribution.ID + "\x00" + linuxPath))
		integration.Shares = append(integration.Shares, Share{
			LinuxPath: linuxPath, WindowsPath: windowsPath, GuestPath: "/mnt/porto-wsl/" + hex.EncodeToString(sum[:8]),
		})
	}
	sort.Slice(integration.Shares, func(i, j int) bool {
		return len(integration.Shares[i].LinuxPath) < len(integration.Shares[j].LinuxPath)
	})
	nonOverlapping := make([]Share, 0, len(integration.Shares))
	for _, share := range integration.Shares {
		covered := false
		for _, parent := range nonOverlapping {
			if strings.HasPrefix(share.LinuxPath, parent.LinuxPath+"/") {
				covered = true
				break
			}
		}
		if !covered {
			nonOverlapping = append(nonOverlapping, share)
		}
	}
	integration.Shares = nonOverlapping
	bundleRoot := m.runtimeDir
	if bundleRoot == "" {
		bundleRoot = filepath.Join(filepath.Dir(executable), "runtime", "wsl")
	}
	archive, err := m.bundle(filepath.Join(bundleRoot, integration.Architecture), integration)
	if err != nil {
		return integration, err
	}
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archive.Name())
	}()
	next := make([]Integration, 0, len(previous)+1)
	for _, existing := range previous {
		if existing.Distribution != name {
			next = append(next, existing)
		}
	}
	next = append(next, integration)
	if err := m.save(next); err != nil {
		return integration, err
	}
	if _, err := m.command(ctx, name, installScript, archive, integration.Owner); err != nil {
		return integration, errors.Join(err, m.save(previous))
	}
	return integration, nil
}

func (m *Manager) Disable(ctx context.Context, name string) (err error) {
	if !m.Supported() || !validDistribution(name) {
		return errors.New("a valid selected Windows WSL distribution is required")
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	lockContext, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	lease, err := filelock.Acquire(lockContext, filepath.Join(m.root, "wsl-integrations.lock"))
	if err != nil {
		return fmt.Errorf("lock WSL integration removal: %w", err)
	}
	defer func() { err = errors.Join(err, lease.Close()) }()
	integrations, err := m.state()
	if err != nil {
		return err
	}
	next := make([]Integration, 0, len(integrations))
	for _, integration := range integrations {
		if integration.Distribution != name {
			next = append(next, integration)
			continue
		}
		distribution, err := m.selected(ctx, name)
		if err != nil {
			return err
		}
		if distribution.ID != integration.ID {
			return errors.New("WSL distribution identity changed; no replacement distribution was modified")
		}
		if _, err := m.command(ctx, name, uninstallScript, nil, integration.Owner); err != nil {
			return err
		}
	}
	return m.save(next)
}

func (m *Manager) Authorize(ctx context.Context, name, owner string) (Integration, error) {
	distribution, err := m.selected(ctx, name)
	if err != nil {
		return Integration{}, err
	}
	integrations, err := m.state()
	if err != nil {
		return Integration{}, err
	}
	for _, integration := range integrations {
		if integration.Distribution == name && integration.ID == distribution.ID && integration.Owner == owner && owner != "" {
			if !distribution.Running {
				return Integration{}, errors.New("selected WSL distribution is stopped; start it and retry")
			}
			return integration, nil
		}
	}
	return Integration{}, errors.New("WSL distribution has not opted in to this Porto integration")
}

func TranslatePath(integration Integration, source string) (string, error) {
	if !path.IsAbs(source) || strings.ContainsAny(source, "\x00\r\n") {
		return "", errors.New("WSL bind sources must be absolute Linux paths")
	}
	source = path.Clean(source)
	shares := append([]Share(nil), integration.Shares...)
	sort.Slice(shares, func(i, j int) bool { return len(shares[i].LinuxPath) > len(shares[j].LinuxPath) })
	for _, share := range shares {
		if source == share.LinuxPath || strings.HasPrefix(source, share.LinuxPath+"/") {
			return share.GuestPath + strings.TrimPrefix(source, share.LinuxPath), nil
		}
	}
	return "", fmt.Errorf("WSL bind source %q is outside the selected shared roots; enable this distribution with '--mount-root %s' and explicitly apply its engine mounts", source, path.Dir(source))
}

func windowsSharePath(distribution, linuxPath, probedPath string) (string, error) {
	if strings.ContainsAny(probedPath, "\x00\r\n") {
		return "", errors.New("WSL returned an invalid Windows share path")
	}
	if len(probedPath) >= 2 && probedPath[1] == ':' {
		drive := probedPath[0]
		if len(probedPath) < 3 || !((drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')) ||
			(probedPath[2] != '\\' && probedPath[2] != '/') || strings.ContainsAny(probedPath[2:], `<>:"|?*`) {
			return "", errors.New("WSL returned a non-absolute or invalid native Windows drive path")
		}
		tail := path.Clean(strings.ReplaceAll(probedPath[2:], `\`, "/"))
		return strings.ToUpper(probedPath[:1]) + ":" + strings.ReplaceAll(tail, "/", `\`), nil
	}
	return `\\wsl.localhost\` + distribution + strings.ReplaceAll(linuxPath, "/", `\`), nil
}

func (m *Manager) Shares() ([]Share, error) {
	integrations, err := m.state()
	if err != nil {
		return nil, err
	}
	shares := []Share{}
	for _, integration := range integrations {
		shares = append(shares, integration.Shares...)
	}
	return shares, nil
}

func (m *Manager) bundle(root string, integration Integration) (*os.File, error) {
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(m.root, ".porto-wsl-bundle-*.tar")
	if err != nil {
		return nil, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()
	writer := tar.NewWriter(file)
	for _, name := range []string{"bin/docker", "bin/porto-wsl", "docker/cli-plugins/docker-compose", "docker/cli-plugins/docker-buildx"} {
		input, err := os.Open(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return nil, fmt.Errorf("native Linux/%s WSL tools are missing; install the matching Porto desktop package or set PORTO_WSL_RUNTIME_DIR: %w", integration.Architecture, err)
		}
		info, err := input.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = input.Close()
			return nil, fmt.Errorf("WSL runtime tool %s is not a regular file", name)
		}
		err = writer.WriteHeader(&tar.Header{Name: "tools/" + name, Mode: 0o755, Size: info.Size()})
		if err == nil {
			_, err = io.Copy(writer, input)
		}
		err = errors.Join(err, input.Close())
		if err != nil {
			return nil, err
		}
	}
	configuration, err := json.Marshal(integration.Bridge)
	if err != nil {
		return nil, err
	}
	for name, data := range map[string][]byte{
		"owner": []byte(integration.Owner + "\n"), "integration.json": configuration,
		"bin/docker": []byte(dockerWrapper), "bin/docker-credential-porto": []byte(credentialWrapper),
		"environment": []byte("case \"$PATH\" in \"$HOME/.local/share/porto/wsl/bin\"|\"$HOME/.local/share/porto/wsl/bin\":*) ;; *) export PATH=\"$HOME/.local/share/porto/wsl/bin:$PATH\" ;; esac\n"),
	} {
		mode := int64(0o600)
		if strings.HasPrefix(name, "bin/") {
			mode = 0o755
		}
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data))}); err != nil {
			return nil, err
		}
		if _, err := writer.Write(data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	succeeded = true
	return file, nil
}
