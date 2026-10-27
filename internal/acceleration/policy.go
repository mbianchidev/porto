package acceleration

import (
	"errors"
	"fmt"
	"strings"

	"github.com/containerd/platforms"
)

const (
	Automatic        = "automatic"
	NativeOnly       = "native-only"
	RosettaPreferred = "rosetta-preferred"
	QEMUCompatible   = "qemu-compatible"
	PolicyLabel      = "dev.porto.architecture-policy"
	ExecutionLabel   = "io.porto.container.execution"
	PlatformLabel    = "io.porto.container.platform"
)

type Settings struct {
	Policy         string `json:"policy"`
	RosettaEnabled bool   `json:"rosettaEnabled"`
}

type Capability struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

type Capabilities struct {
	HostOS             string            `json:"hostOS"`
	HostArchitecture   string            `json:"hostArchitecture"`
	GuestArchitecture  string            `json:"guestArchitecture,omitempty"`
	VMType             string            `json:"vmType,omitempty"`
	LimaVersion        string            `json:"limaVersion,omitempty"`
	QEMUVersion        string            `json:"qemuVersion,omitempty"`
	QEMUArchitectures  []string          `json:"qemuArchitectures,omitempty"`
	GuestArchitectures []string          `json:"guestArchitectures,omitempty"`
	Virtualization     Capability        `json:"virtualization"`
	Rosetta            Capability        `json:"rosetta"`
	RosettaHost        Capability        `json:"rosettaHost"`
	Binfmt             Capability        `json:"binfmt"`
	AMD64Handler       string            `json:"amd64Handler,omitempty"`
	Handlers           map[string]string `json:"handlers,omitempty"`
}

type Selection struct {
	Platform  string `json:"platform"`
	Policy    string `json:"policy"`
	Execution string `json:"execution"`
	Reason    string `json:"reason,omitempty"`
}

func Defaults() Settings { return Settings{Policy: Automatic} }

func Validate(settings Settings) error {
	switch settings.Policy {
	case Automatic, NativeOnly, RosettaPreferred, QEMUCompatible:
	default:
		return fmt.Errorf("architecture policy must be %s, %s, %s, or %s", Automatic, NativeOnly, RosettaPreferred, QEMUCompatible)
	}
	if settings.RosettaEnabled && (settings.Policy == NativeOnly || settings.Policy == QEMUCompatible) {
		return errors.New("native-only and QEMU-compatible policies cannot enable Rosetta translation")
	}
	return nil
}

func EffectivePolicy(global string, overrides ...string) (string, error) {
	policy := global
	if policy == "" {
		policy = Automatic
	}
	for _, override := range overrides {
		if override != "" {
			policy = override
		}
	}
	if err := Validate(Settings{Policy: policy}); err != nil {
		return "", err
	}
	return policy, nil
}

func Architecture(value string) string {
	switch strings.ToLower(value) {
	case "x86_64", "x86-64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return strings.ToLower(value)
	}
}

func Select(capabilities Capabilities, policy, platform string) (Selection, error) {
	policy, err := EffectivePolicy(policy)
	if err != nil {
		return Selection{}, err
	}
	guest := Architecture(capabilities.GuestArchitecture)
	if guest == "" {
		return Selection{}, errors.New("backend architecture is unavailable; refresh runtime capabilities before creating a workload")
	}
	if platform == "" {
		platform = "linux/" + guest
	}
	target, err := platforms.Parse(platform)
	if err != nil || target.OS != "linux" {
		return Selection{}, fmt.Errorf("unsupported Linux workload platform %q", platform)
	}
	selection := Selection{Platform: platforms.Format(platforms.Normalize(target)), Policy: policy, Execution: "unsupported"}
	if target.Architecture == guest {
		selection.Execution = "native"
		return selection, nil
	}
	if policy == NativeOnly {
		return selection, fmt.Errorf("native-only policy rejects %s on a linux/%s backend", selection.Platform, guest)
	}
	if target.Architecture == "amd64" && guest == "arm64" {
		if capabilities.AMD64Handler == "rosetta" && capabilities.Rosetta.Supported && policy != QEMUCompatible {
			selection.Execution = "rosetta"
			selection.Reason = "amd64 Linux userspace translated inside the native arm64 VZ guest"
			return selection, nil
		}
		if capabilities.AMD64Handler == "qemu" && capabilities.Binfmt.Supported {
			selection.Execution = "qemu"
			if policy == RosettaPreferred {
				selection.Reason = "Rosetta is unavailable in this backend; using its verified QEMU handler"
			}
			return selection, nil
		}
		return selection, errors.New("amd64 translation is not ready; explicitly apply the selected architecture policy to the owned engine, or install/configure its QEMU binfmt handler")
	}
	if capabilities.Binfmt.Supported && capabilities.Handlers[target.Architecture] == "qemu" {
		selection.Execution = "qemu"
		return selection, nil
	}
	return selection, fmt.Errorf("no verified container binfmt handler is available for %s; configure emulation in the selected backend, not on the host", selection.Platform)
}

func SelectVM(capabilities Capabilities, policy, guest, vmType string, rosetta bool) (Selection, error) {
	if guest == "" {
		guest = capabilities.HostArchitecture
	}
	guest = Architecture(guest)
	policy, err := EffectivePolicy(policy)
	if err != nil {
		return Selection{}, err
	}
	selection := Selection{Platform: "linux/" + guest, Policy: policy, Execution: "native"}
	if guest != "amd64" && guest != "arm64" {
		return selection, fmt.Errorf("unsupported VM architecture %q", guest)
	}
	if rosetta {
		if policy == NativeOnly || policy == QEMUCompatible {
			return selection, errors.New("the selected VM policy does not allow Rosetta; disable its Rosetta userspace option")
		}
		if guest != "arm64" || vmType != "vz" || !capabilities.Rosetta.Supported {
			return selection, errors.New("Rosetta requires an arm64 Linux VM using VZ on a supported Apple Silicon Mac with Rosetta already installed; it cannot accelerate an amd64 guest kernel")
		}
	}
	if guest != Architecture(capabilities.HostArchitecture) {
		if vmType == "vz" {
			return selection, errors.New("VZ cannot boot a foreign-architecture kernel; choose QEMU, or use an arm64 VM with optional Rosetta userspace translation")
		}
		if policy == NativeOnly {
			return selection, fmt.Errorf("native-only policy rejects an %s VM on %s", guest, capabilities.HostArchitecture)
		}
		matchingQEMU := false
		for _, architecture := range capabilities.QEMUArchitectures {
			matchingQEMU = matchingQEMU || architecture == guest
		}
		if !matchingQEMU {
			return selection, errors.New("foreign-architecture VMs require the matching QEMU system emulator; install QEMU and refresh capabilities")
		}
		matchingAgent := false
		for _, architecture := range capabilities.GuestArchitectures {
			matchingAgent = matchingAgent || architecture == guest
		}
		if !matchingAgent {
			return selection, errors.New("Lima's matching additional Linux guest agent is missing; install lima-additional-guestagents and refresh capabilities")
		}
		selection.Execution = "qemu"
		selection.Reason = "full-system software emulation; Rosetta does not accelerate the guest kernel"
	}
	if vmType == "vz" && !capabilities.Virtualization.Supported {
		return selection, fmt.Errorf("VZ is unavailable: %s", capabilities.Virtualization.Reason)
	}
	return selection, nil
}
