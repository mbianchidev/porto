package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestEngineResourceBudgetsRejectUnsupportedChangesBeforeMutation(t *testing.T) {
	for _, limits := range []EngineLimits{{CPUs: -1}, {CPUs: 129}, {MemoryMiB: 64}, {DiskGiB: -1}} {
		if err := limits.Validate(); err == nil {
			t.Fatalf("invalid budget was accepted: %+v", limits)
		}
	}
	if _, err := engineLimitsExpression(EngineConfiguration{VMType: "qemu", Disk: 40 << 30}, nil, EngineLimits{DiskGiB: 20}, "synthetic-owner"); err == nil {
		t.Fatal("disk shrink was accepted")
	}
	if _, err := engineLimitsExpression(EngineConfiguration{VMType: "synthetic-unsupported"}, nil, EngineLimits{CPUs: 2}, "synthetic-owner"); err == nil {
		t.Fatal("unsupported backend resource change was accepted")
	}
}

func TestEngineResourcePlanPreservesUnrelatedProvisionsAndDisks(t *testing.T) {
	swap := 512
	original := []byte("cpus: 4\nmounts:\n- location: /synthetic/project\nprovision:\n- mode: system\n  script: '# synthetic unrelated setup'\n- mode: system\n  script: \"# porto-engine-swap\\nold owned swap\"\n")
	expression, err := engineLimitsExpression(EngineConfiguration{VMType: "vz", Disk: 20 << 30}, original,
		EngineLimits{CPUs: 2, MemoryMiB: 2048, SwapMiB: &swap, DiskGiB: 40}, "synthetic-owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{".cpus = 2", `.memory = "2048MiB"`, `.disk = "40GiB"`} {
		if !strings.Contains(expression, required) {
			t.Fatalf("resource plan omitted %q: %s", required, expression)
		}
	}
	_, remainder, ok := strings.Cut(expression, `.provision = ("`)
	if !ok {
		t.Fatal("bounded provision replacement was not encoded")
	}
	encoded, _, _ := strings.Cut(remainder, `" |`)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var provisions []struct{ Mode, Script string }
	if err := json.Unmarshal(data, &provisions); err != nil {
		t.Fatal(err)
	}
	if len(provisions) != 2 || provisions[0].Script != "# synthetic unrelated setup" ||
		!strings.Contains(provisions[1].Script, "size=512") || !strings.Contains(provisions[1].Script, "synthetic-owner") {
		t.Fatalf("unrelated settings were replaced: %+v", provisions)
	}
	if strings.Contains(expression, ".mounts") {
		t.Fatal("resource changes rewrote host shares")
	}
}

func TestNewEngineSwapBudgetAppendsWithoutReplacingTemplateSetup(t *testing.T) {
	swap := 0
	expression, err := engineLimitsExpression(EngineConfiguration{VMType: "qemu"}, nil, EngineLimits{SwapMiB: &swap}, "synthetic-owner")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(expression, ".provision = (.provision // []) + ") {
		t.Fatal("new engine resource setup would replace inherited template provisions")
	}
}

func TestEngineConfigurationStopsFailedBootBeforeRestoringOriginalConfiguration(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, engineInstanceName)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "lima.yaml")
	const original = "cpus: 4\nmemory: 4GiB\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	stops, starts := 0, 0
	bootFailure := errors.New("synthetic VM started but guest readiness failed")
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case command.Name == "nerdctl":
			return []byte("{}"), nil
		case joined == "list porto-engine --json":
			return json.Marshal(struct {
				Name string `json:"name"`
				EngineConfiguration
			}{Name: engineInstanceName, EngineConfiguration: EngineConfiguration{Directory: directory, Status: "Running", VMType: "vz"}})
		case strings.Contains(joined, `.porto-engine-owner`):
			return []byte("synthetic-owner\n"), nil
		case joined == "stop porto-engine":
			stops++
			return nil, nil
		case strings.HasPrefix(joined, "edit --tty=false --set="):
			return nil, os.WriteFile(path, []byte("cpus: 2\nmemory: 2GiB\n"), 0o600)
		case strings.HasPrefix(joined, "start --timeout="):
			starts++
			if starts == 1 {
				return nil, bootFailure
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != original || stops != 2 {
				return nil, errors.New("rollback restarted without stopping the failed VM and restoring its original YAML")
			}
			return nil, nil
		}
		return nil, errors.New("unexpected synthetic engine command: " + command.Name + " " + joined)
	}}
	manager := NewWithStateDir(runner, root)
	manager.lookPath = func(name string) (string, error) { return name, nil }
	manager.dialBuildKit = workingBuildKitDialer(t)
	backend := newSnapshotterTestRuntime(t, "", "overlayfs", &snapshotterTestServer{snapshotter: "overlayfs"})
	manager.runtimeConnector = func(context.Context) (containerRuntime, error) { return backend, nil }
	if err := manager.writeEngineState(engineState{Mode: "lima", Instance: engineInstanceName, OwnerID: "synthetic-owner"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := manager.editEngineConfiguration(ctx, true,
		func(EngineConfiguration, []byte) (string, error) { return ".cpus = 2", nil }, nil, nil)
	if !errors.Is(err, bootFailure) || starts != 2 || stops != 2 {
		t.Fatalf("failed boot recovery sequence: starts=%d stops=%d error=%v", starts, stops, err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != original {
		t.Fatalf("failed boot lost the original configuration: %s, %v", data, readErr)
	}
}
