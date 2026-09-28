package diagnostics

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	portodocker "github.com/mbianchidev/porto/internal/docker"
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
	for _, check := range checks {
		if check.State != StateUnavailable {
			t.Fatalf("%s state = %q, want unavailable", check.ID, check.State)
		}
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

func TestDockerSocketCheckRejectsStaleRegularFile(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(socketPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	check := (&Collector{DockerSocket: socketPath}).dockerSocketCheck(portodocker.Status{Available: true})
	if check.State != StateUnsafe || !strings.Contains(check.Summary, "not a socket") {
		t.Fatalf("unexpected stale socket check: %+v", check)
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
