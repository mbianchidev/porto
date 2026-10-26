package docker

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
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
