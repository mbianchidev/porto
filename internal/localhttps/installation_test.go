package localhttps

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/certificates"
)

func TestInstallationRollsBackNewHelperAndTrust(t *testing.T) {
	denied := errors.New("certificate store denied access")
	var calls []string
	steps := installSteps{
		install: func() error { calls = append(calls, "install"); return nil },
		remove:  func() error { calls = append(calls, "remove"); return nil },
		trust:   func() error { calls = append(calls, "trust"); return denied },
		untrust: func() error { calls = append(calls, "untrust"); return nil },
		verify:  func() bool { return false },
		mark:    func() error { calls = append(calls, "mark"); return nil },
	}

	if err := installWithRollback(steps); !errors.Is(err, denied) {
		t.Fatalf("install error = %v", err)
	}
	if want := []string{"install", "trust", "untrust", "remove"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestSystemdInstallationRollsBackPartialActivation(t *testing.T) {
	root := t.TempDir()
	certificate := filepath.Join(root, "leaf.pem")
	authority := filepath.Join(root, "ca.pem")
	if _, err := certificates.New(certificate, certificate+".key", authority, authority+".key").Ensure(); err != nil {
		t.Fatal(err)
	}
	units, trust := filepath.Join(root, "units"), filepath.Join(root, "trust")
	for _, directory := range []string{units, trust} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	activation := errors.New("port 443 is occupied")
	var calls []string
	integration := systemdIntegration{
		unitDirectory: units, trustDirectory: trust,
		proxyExecutable: "/usr/lib/systemd/systemd-socket-proxyd",
		refreshTrust:    []string{"update-ca-certificates"},
		run: func(name string, args ...string) error {
			calls = append(calls, name+" "+strings.Join(args, " "))
			if name == "systemctl" && len(args) > 0 && args[0] == "enable" {
				return activation
			}
			return nil
		},
	}
	if err := integration.install(authority); !errors.Is(err, activation) {
		t.Fatalf("installation error = %v", err)
	}
	for _, directory := range []string{units, trust} {
		entries, err := os.ReadDir(directory)
		if err != nil || len(entries) != 0 {
			t.Fatalf("partial installation remains in %s: %v, %v", directory, entries, err)
		}
	}
	if !strings.Contains(strings.Join(calls, "\n"), "systemctl stop porto-https.socket porto-https.service") {
		t.Fatalf("partial listeners were not stopped: %v", calls)
	}
}
