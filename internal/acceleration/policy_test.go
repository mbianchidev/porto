package acceleration

import (
	"strings"
	"testing"
)

func TestArchitecturePolicySelection(t *testing.T) {
	for _, scenario := range []struct {
		name, guest, policy, platform, handler, execution, failure string
		binfmt, rosetta                                            bool
	}{
		{name: "native needs no emulator", guest: "arm64", policy: Automatic, platform: "linux/arm64", execution: "native"},
		{name: "native default platform", guest: "aarch64", policy: NativeOnly, execution: "native"},
		{name: "native only rejects amd64", guest: "arm64", policy: NativeOnly, platform: "linux/amd64", failure: "native-only"},
		{name: "Rosetta preferred", guest: "arm64", policy: RosettaPreferred, platform: "linux/amd64", handler: "rosetta", rosetta: true, execution: "rosetta"},
		{name: "verified QEMU fallback", guest: "arm64", policy: RosettaPreferred, platform: "linux/amd64", handler: "qemu", binfmt: true, execution: "qemu"},
		{name: "QEMU cannot claim Rosetta", guest: "arm64", policy: QEMUCompatible, platform: "linux/amd64", handler: "rosetta", rosetta: true, failure: "not ready"},
		{name: "missing handler", guest: "arm64", policy: Automatic, platform: "linux/amd64", failure: "not ready"},
		{name: "unknown backend", policy: Automatic, failure: "architecture is unavailable"},
		{name: "unknown policy", guest: "arm64", policy: "synthetic-invalid", failure: "policy must be"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result, err := Select(Capabilities{GuestArchitecture: scenario.guest, AMD64Handler: scenario.handler,
				Rosetta: Capability{Supported: scenario.rosetta}, Binfmt: Capability{Supported: scenario.binfmt}},
				scenario.policy, scenario.platform)
			if scenario.failure != "" {
				if err == nil || !strings.Contains(err.Error(), scenario.failure) {
					t.Fatalf("selection=%+v, error=%v", result, err)
				}
			} else if err != nil || result.Execution != scenario.execution {
				t.Fatalf("selection=%+v, error=%v", result, err)
			}
		})
	}
}

func TestArchitectureOverridePrecedence(t *testing.T) {
	policy, err := EffectivePolicy(Automatic, QEMUCompatible, NativeOnly)
	if err != nil || policy != NativeOnly {
		t.Fatalf("workload override did not win: %q, %v", policy, err)
	}
	if _, err := EffectivePolicy(Automatic, "synthetic-invalid"); err == nil {
		t.Fatal("invalid override was ignored")
	}
}

func TestRosettaDoesNotAccelerateForeignVMKernels(t *testing.T) {
	host := Capabilities{HostArchitecture: "arm64", QEMUVersion: "synthetic-qemu",
		QEMUArchitectures: []string{"amd64"}, GuestArchitectures: []string{"amd64", "arm64"},
		Virtualization: Capability{Supported: true}, Rosetta: Capability{Supported: true}}
	if _, err := SelectVM(host, RosettaPreferred, "amd64", "vz", true); err == nil {
		t.Fatal("Rosetta was incorrectly advertised as amd64 kernel virtualization")
	}
	result, err := SelectVM(host, QEMUCompatible, "amd64", "qemu", false)
	if err != nil || result.Execution != "qemu" {
		t.Fatalf("foreign kernel selection = %+v, %v", result, err)
	}
	if _, err := SelectVM(host, NativeOnly, "amd64", "qemu", false); err == nil {
		t.Fatal("native-only allowed a foreign VM")
	}
	if _, err := SelectVM(host, RosettaPreferred, "arm64", "vz", true); err != nil {
		t.Fatal(err)
	}
}
