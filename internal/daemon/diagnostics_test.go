package daemon

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mbianchidev/porto/internal/diagnostics"
)

func TestDiagnosticsReportWorksWithoutRuntimeManagers(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	response := httptest.NewRecorder()
	new(Server).diagnosticsReport(response, httptest.NewRequest(http.MethodGet, "/api/diagnostics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var report diagnostics.Report
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if len(report.Checks) == 0 || report.Version == "" {
		t.Fatalf("incomplete report: %+v", report)
	}
}

func TestDiagnosticsBundleIsLocalZipWithReport(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	response := httptest.NewRecorder()
	new(Server).diagnosticsBundle(response, httptest.NewRequest(http.MethodGet, "/api/diagnostics/bundle", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
	reader, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	for _, file := range reader.File {
		if file.Name == "report.json" {
			return
		}
	}
	t.Fatal("diagnostic bundle does not contain report.json")
}

func TestDiagnosticsRepairRequiresConfirmation(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/diagnostics/repair/restart-docker-runtime",
		bytes.NewBufferString(`{"confirm":false}`),
	)
	request.SetPathValue("action", "restart-docker-runtime")
	response := httptest.NewRecorder()
	new(Server).diagnosticsRepair(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}
