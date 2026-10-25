package windowscontainers

import (
	"debug/pe"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Host struct {
	OS             string `json:"os"`
	Architecture   string `json:"architecture"`
	Build          int    `json:"build"`
	Edition        string `json:"edition"`
	Server         bool   `json:"server"`
	Containers     bool   `json:"containers"`
	HyperV         bool   `json:"hyperV"`
	Virtualization bool   `json:"virtualization"`
	Problem        string `json:"problem,omitempty"`
}

type ImagePlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	OSVersion    string `json:"os.version"`
}

func architectureForMachine(machine uint16) string {
	switch machine {
	case pe.IMAGE_FILE_MACHINE_AMD64:
		return "amd64"
	case pe.IMAGE_FILE_MACHINE_ARM64:
		return "arm64"
	case pe.IMAGE_FILE_MACHINE_I386:
		return "386"
	default:
		return "unknown"
	}
}

// PotentiallySupported does not treat an unelevated feature probe as an
// unsupported platform. Feature-dependent capabilities still require Validate.
func (h Host) PotentiallySupported() bool {
	if h.OS != "windows" || h.Architecture != "amd64" {
		return false
	}
	if h.Server {
		return h.Build == 20348 || h.Build == 26100
	}
	edition := strings.ToLower(h.Edition)
	return h.Build >= 22000 && (strings.HasPrefix(edition, "professional") ||
		strings.HasPrefix(edition, "enterprise") || strings.HasPrefix(edition, "education"))
}

func (h Host) Validate() error {
	if h.Problem != "" {
		return fmt.Errorf("native Windows capability inspection failed: %s", h.Problem)
	}
	if h.OS != "windows" {
		return errors.New("native Windows containers require a Windows host")
	}
	if h.Architecture != "amd64" {
		return errors.New("native Windows containers currently require amd64; Linux containers and WSL tooling remain separate, with no x64 binary substitution")
	}
	edition := strings.ToLower(h.Edition)
	if !h.Server && !(strings.HasPrefix(edition, "professional") ||
		strings.HasPrefix(edition, "enterprise") || strings.HasPrefix(edition, "education")) {
		return fmt.Errorf("Windows edition %q does not support this native container mode", h.Edition)
	}
	if (h.Server && h.Build != 20348 && h.Build != 26100) || (!h.Server && h.Build < 22000) {
		return fmt.Errorf("Windows build %d is outside the supported Server 2022/2025 or Windows 11 host matrix", h.Build)
	}
	if !h.Containers {
		return errors.New("enable the Windows Containers feature and complete any requested reboot before installing the native runtime; Porto will not change host features automatically")
	}
	if !h.Server && (!h.HyperV || !h.Virtualization) {
		return errors.New("Windows client native containers require enabled Hyper-V and verified hardware or nested virtualization")
	}
	return nil
}

func (h Host) Isolation(requested string) (string, error) {
	if err := h.Validate(); err != nil {
		return "", err
	}
	if requested == "" || requested == "default" {
		if h.Server {
			requested = "process"
		} else {
			requested = "hyperv"
		}
	}
	switch requested {
	case "process":
		if !h.Server {
			return "", errors.New("native process isolation is capability-gated to supported Windows Server hosts; select Hyper-V isolation on Windows clients")
		}
	case "hyperv":
		if !h.HyperV || !h.Virtualization {
			return "", errors.New("Hyper-V isolation requires the enabled feature and verified hardware or nested virtualization")
		}
	default:
		return "", errors.New("native Windows isolation must be process or hyperv; host-process containers are not supported")
	}
	return requested, nil
}

func (h Host) ValidateImage(image ImagePlatform, requested string) error {
	isolation, err := h.Isolation(requested)
	if err != nil {
		return err
	}
	if image.OS != "windows" || image.Architecture != h.Architecture {
		return fmt.Errorf("native Windows mode requires a windows/%s image, not %s/%s; use the separate Linux endpoint for Linux images",
			h.Architecture, image.OS, image.Architecture)
	}
	version := strings.Split(image.OSVersion, ".")
	if len(version) != 4 || version[0] != "10" || version[1] != "0" {
		return fmt.Errorf("Windows image must declare a valid 10.0.build.revision OS version, got %q", image.OSVersion)
	}
	build, err := strconv.Atoi(version[2])
	if err != nil {
		return fmt.Errorf("invalid Windows image build %q: %w", image.OSVersion, err)
	}
	revision, err := strconv.Atoi(version[3])
	if err != nil || revision < 0 {
		return fmt.Errorf("invalid Windows image revision %q", image.OSVersion)
	}
	if build != 17763 && build != 20348 && build != 26100 {
		return fmt.Errorf("Windows image build %d has no supported native runtime contract", build)
	}
	if isolation == "process" && build != h.Build {
		return fmt.Errorf("process-isolated image build %d does not match host build %d; select supported Hyper-V isolation or a matching image", build, h.Build)
	}
	if isolation == "hyperv" && build > h.Build {
		return fmt.Errorf("Hyper-V image build %d is newer than host build %d", build, h.Build)
	}
	return nil
}
