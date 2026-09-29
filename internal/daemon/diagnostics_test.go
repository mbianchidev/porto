package daemon

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/diagnostics"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type diagnosticsRepairRunner struct {
	endpoint string
}

func (r diagnosticsRepairRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	switch strings.Join(command.Args, " ") {
	case "context inspect porto":
		return []byte(`[{"Name":"porto","Endpoints":{"docker":{"Host":"` + r.endpoint + `"}}}]`), nil
	case "context update porto --docker host=" + r.endpoint:
		return nil, nil
	default:
		return nil, nil
	}
}

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

func TestDiagnosticsRepairReturnsFreshReport(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	socketPath := "/tmp/porto.sock"
	server := &Server{
		docker:       portodocker.New(diagnosticsRepairRunner{endpoint: portodocker.EndpointURL(socketPath)}),
		dockerSocket: socketPath,
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/diagnostics/repair/reinstall-docker-context",
		bytes.NewBufferString(`{"confirm":true}`),
	)
	request.SetPathValue("action", "reinstall-docker-context")
	response := httptest.NewRecorder()

	server.diagnosticsRepair(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Report diagnostics.Report `json:"report"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode repair response: %v", err)
	}
	if result.Report.GeneratedAt.IsZero() || len(result.Report.Checks) == 0 {
		t.Fatalf("repair response did not contain a fresh report: %+v", result.Report)
	}
}
