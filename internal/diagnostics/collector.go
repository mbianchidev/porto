package diagnostics

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/certificates"
	"github.com/mbianchidev/porto/internal/config"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/dockercli"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/localhttps"
	"github.com/mbianchidev/porto/internal/providers"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

const (
	diagnosticProbeTimeout = 8 * time.Second
	collectionTimeout      = 30 * time.Second
	maxBundleFileBytes     = 256 * 1024
)

type SettingsReader interface {
	Settings(context.Context) (app.Settings, error)
}

type Collector struct {
	Settings          SettingsReader
	SettingsError     error
	Docker            *portodocker.Manager
	DockerStatus      func(context.Context) portodocker.Status
	Kubernetes        *kubernetes.Manager
	Clusters          *kubernetes.ClusterProvisioner
	VMs               *vm.Manager
	Providers         *providers.Manager
	CertificateStatus func() (certificates.Status, error)
	DockerSocket      string
	StateDirectory    string
	LogPath           string
	DaemonAvailable   bool
	DashboardReady    bool
	DaemonMessage     string
	DaemonIdentity    string
	Now               func() time.Time
	DiskCapacity      func(string) (uint64, uint64, error)
	DialContext       func(context.Context, string, string) (net.Conn, error)
	LookupHost        func(context.Context, string) ([]string, error)
	Executable        func() (string, error)
	HomeDirectory     func() (string, error)
	Hostname          func() (string, error)
	CurrentUser       func() (*user.User, error)
}

func NewLocalCollector(settings SettingsReader) (*Collector, error) {
	stateDirectory, err := config.Dir()
	if err != nil {
		return nil, err
	}
	dockerStateDirectory, err := config.DockerEngineDir()
	if err != nil {
		return nil, err
	}
	kubeconfigDirectory, err := config.KubernetesConfigDir()
	if err != nil {
		return nil, err
	}
	vmStateDirectory, err := config.VMStateDir()
	if err != nil {
		return nil, err
	}
	dockerSocket, err := config.DockerSocketPath()
	if err != nil {
		return nil, err
	}
	logPath, err := config.LogPath()
	if err != nil {
		return nil, err
	}
	certificatePath, keyPath, err := config.CertificatePaths()
	if err != nil {
		return nil, err
	}
	authorityPath, _, err := config.CertificateAuthorityPaths()
	if err != nil {
		return nil, err
	}
	runner := runtimes.ExecRunner{}
	vmManager := vm.NewWithStateDir(runner, vmStateDirectory)
	return &Collector{
		Settings:       settings,
		Docker:         portodocker.NewWithStateDir(runner, dockerStateDirectory),
		Kubernetes:     kubernetes.NewWithKubeconfigRoot(runner, kubeconfigDirectory),
		Clusters:       kubernetes.NewClusterProvisioner(vmManager, runner, kubeconfigDirectory),
		VMs:            vmManager,
		Providers:      providers.New(runner),
		DockerSocket:   dockerSocket,
		StateDirectory: stateDirectory,
		LogPath:        logPath,
		CertificateStatus: func() (certificates.Status, error) {
			return inspectCertificate(certificatePath, keyPath, authorityPath)
		},
	}, nil
}

func (c *Collector) Collect(ctx context.Context) Report {
	c.setDefaults()
	ctx, cancelCollection := context.WithTimeout(ctx, collectionTimeout)
	defer cancelCollection()
	checks := make([]Check, 0, 32)
	checks = append(checks, Check{
		ID:       "porto-version",
		Category: "porto",
		Name:     "Porto version",
		State:    StateHealthy,
		Summary:  config.Version,
		Detail:   runtime.GOOS + "/" + runtime.GOARCH,
	})
	checks = append(checks, c.daemonCheck())
	checks = append(checks, c.stateDirectoryCheck())
	checks = append(checks, c.diskCheck())
	checks = append(checks, c.portChecks(ctx)...)
	checks = append(checks, c.dnsCheck(ctx))
	checks = append(checks, c.executableIntegrityCheck())

	settings := app.Settings{}
	settingsAvailable := false
	if c.SettingsError != nil {
		checks = append(checks, Check{
			ID: "settings", Category: "porto", Name: "Runtime settings", State: StateUnavailable,
			Summary: "Settings database is unavailable", Detail: c.SettingsError.Error(),
		})
	} else if c.Settings == nil {
		checks = append(checks, Check{
			ID: "settings", Category: "porto", Name: "Runtime settings", State: StateUnavailable,
			Summary: "Settings storage is unavailable",
		})
	} else if current, err := c.Settings.Settings(ctx); err != nil {
		checks = append(checks, Check{
			ID: "settings", Category: "porto", Name: "Runtime settings", State: StateUnavailable,
			Summary: "Unable to read runtime settings", Detail: err.Error(),
		})
	} else {
		settings = current
		settingsAvailable = true
		checks = append(checks, Check{
			ID: "settings", Category: "porto", Name: "Runtime settings", State: StateHealthy,
			Summary: "Loaded",
		})
		checks = append(checks, runtimeGateChecks(settings)...)
	}

	if c.Providers != nil {
		probeContext, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
		checks = append(checks, providerChecks(settings, c.Providers.Status(probeContext))...)
		cancel()
	}
	toolchainContext, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	checks = append(checks, dockerToolchainChecks(dockercli.Inspect(toolchainContext, c.runtimeManifestPath()))...)
	cancel()
	checks = append(checks, c.certificateCheck())
	checks = append(checks, c.localHTTPSCheck())
	checks = append(checks, c.dockerChecks(ctx, settings, settingsAvailable)...)
	checks = append(checks, c.kubernetesChecks(ctx, settings, settingsAvailable)...)
	checks = append(checks, c.vmChecks(ctx, settings, settingsAvailable)...)
	sort.SliceStable(checks, func(i, j int) bool {
		if checks[i].Category != checks[j].Category {
			return checks[i].Category < checks[j].Category
		}
		return checks[i].Name < checks[j].Name
	})
	return NewReport(config.Version, c.Now(), checks)
}

func (c *Collector) BundleSources(ctx context.Context) []BundleSource {
	c.setDefaults()
	sources := make([]BundleSource, 0, 12)
	if c.Settings != nil {
		sources = append(sources, BundleSource{
			Name: "config/settings.json", Description: "Porto settings without stored credentials",
			Read: func() ([]byte, error) {
				settings, err := c.Settings.Settings(ctx)
				if err != nil {
					return nil, err
				}
				return json.MarshalIndent(settings, "", "  ")
			},
		})
	}
	sources = append(sources, BundleSource{
		Name: "system.json", Description: "Porto version and host runtime",
		Read: func() ([]byte, error) {
			executable, executableErr := c.Executable()
			return json.MarshalIndent(map[string]any{
				"version": config.Version, "os": runtime.GOOS, "architecture": runtime.GOARCH,
				"executable": executable, "executableError": errorString(executableErr),
				"daemonIdentity": c.DaemonIdentity,
			}, "", "  ")
		},
	})
	if c.LogPath != "" {
		sources = append(sources, fileBundleSource("logs/porto.log", "Last 256 KiB of Porto diagnostics", c.LogPath, true))
	}
	for _, candidate := range []struct {
		name        string
		description string
		path        string
	}{
		{"config/docker-engine.json", "Porto container engine ownership metadata", filepath.Join(c.StateDirectory, "docker", "engine.json")},
		{"config/docker-endpoint.json", "Canonical Docker endpoint metadata", filepath.Join(c.StateDirectory, "docker-endpoint.json")},
	} {
		if candidate.path != "" && fileExists(candidate.path) {
			sources = append(sources, fileBundleSource(candidate.name, candidate.description, candidate.path, false))
		}
	}
	if kubeconfigDirectory, err := config.KubernetesConfigDir(); err == nil {
		entries, readErr := os.ReadDir(kubeconfigDirectory)
		if readErr == nil {
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				name := entry.Name()
				if filepath.Ext(name) != ".yaml" && filepath.Ext(name) != ".yml" && filepath.Ext(name) != ".json" {
					continue
				}
				sources = append(sources, fileBundleSource(
					"kubernetes/"+name,
					"Redacted Porto-managed kubeconfig",
					filepath.Join(kubeconfigDirectory, name),
					false,
				))
			}
		}
	}
	if home, err := c.HomeDirectory(); err == nil && home != "" {
		globalKubeconfig := filepath.Join(home, ".kube", "config")
		if fileExists(globalKubeconfig) {
			sources = append(sources, fileBundleSource(
				"kubernetes/global-config.yaml",
				"Redacted global kubeconfig",
				globalKubeconfig,
				false,
			))
		}
	}
	if manifest := c.runtimeManifestPath(); manifest != "" {
		sources = append(sources, fileBundleSource(
			"runtime/VERSIONS",
			"Bundled runtime version and checksum manifest",
			manifest,
			false,
		))
	}
	return sources
}

func (c *Collector) RedactionContext() RedactionContext {
	c.setDefaults()
	context := RedactionContext{}
	context.HomeDirectory, _ = c.HomeDirectory()
	context.Hostname, _ = c.Hostname()
	if current, err := c.CurrentUser(); err == nil && current != nil {
		context.Username = current.Username
	}
	return context
}

func (c *Collector) setDefaults() {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.DiskCapacity == nil {
		c.DiskCapacity = diskCapacity
	}
	if c.DialContext == nil {
		dialer := &net.Dialer{Timeout: 500 * time.Millisecond}
		c.DialContext = dialer.DialContext
	}
	if c.LookupHost == nil {
		c.LookupHost = net.DefaultResolver.LookupHost
	}
	if c.Executable == nil {
		c.Executable = os.Executable
	}
	if c.HomeDirectory == nil {
		c.HomeDirectory = os.UserHomeDir
	}
	if c.Hostname == nil {
		c.Hostname = os.Hostname
	}
	if c.CurrentUser == nil {
		c.CurrentUser = user.Current
	}
}

func (c *Collector) daemonCheck() Check {
	if c.DaemonAvailable {
		if c.DaemonMessage != "" {
			return Check{
				ID: "daemon-api", Category: "porto", Name: "Daemon and dashboard", State: StateDegraded,
				Summary: "Daemon API is reachable but its diagnostics endpoint is unavailable",
				Detail:  c.DaemonMessage,
			}
		}
		if !c.DashboardReady {
			return Check{
				ID: "daemon-api", Category: "porto", Name: "Daemon and dashboard", State: StateDegraded,
				Summary: "Daemon API is reachable but dashboard assets are unavailable",
			}
		}
		return Check{
			ID: "daemon-api", Category: "porto", Name: "Daemon and dashboard", State: StateHealthy,
			Summary: "API is reachable",
		}
	}
	return Check{
		ID: "daemon-api", Category: "porto", Name: "Daemon and dashboard", State: StateUnavailable,
		Summary: "Daemon API is not reachable", Detail: "Run 'porto daemon start' to restore the dashboard and background services.",
	}
}

func (c *Collector) stateDirectoryCheck() Check {
	if strings.TrimSpace(c.StateDirectory) == "" {
		return Check{
			ID: "state-directory", Category: "host", Name: "Porto state directory", State: StateUnsafe,
			Summary: "State directory is unavailable",
		}
	}
	file, err := os.CreateTemp(c.StateDirectory, ".diagnostic-write-*")
	if err != nil {
		return Check{
			ID: "state-directory", Category: "host", Name: "Porto state directory", State: StateUnsafe,
			Summary: "State directory is not writable", Detail: err.Error(),
		}
	}
	path := file.Name()
	closeErr := file.Close()
	removeErr := os.Remove(path)
	if err := errors.Join(closeErr, removeErr); err != nil {
		return Check{
			ID: "state-directory", Category: "host", Name: "Porto state directory", State: StateDegraded,
			Summary: "State directory write probe could not be cleaned up", Detail: err.Error(),
		}
	}
	return Check{
		ID: "state-directory", Category: "host", Name: "Porto state directory", State: StateHealthy,
		Summary: "Writable", Detail: c.StateDirectory,
	}
}

func (c *Collector) diskCheck() Check {
	if c.StateDirectory == "" {
		return Check{
			ID: "disk-capacity", Category: "host", Name: "Disk capacity", State: StateUnavailable,
			Summary: "State directory is unavailable",
		}
	}
	free, total, err := c.DiskCapacity(c.StateDirectory)
	if err != nil {
		return Check{
			ID: "disk-capacity", Category: "host", Name: "Disk capacity", State: StateUnavailable,
			Summary: "Unable to inspect free disk capacity", Detail: err.Error(),
		}
	}
	return diskCapacityCheck(c.StateDirectory, free, total)
}

func (c *Collector) portChecks(ctx context.Context) []Check {
	return []Check{
		c.portCheck(ctx, "daemon-port", "Daemon API port", config.DaemonAddr, c.DaemonAvailable),
		c.portCheck(ctx, "http-router-port", "HTTP router port", config.RouterAddr, c.DaemonAvailable),
		c.portCheck(ctx, "https-router-port", "HTTPS router port", config.RouterTLSAddress(), c.DaemonAvailable),
	}
}

func (c *Collector) portCheck(ctx context.Context, id, name, address string, expected bool) Check {
	probeContext, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	connection, err := c.DialContext(probeContext, "tcp", address)
	if err == nil {
		_ = connection.Close()
		if expected {
			return Check{ID: id, Category: "network", Name: name, State: StateHealthy, Summary: "Listening", Detail: address}
		}
		return Check{
			ID: id, Category: "network", Name: name, State: StateDegraded,
			Summary: "Port is listening while the daemon was not detected", Detail: address,
		}
	}
	state := StateDegraded
	if expected {
		state = StateUnavailable
	}
	return Check{
		ID: id, Category: "network", Name: name, State: state,
		Summary: "Not listening", Detail: address + ": " + err.Error(),
	}
}

func (c *Collector) dnsCheck(ctx context.Context) Check {
	name := "diagnostics." + config.LocalhostDomain
	probeContext, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addresses, err := c.LookupHost(probeContext, name)
	if err != nil {
		return Check{
			ID: "local-dns", Category: "network", Name: "Local DNS", State: StateDegraded,
			Summary: "Porto localhost names do not resolve", Detail: err.Error(),
		}
	}
	for _, address := range addresses {
		if ip := net.ParseIP(address); ip != nil && ip.IsLoopback() {
			return Check{
				ID: "local-dns", Category: "network", Name: "Local DNS", State: StateHealthy,
				Summary: name + " resolves to loopback", Detail: strings.Join(addresses, ", "),
			}
		}
	}
	return Check{
		ID: "local-dns", Category: "network", Name: "Local DNS", State: StateDegraded,
		Summary: "Porto localhost name did not resolve to loopback", Detail: strings.Join(addresses, ", "),
	}
}

func (c *Collector) executableIntegrityCheck() Check {
	executable, err := c.Executable()
	if err != nil {
		return Check{
			ID: "bundled-runtime", Category: "porto", Name: "Bundled runtime integrity", State: StateUnavailable,
			Summary: "Unable to resolve the Porto executable", Detail: err.Error(),
		}
	}
	info, err := os.Stat(executable)
	if err != nil || info.IsDir() {
		return Check{
			ID: "bundled-runtime", Category: "porto", Name: "Bundled runtime integrity", State: StateUnsafe,
			Summary: "Porto executable is missing or invalid", Detail: errorString(err),
		}
	}
	manifest := c.runtimeManifestPath()
	packaged := strings.Contains(executable, ".app"+string(filepath.Separator)+"Contents"+string(filepath.Separator)+"Resources") ||
		strings.Contains(strings.ToLower(executable), string(filepath.Separator)+"resources"+string(filepath.Separator))
	if manifest == "" {
		state := StateHealthy
		summary := "Development executable is readable"
		if packaged {
			state = StateUnsafe
			summary = "Packaged runtime manifest is missing"
		}
		return Check{
			ID: "bundled-runtime", Category: "porto", Name: "Bundled runtime integrity", State: state,
			Summary: summary, Detail: executable,
		}
	}
	data, err := os.ReadFile(manifest)
	if err != nil || !strings.Contains(string(data), "porto-runtime-helper") {
		return Check{
			ID: "bundled-runtime", Category: "porto", Name: "Bundled runtime integrity", State: StateUnsafe,
			Summary: "Bundled runtime manifest is unreadable or incomplete", Detail: errorString(err),
		}
	}
	return Check{
		ID: "bundled-runtime", Category: "porto", Name: "Bundled runtime integrity", State: StateHealthy,
		Summary: "Runtime manifest is present", Detail: manifest,
	}
}

func (c *Collector) runtimeManifestPath() string {
	executable, err := c.Executable()
	if err != nil {
		return ""
	}
	base := filepath.Dir(executable)
	for _, candidate := range []string{
		filepath.Join(base, "runtime", "VERSIONS"),
		filepath.Join(base, "resources", "runtime", "VERSIONS"),
	} {
		if fileExists(candidate) {
			return candidate
		}
	}
	return ""
}

func (c *Collector) certificateCheck() Check {
	if c.CertificateStatus == nil {
		return Check{
			ID: "certificates", Category: "network", Name: "TLS certificates", State: StateUnavailable,
			Summary: "Certificate status is unavailable",
		}
	}
	status, err := c.CertificateStatus()
	if err != nil {
		return Check{
			ID: "certificates", Category: "network", Name: "TLS certificates", State: StateUnavailable,
			Summary: "TLS certificate is unavailable", Detail: err.Error(),
			Repair: &Repair{
				ID: "renew-certificates", Label: "Regenerate certificates",
				Description:  "Regenerate only Porto's local certificate authority and leaf certificate.",
				Confirmation: "Regenerate Porto-managed local TLS certificates?",
			},
		}
	}
	remaining := status.NotAfter.Sub(c.Now())
	state := StateHealthy
	summary := "Valid until " + status.NotAfter.Format(time.RFC3339)
	var repair *Repair
	if remaining < 0 {
		state = StateUnavailable
		summary = "Certificate has expired"
		repair = certificateRepair()
	} else if remaining < 30*24*time.Hour {
		state = StateDegraded
		summary = "Certificate expires in " + remaining.Round(time.Hour).String()
		repair = certificateRepair()
	}
	return Check{
		ID: "certificates", Category: "network", Name: "TLS certificates", State: state,
		Summary: summary, Detail: status.Fingerprint, Repair: repair,
	}
}

func certificateRepair() *Repair {
	return &Repair{
		ID: "renew-certificates", Label: "Regenerate certificates",
		Description:  "Regenerate only Porto's local certificate authority and leaf certificate.",
		Confirmation: "Regenerate Porto-managed local TLS certificates?",
	}
}

func (c *Collector) localHTTPSCheck() Check {
	certificatePath, _, err := config.CertificatePaths()
	if err != nil {
		return Check{
			ID: "local-https", Category: "network", Name: "Portless local HTTPS", State: StateUnavailable,
			Summary: "Unable to resolve certificate path", Detail: err.Error(),
		}
	}
	status := localhttps.Snapshot(certificatePath)
	if !status.Installed {
		return Check{
			ID: "local-https", Category: "network", Name: "Portless local HTTPS", State: StateHealthy,
			Summary: "Optional portless HTTPS integration is not installed",
		}
	}
	if !status.Listening || !status.Trusted {
		return Check{
			ID: "local-https", Category: "network", Name: "Portless local HTTPS", State: StateDegraded,
			Summary: "Installed but not fully operational",
			Detail:  fmt.Sprintf("listening=%t trusted=%t", status.Listening, status.Trusted),
		}
	}
	return Check{
		ID: "local-https", Category: "network", Name: "Portless local HTTPS", State: StateHealthy,
		Summary: "Installed, listening, and trusted",
	}
}

func (c *Collector) dockerChecks(ctx context.Context, settings app.Settings, settingsAvailable bool) []Check {
	if !settingsAvailable || c.Docker == nil {
		return nil
	}
	checks := make([]Check, 0, 5)
	ownershipContext, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	ownership := c.Docker.EngineOwnershipStatus(ownershipContext)
	cancel()
	checks = append(checks, ownershipCheck(ownership))
	if !settings.DockerEnabled {
		return checks
	}
	statusContext, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	status := portodocker.Status{}
	if c.DockerStatus != nil {
		status = c.DockerStatus(statusContext)
	} else {
		status = c.Docker.Status(statusContext, c.DockerSocket)
	}
	cancel()
	dockerCheck := Check{
		ID: "containerd", Category: "containers", Name: "Container runtime", State: StateHealthy,
		Summary: status.Backend, Detail: status.Message,
	}
	if !status.Available {
		dockerCheck.State = StateUnavailable
		dockerCheck.Summary = "Container runtime is unavailable"
		dockerCheck.Repair = dockerRuntimeRepair()
	} else if status.Stale {
		dockerCheck.State = StateDegraded
		dockerCheck.Summary = "Container inventory is stale"
		dockerCheck.Repair = dockerRuntimeRepair()
	}
	checks = append(checks, dockerCheck)
	if status.CanonicalPath != "" {
		contextCheck := Check{
			ID: "docker-context", Category: "containers", Name: "Canonical Docker endpoint",
			State: StateHealthy, Summary: "Canonical endpoint points to Porto", Detail: status.CanonicalPath,
		}
		if !status.Canonical {
			contextCheck.State = StateDegraded
			contextCheck.Summary = "Canonical Docker endpoint does not point to Porto"
			contextCheck.Detail = status.CanonicalLink
			contextCheck.Repair = &Repair{
				ID: "reinstall-docker-context", Label: "Reinstall Porto Docker context",
				Description:  "Update only the named Porto Docker context to use Porto's endpoint.",
				Confirmation: "Reinstall the Porto Docker context for the current user?",
			}
		}
		checks = append(checks, contextCheck)
	}
	checks = append(checks, c.dockerSocketCheck(status))
	if status.Available {
		buildContext, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
		connection, err := c.Docker.DialBuildKit(buildContext)
		buildKitCheck := Check{
			ID: "buildkit", Category: "containers", Name: "BuildKit", State: StateHealthy,
			Summary: "BuildKit connection is available",
		}
		if err != nil {
			buildKitCheck.State = StateDegraded
			buildKitCheck.Summary = "BuildKit is unavailable"
			buildKitCheck.Detail = err.Error()
			buildKitCheck.Repair = dockerRuntimeRepair()
		} else {
			_ = connection.Close()
		}
		cancel()
		checks = append(checks, buildKitCheck)
		snapshot := c.Docker.ContainerSnapshot()
		cni := snapshot.Capabilities.NetworkUpdates
		cniCheck := Check{
			ID: "container-networking", Category: "containers", Name: "Container networking",
			State: StateHealthy, Summary: "CNI network updates are available",
		}
		if !cni.Supported {
			cniCheck.State = StateDegraded
			cniCheck.Summary = "CNI network updates are unavailable"
			cniCheck.Detail = cni.Reason
		}
		checks = append(checks, cniCheck)
	}
	return checks
}

func ownershipCheck(status portodocker.EngineOwnershipStatus) Check {
	check := Check{
		ID: "engine-ownership", Category: "containers", Name: "Engine ownership",
		State: StateHealthy, Summary: status.Message,
	}
	switch {
	case status.Conflict:
		check.State = StateUnsafe
		check.Summary = "Engine ownership is ambiguous; automatic repair is disabled"
		check.Detail = status.Message
	case !status.Configured:
		check.Summary = "No Porto-managed engine metadata exists"
	case status.Verified:
		check.Summary = "Ownership marker verified"
	case status.Owned:
		check.State = StateDegraded
		check.Summary = "Ownership metadata exists but the guest marker was not verified"
		check.Detail = status.Message
	default:
		check.State = StateUnavailable
		check.Summary = "Configured engine is missing"
		check.Detail = status.Message
	}
	return check
}

func dockerRuntimeRepair() *Repair {
	return &Repair{
		ID: "restart-docker-runtime", Label: "Rebuild Docker runtime connection",
		Description:  "Restart Porto's Docker API, containerd event subscription, and owned tunnels without deleting containers or the engine VM.",
		Confirmation: "Restart Porto's Docker API and rebuild its owned runtime connections?",
	}
}

func (c *Collector) dockerSocketCheck(status portodocker.Status) Check {
	if runtime.GOOS == "windows" {
		state := StateUnavailable
		summary := "Named pipe is unavailable"
		if status.Available {
			state = StateHealthy
			summary = "Named pipe is available"
		}
		return Check{
			ID: "docker-socket", Category: "containers", Name: "Docker endpoint",
			State: state, Summary: summary, Detail: c.DockerSocket,
		}
	}
	info, err := os.Stat(c.DockerSocket)
	if err != nil {
		return Check{
			ID: "docker-socket", Category: "containers", Name: "Docker endpoint",
			State: StateUnavailable, Summary: "Docker socket is missing", Detail: err.Error(),
			Repair: dockerRuntimeRepair(),
		}
	}
	if info.Mode()&os.ModeSocket == 0 {
		return Check{
			ID: "docker-socket", Category: "containers", Name: "Docker endpoint",
			State: StateUnsafe, Summary: "Docker endpoint path is not a socket", Detail: c.DockerSocket,
		}
	}
	return Check{
		ID: "docker-socket", Category: "containers", Name: "Docker endpoint",
		State: StateHealthy, Summary: "Socket is present", Detail: c.DockerSocket,
	}
}

func (c *Collector) kubernetesChecks(ctx context.Context, settings app.Settings, settingsAvailable bool) []Check {
	if !settingsAvailable || c.Kubernetes == nil || !settings.KubernetesEnabled {
		return nil
	}
	checks := make([]Check, 0, 4)
	statusContext, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	status := c.Kubernetes.Status(statusContext, "")
	cancel()
	statusCheck := Check{
		ID: "kubernetes-api", Category: "kubernetes", Name: "Kubernetes API",
		State: StateHealthy, Summary: status.Context + " " + status.ServerVersion,
	}
	if !status.Available {
		statusCheck.State = StateUnavailable
		statusCheck.Summary = "No reachable Kubernetes API"
		if strings.Contains(status.Message, "No Porto-managed Kubernetes cluster exists") {
			statusCheck.State = StateDegraded
			statusCheck.Summary = "No Kubernetes cluster is configured"
		}
		statusCheck.Detail = status.Message
	}
	checks = append(checks, statusCheck)
	contextContext, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	contexts, err := c.Kubernetes.Contexts(contextContext)
	cancel()
	if err != nil {
		checks = append(checks, Check{
			ID: "kubeconfigs", Category: "kubernetes", Name: "Kubeconfigs", State: StateUnavailable,
			Summary: "Kubeconfig discovery failed", Detail: err.Error(),
		})
	} else if len(contexts) == 0 {
		checks = append(checks, Check{
			ID: "kubeconfigs", Category: "kubernetes", Name: "Kubeconfigs", State: StateDegraded,
			Summary: "No Kubernetes contexts were found",
		})
	} else {
		checks = append(checks, Check{
			ID: "kubeconfigs", Category: "kubernetes", Name: "Kubeconfigs", State: StateHealthy,
			Summary: fmt.Sprintf("%d context(s) parsed", len(contexts)),
		})
	}
	if status.Available && c.Clusters != nil {
		clusterContext, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
		clusters, listErr := c.Clusters.List(clusterContext)
		cancel()
		if listErr != nil {
			checks = append(checks, Check{
				ID: "kubernetes-addons", Category: "kubernetes", Name: "Managed add-ons", State: StateDegraded,
				Summary: "Unable to inspect managed clusters", Detail: listErr.Error(),
			})
		} else {
			for _, cluster := range clusters {
				if !strings.EqualFold(cluster.State, "running") {
					continue
				}
				checks = append(checks, c.kubernetesAddonCheck(ctx, cluster))
			}
		}
	}
	return checks
}

func (c *Collector) kubernetesAddonCheck(ctx context.Context, cluster kubernetes.Cluster) Check {
	check := Check{
		ID: "kubernetes-addons-" + identifier(cluster.Name), Category: "kubernetes",
		Name: "Managed add-ons: " + cluster.Name, State: StateHealthy,
		Summary: "Metrics API and Gateway API are available",
	}
	probeContext, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	_, metricsErr := c.Kubernetes.ResourceStats(probeContext, cluster.Context)
	cancel()
	probeContext, cancel = context.WithTimeout(ctx, diagnosticProbeTimeout)
	gatewayClasses, gatewayErr := c.Kubernetes.GatewayClasses(probeContext, cluster.Context)
	cancel()
	var failures []string
	if metricsErr != nil {
		failures = append(failures, "metrics: "+metricsErr.Error())
	}
	if gatewayErr != nil {
		failures = append(failures, "gateway: "+gatewayErr.Error())
	} else if len(gatewayClasses) == 0 {
		failures = append(failures, "gateway: no GatewayClass is installed")
	}
	if len(failures) == 0 {
		return check
	}
	check.State = StateDegraded
	check.Summary = "One or more Porto-managed add-ons need repair"
	check.Detail = strings.Join(failures, "; ")
	check.Repair = &Repair{
		ID: "repair-kubernetes-addons", Target: cluster.Name, Label: "Repair managed add-ons",
		Description:  "Reapply only Porto-managed storage, metrics, and Gateway API add-ons for this owned cluster.",
		Confirmation: fmt.Sprintf("Repair Porto-managed add-ons for cluster %q?", cluster.Name),
	}
	return check
}

func (c *Collector) vmChecks(ctx context.Context, settings app.Settings, settingsAvailable bool) []Check {
	if !settingsAvailable || c.VMs == nil || !settings.VMsEnabled {
		return nil
	}
	statusContext, cancel := context.WithTimeout(ctx, diagnosticProbeTimeout)
	status := c.VMs.Status(statusContext)
	cancel()
	check := Check{
		ID: "virtual-machines", Category: "virtual-machines", Name: "Lima virtual machines",
		State: StateHealthy, Summary: status.Version,
	}
	if !status.Available {
		check.State = StateUnavailable
		check.Summary = "Virtual machine provider is unavailable"
		check.Detail = status.Message
	}
	return []Check{check}
}

func fileBundleSource(name, description, filePath string, tail bool) BundleSource {
	return BundleSource{
		Name: name, Description: description,
		Read: func() ([]byte, error) {
			if tail {
				return readFileTail(filePath, maxBundleFileBytes)
			}
			data, err := os.ReadFile(filePath)
			if err != nil {
				return nil, err
			}
			if len(data) > maxBundleFileBytes {
				return data[:maxBundleFileBytes], nil
			}
			return data, nil
		},
	}
}

func readFileTail(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	offset := max(int64(0), info.Size()-int64(limit))
	if _, err := file.Seek(offset, 0); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(file, int64(limit)))
}

func inspectCertificate(certificatePath, keyPath, authorityPath string) (certificates.Status, error) {
	certificateData, err := os.ReadFile(certificatePath)
	if err != nil {
		return certificates.Status{}, err
	}
	block, _ := pem.Decode(certificateData)
	if block == nil || block.Type != "CERTIFICATE" {
		return certificates.Status{}, errors.New("decode Porto TLS certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return certificates.Status{}, err
	}
	if _, err := os.Stat(keyPath); err != nil {
		return certificates.Status{}, fmt.Errorf("inspect Porto TLS private key: %w", err)
	}
	if _, err := os.Stat(authorityPath); err != nil {
		return certificates.Status{}, fmt.Errorf("inspect Porto TLS certificate authority: %w", err)
	}
	sum := sha256.Sum256(certificate.Raw)
	return certificates.Status{
		CertificatePath:          certificatePath,
		KeyPath:                  keyPath,
		CertificateAuthorityPath: authorityPath,
		DNSNames:                 append([]string(nil), certificate.DNSNames...),
		NotBefore:                certificate.NotBefore,
		NotAfter:                 certificate.NotAfter,
		Fingerprint:              hex.EncodeToString(sum[:]),
	}, nil
}

func identifier(value string) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(value) {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') {
			builder.WriteRune(character)
		} else if builder.Len() > 0 && !strings.HasSuffix(builder.String(), "-") {
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
