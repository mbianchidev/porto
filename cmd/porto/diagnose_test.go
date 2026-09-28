package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/diagnostics"
)

func TestWriteDiagnosticReportIncludesScopedRepairCommand(t *testing.T) {
	report := diagnostics.NewReport("1.2.12", time.Unix(1, 0), []diagnostics.Check{{
		ID: "addons", Category: "kubernetes", Name: "Managed add-ons",
		State: diagnostics.StateDegraded, Summary: "repair required",
		Repair: &diagnostics.Repair{
			ID: "repair-kubernetes-addons", Target: "dev", Label: "Repair",
		},
	}})
	var output bytes.Buffer
	writeDiagnosticReport(&output, report)
	if !strings.Contains(output.String(), "porto diagnose repair repair-kubernetes-addons --target dev --confirm") {
		t.Fatalf("repair command missing:\n%s", output.String())
	}
}

func TestDiagnosticStatusErrorPreservesExitCode(t *testing.T) {
	err := diagnosticStatusError{state: diagnostics.StateUnsafe, code: 2}
	if err.ExitCode() != 2 || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unexpected diagnostic error: %v, code %d", err, err.ExitCode())
	}
}
