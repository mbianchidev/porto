package diagnostics

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/dockercli"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/providers"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type diagnosticRunner struct {
	output []byte
	err    error
}

func (r diagnosticRunner) Run(context.Context, runtimes.Command) ([]byte, error) {
	return r.output, r.err
}

func TestDiskCapacityCheckMarksUnsafeRepairConditions(t *testing.T) {
	check := diskCapacityCheck("/tmp", 256*1024*1024, 100*1024*1024*1024)
	if check.State != StateUnsafe {
		t.Fatalf("state = %q, want %q", check.State, StateUnsafe)
	}
	if check.Repair != nil {
		t.Fatalf("low disk check offered an unsafe repair: %+v", check.Repair)
	}
}

func TestProviderChecksRespectEnabledRuntimeRequirements(t *testing.T) {
	checks := providerChecks(app.Settings{VMsEnabled: true}, []providers.Status{
		{Name: "lima", Command: "limactl", Installed: false, Message: "limactl is not installed"},
		{Name: "qemu", Command: "qemu-system-aarch64", Installed: false, Message: "QEMU is not installed"},
	})
	if len(checks) != 2 {
		t.Fatalf("provider checks = %d, want 2", len(checks))
	}
	if checks[0].State != StateUnavailable {
		t.Fatalf("Lima state = %q, want unavailable", checks[0].State)
	}
	qemuState := StateUnavailable
	if runtime.GOOS == "darwin" {
		qemuState = StateDegraded
	}
	if checks[1].State != qemuState {
		t.Fatalf("QEMU state = %q, want %q", checks[1].State, qemuState)
	}
}

func TestDisabledRuntimeGateIsHealthy(t *testing.T) {
	checks := runtimeGateChecks(app.Settings{})
	for _, check := range checks {
		if check.State != StateHealthy || check.Summary != "Disabled by settings" {
			t.Fatalf("unexpected disabled runtime check: %+v", check)
		}
	}
}

func TestDockerToolchainChecksFailMissingPromisedPlugin(t *testing.T) {
	checks := dockerToolchainChecks([]dockercli.Status{{
		Name: "compose", Supported: true, BundledExpected: true,
		Message: "docker compose failed: executable not found",
	}})
	if len(checks) != 1 || checks[0].State != StateUnavailable {
		t.Fatalf("unexpected Docker toolchain check: %+v", checks)
	}
}

func TestDockerToolchainChecksExplainUnsupportedPlatform(t *testing.T) {
	checks := dockerToolchainChecks([]dockercli.Status{{
		Name: "buildx", Supported: false,
		Message: "not available because Docker CLI is not bundled for windows/arm64",
	}})
	if len(checks) != 1 || checks[0].State != StateHealthy ||
		!strings.Contains(checks[0].Summary, "windows/arm64") {
		t.Fatalf("unexpected unsupported toolchain check: %+v", checks)
	}
}

func TestDockerSocketCheckRejectsStaleRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows Docker endpoints use named pipes")
	}
	socketPath := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(socketPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	check := (&Collector{DockerSocket: socketPath}).dockerSocketCheck(portodocker.Status{Available: true})
	if check.State != StateUnsafe || !strings.Contains(check.Summary, "not a socket") {
		t.Fatalf("unexpected stale socket check: %+v", check)
	}
}

func TestDockerContextCheckMatchesRepairScope(t *testing.T) {
	expected := "unix:///tmp/porto.sock"
	healthy := dockerContextCheck(portodocker.ContextStatus{
		Installed: true, Endpoint: expected, ExpectedEndpoint: expected, Matches: true,
	})
	if healthy.State != StateHealthy || healthy.Repair != nil {
		t.Fatalf("healthy context check = %+v", healthy)
	}
	degraded := dockerContextCheck(portodocker.ContextStatus{
		ExpectedEndpoint: expected,
		Message:          "Porto Docker context is missing",
	})
	if degraded.State != StateDegraded || degraded.Repair == nil ||
		degraded.Repair.ID != "reinstall-docker-context" {
		t.Fatalf("degraded context check = %+v", degraded)
	}
}

func TestCanonicalDockerEndpointOwnedElsewhereIsNeutral(t *testing.T) {
	check := canonicalDockerEndpointCheck(portodocker.Status{
		CanonicalPath: "/var/run/docker.sock",
		CanonicalLink: "/another/runtime/docker.sock",
	})
	if check.State != StateNeutral || check.Repair != nil {
		t.Fatalf("canonical endpoint check = %+v", check)
	}
}

func TestKubernetesChecksAreNeutralWithoutConfiguredClusters(t *testing.T) {
	collector := &Collector{
		Kubernetes: kubernetes.NewWithKubeconfigRoot(diagnosticRunner{}, t.TempDir()),
	}
	checks := collector.kubernetesChecks(
		context.Background(),
		app.Settings{KubernetesEnabled: true},
		true,
	)
	states := make(map[string]State)
	for _, check := range checks {
		states[check.ID] = check.State
	}
	if states["kubernetes-api"] != StateNeutral || states["kubeconfigs"] != StateNeutral {
		t.Fatalf("Kubernetes empty-state checks = %+v", checks)
	}
}

func TestKubernetesChecksSurfaceBrokenManagedKubeconfig(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "broken.yaml"), []byte("not: valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	collector := &Collector{
		Kubernetes: kubernetes.NewWithKubeconfigRoot(diagnosticRunner{output: []byte("not-json")}, root),
	}
	checks := collector.kubernetesChecks(
		context.Background(),
		app.Settings{KubernetesEnabled: true},
		true,
	)
	for _, check := range checks {
		if check.ID == "kubeconfigs" {
			if check.State != StateUnavailable || !strings.Contains(check.Detail, "decode managed Kubernetes context") {
				t.Fatalf("unexpected kubeconfig check: %+v", check)
			}
			return
		}
	}
	t.Fatalf("kubeconfig check missing: %+v", checks)
}

func TestCollectorReportsBrokenSettingsDatabaseWithoutStopping(t *testing.T) {
	expected := errors.New("synthetic database corruption")
	collector := &Collector{
		SettingsError:  expected,
		StateDirectory: t.TempDir(),
		Now:            func() time.Time { return time.Unix(1, 0) },
		DiskCapacity:   func(string) (uint64, uint64, error) { return 8 << 30, 16 << 30, nil },
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("not listening")
		},
		LookupHost: func(context.Context, string) ([]string, error) {
			return []string{"127.0.0.1"}, nil
		},
		Executable: func() (string, error) { return os.Executable() },
	}
	report := collector.Collect(context.Background())
	for _, check := range report.Checks {
		if check.ID == "settings" {
			if check.State != StateUnavailable || !strings.Contains(check.Detail, expected.Error()) {
				t.Fatalf("unexpected settings check: %+v", check)
			}
			return
		}
	}
	t.Fatal("settings database failure was not reported")
}
