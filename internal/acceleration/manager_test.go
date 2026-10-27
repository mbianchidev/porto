package acceleration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type probeRunner struct {
	run func(runtimes.Command) ([]byte, error)
}

func (r probeRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	return r.run(command)
}

func TestArchitectureSettingsPersistWithoutInstallingHostTools(t *testing.T) {
	root := t.TempDir()
	manager := New(root, probeRunner{run: func(command runtimes.Command) ([]byte, error) {
		t.Fatalf("saving a non-Rosetta policy ran a host command: %+v", command)
		return nil, nil
	}})
	settings := Settings{Policy: NativeOnly}
	if err := manager.Save(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	restored, err := New(root, nil).Settings()
	if err != nil || restored != settings {
		t.Fatalf("policy after restart = %+v, %v", restored, err)
	}
	manager.native = func() (Capability, Capability) {
		return Capability{}, Capability{Reason: "synthetic unavailable framework"}
	}
	manager.lookPath = func(string) (string, error) { return "", errors.New("synthetic missing tool") }
	if err := manager.Save(context.Background(), Settings{Policy: RosettaPreferred, RosettaEnabled: true}); err == nil {
		t.Fatal("unsupported Rosetta settings were persisted")
	}
	unchanged, err := manager.Settings()
	if err != nil || unchanged != settings {
		t.Fatalf("rejected configuration replaced the original: %+v, %v", unchanged, err)
	}
}

func TestCapabilitiesRefreshAfterProviderChanges(t *testing.T) {
	version, rosetta := "synthetic-lima-1", false
	manager := New(t.TempDir(), probeRunner{run: func(command runtimes.Command) ([]byte, error) {
		if command.Name == "limactl" {
			return []byte(`{"version":"` + version + `","vmTypes":["vz","qemu"]}`), nil
		}
		return []byte("synthetic QEMU"), nil
	}})
	manager.lookPath = func(name string) (string, error) { return name, nil }
	manager.native = func() (Capability, Capability) {
		return Capability{Supported: true}, Capability{Supported: rosetta}
	}
	first := manager.ProbeHost(context.Background())
	version, rosetta = "synthetic-lima-2", true
	second := manager.ProbeHost(context.Background())
	if first.LimaVersion == second.LimaVersion || first.Rosetta.Supported || !second.Rosetta.Supported {
		t.Fatalf("provider changes stayed cached: first=%+v second=%+v", first, second)
	}
}

func TestCorruptArchitectureSettingsFailExplicitly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "acceleration.json"), []byte(`{"policy":"synthetic-invalid"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root, nil).Settings(); err == nil {
		t.Fatal("corrupt policy silently fell back to automatic")
	}
}
