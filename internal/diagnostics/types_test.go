package diagnostics

import (
	"testing"
	"time"
)

func TestNewReportUsesHighestSeverity(t *testing.T) {
	report := NewReport("1.2.12", time.Unix(1, 0).UTC(), []Check{
		{ID: "healthy", State: StateHealthy},
		{ID: "degraded", State: StateDegraded},
		{ID: "unavailable", State: StateUnavailable},
		{ID: "unsafe", State: StateUnsafe},
	})

	if report.Overall != StateUnsafe {
		t.Fatalf("overall = %q, want %q", report.Overall, StateUnsafe)
	}
	if report.Summary.Healthy != 1 || report.Summary.Degraded != 1 ||
		report.Summary.Unavailable != 1 || report.Summary.Unsafe != 1 {
		t.Fatalf("unexpected summary: %+v", report.Summary)
	}
	if report.ExitCode() != 2 {
		t.Fatalf("exit code = %d, want 2", report.ExitCode())
	}
}

func TestDegradedReportStillReturnsSuccess(t *testing.T) {
	report := NewReport("1.2.12", time.Now(), []Check{{ID: "warning", State: StateDegraded}})
	if report.ExitCode() != 0 {
		t.Fatalf("exit code = %d, want 0", report.ExitCode())
	}
}
