package diagnostics

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/dockercli"
	"github.com/mbianchidev/porto/internal/providers"
)

const (
	unsafeFreeDiskBytes   = 512 * 1024 * 1024
	degradedFreeDiskBytes = 2 * 1024 * 1024 * 1024
)

func diskCapacityCheck(path string, free, total uint64) Check {
	check := Check{
		ID:       "disk-capacity",
		Category: "host",
		Name:     "Disk capacity",
		State:    StateHealthy,
		Summary:  fmt.Sprintf("%s free of %s", formatBytes(free), formatBytes(total)),
		Detail:   path,
	}
	switch {
	case free < unsafeFreeDiskBytes:
		check.State = StateUnsafe
		check.Summary = fmt.Sprintf("Only %s is free; repairs and image operations are unsafe", formatBytes(free))
	case free < degradedFreeDiskBytes:
		check.State = StateDegraded
		check.Summary = fmt.Sprintf("Only %s is free; cleanup is recommended", formatBytes(free))
	}
	return check
}

func runtimeGateChecks(settings app.Settings) []Check {
	return []Check{
		runtimeGateCheck("docker", "Docker", settings.DockerEnabled),
		runtimeGateCheck("kubernetes", "Kubernetes", settings.KubernetesEnabled),
		runtimeGateCheck("vms", "Virtual machines", settings.VMsEnabled),
	}
}

func runtimeGateCheck(id, name string, enabled bool) Check {
	summary := "Disabled by settings"
	if enabled {
		summary = "Enabled"
	}
	return Check{
		ID:       "runtime-" + id,
		Category: "runtime",
		Name:     name,
		State:    StateHealthy,
		Summary:  summary,
	}
}

func providerChecks(settings app.Settings, statuses []providers.Status) []Check {
	checks := make([]Check, 0, len(statuses))
	for _, status := range statuses {
		name := strings.TrimSpace(status.Name)
		displayName := "Unknown provider"
		if name != "" {
			displayName = strings.ToUpper(name[:1]) + name[1:]
		}
		check := Check{
			ID:       "provider-" + identifier(name),
			Category: "tools",
			Name:     displayName,
			State:    StateHealthy,
			Summary:  status.Version,
			Detail:   status.Command,
		}
		if status.Installed {
			if check.Summary == "" {
				check.Summary = "Installed"
			}
			checks = append(checks, check)
			continue
		}
		check.Summary = status.Message
		required := settings.VMsEnabled &&
			(name == "lima" || (name == "qemu" && runtime.GOOS != "darwin"))
		relevant := settings.DockerEnabled || settings.KubernetesEnabled || settings.VMsEnabled
		switch {
		case required:
			check.State = StateUnavailable
		case relevant:
			check.State = StateDegraded
		default:
			check.Summary = "Not installed; not required by enabled runtimes"
		}
		checks = append(checks, check)
	}
	return checks
}

func dockerToolchainChecks(statuses []dockercli.Status) []Check {
	checks := make([]Check, 0, len(statuses))
	for _, status := range statuses {
		name := status.Name
		if name == "docker" {
			name = "Docker CLI"
		} else if name == "dive" {
			name = "Dive image layers"
		} else {
			name = "Docker " + strings.ToUpper(name[:1]) + name[1:]
		}
		check := Check{
			ID:       "docker-tool-" + status.Name,
			Category: "tools",
			Name:     name,
			State:    StateHealthy,
			Summary:  status.Version,
			Detail:   status.Path,
		}
		switch {
		case !status.Supported:
			check.Summary = status.Message
		case status.Installed && status.Message == "":
			if check.Summary == "" {
				check.Summary = "Installed"
			}
		case status.Installed:
			check.State = StateDegraded
			check.Summary = status.Message
		case status.BundledExpected:
			check.State = StateUnavailable
			check.Summary = status.Message
			if check.Summary == "" {
				check.Summary = "Promised bundled executable is missing"
			}
		default:
			check.State = StateDegraded
			check.Summary = status.Message
			if check.Summary == "" {
				check.Summary = "Not installed"
			}
		}
		checks = append(checks, check)
	}
	return checks
}

func formatBytes(value uint64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
		tib = 1024 * gib
	)
	switch {
	case value >= tib:
		return fmt.Sprintf("%.1f TiB", float64(value)/tib)
	case value >= gib:
		return fmt.Sprintf("%.1f GiB", float64(value)/gib)
	case value >= mib:
		return fmt.Sprintf("%.1f MiB", float64(value)/mib)
	case value >= kib:
		return fmt.Sprintf("%.1f KiB", float64(value)/kib)
	default:
		return fmt.Sprintf("%d B", value)
	}
}
