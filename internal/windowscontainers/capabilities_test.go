package windowscontainers

import (
	"strings"
	"testing"
)

func supportedHost() Host {
	return Host{OS: "windows", Architecture: "amd64", Build: 26100, Edition: "ServerDatacenter",
		Server: true, Containers: true, HyperV: true, Virtualization: true}
}

func TestHostCapabilityRejectsUnsupportedHostsBeforeProvisioning(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Host)
		want string
	}{
		{"non-windows", func(host *Host) { host.OS = "linux" }, "Windows"},
		{"arm64", func(host *Host) { host.Architecture = "arm64" }, "amd64"},
		{"home", func(host *Host) { host.Server = false; host.Edition = "Core" }, "edition"},
		{"feature", func(host *Host) { host.Containers = false }, "Containers"},
		{"old-build", func(host *Host) { host.Build = 17763 }, "build"},
		{"inspection", func(host *Host) { host.Problem = "synthetic permission denial" }, "permission denial"},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := supportedHost()
			test.edit(&host)
			if err := host.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("capability = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNativeImageCompatibilityUsesOSArchitectureBuildAndIsolation(t *testing.T) {
	for _, test := range []struct {
		name      string
		image     ImagePlatform
		isolation string
		valid     bool
	}{
		{"matching-process", ImagePlatform{"windows", "amd64", "10.0.26100.1000"}, "process", true},
		{"revision-difference", ImagePlatform{"windows", "amd64", "10.0.26100.9999"}, "process", true},
		{"mismatched-process", ImagePlatform{"windows", "amd64", "10.0.20348.1"}, "process", false},
		{"older-hyperv", ImagePlatform{"windows", "amd64", "10.0.20348.1"}, "hyperv", true},
		{"future-hyperv", ImagePlatform{"windows", "amd64", "10.0.30000.1"}, "hyperv", false},
		{"linux", ImagePlatform{"linux", "amd64", ""}, "process", false},
		{"arm64", ImagePlatform{"windows", "arm64", "10.0.26100.1"}, "process", false},
		{"missing-build", ImagePlatform{"windows", "amd64", ""}, "process", false},
		{"host-process", ImagePlatform{"windows", "amd64", "10.0.26100.1"}, "host", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := supportedHost().ValidateImage(test.image, test.isolation); (err == nil) != test.valid {
				t.Fatalf("image compatibility = %v, want valid=%t", err, test.valid)
			}
		})
	}
}

func TestNativeWindowsContainersArchitectureUsesTheNativeMachineNotEmulation(t *testing.T) {
	for machine, want := range map[uint16]string{0x8664: "amd64", 0xaa64: "arm64", 0x14c: "386", 0: "unknown"} {
		if actual := architectureForMachine(machine); actual != want {
			t.Fatalf("native architecture %#x=%s, want %s", machine, actual, want)
		}
	}
}

func TestUnelevatedCapabilityInspectionStillOffersExplicitSetup(t *testing.T) {
	host := supportedHost()
	host.Problem = "administrator inspection required"
	host.Containers, host.HyperV, host.Virtualization = false, false, false
	status := statusForHost(host)
	if !status.Supported || status.Capabilities.Process.Supported || status.Capabilities.HyperV.Supported {
		t.Fatalf("unelevated platform/capability status = %+v", status)
	}
	host.Architecture = "arm64"
	if host.PotentiallySupported() {
		t.Fatal("native ARM64 was offered an AMD64 runtime")
	}
}

func TestHyperVRequiresActualFeatureAndVirtualization(t *testing.T) {
	image := ImagePlatform{"windows", "amd64", "10.0.20348.1"}
	for _, edit := range []func(*Host){
		func(host *Host) { host.HyperV = false },
		func(host *Host) { host.Virtualization = false },
	} {
		host := supportedHost()
		edit(&host)
		if err := host.ValidateImage(image, "hyperv"); err == nil {
			t.Fatal("Hyper-V isolation was inferred without a usable host capability")
		}

	}
	host := supportedHost()
	host.Server, host.Edition = false, "Professional"
	if err := host.ValidateImage(ImagePlatform{"windows", "amd64", "10.0.26100.1"}, "process"); err == nil {
		t.Fatal("client process isolation was advertised without native acceptance")
	}
}
