package diagnostics

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestBuildBundleRedactsSourcesAndKeepsPartialFailures(t *testing.T) {
	report := NewReport("1.2.12", time.Unix(1, 0).UTC(), []Check{
		{ID: "daemon", State: StateHealthy, Summary: "reachable", Detail: "token=report-secret"},
	})
	var output bytes.Buffer
	preview, err := BuildBundle(&output, report, []BundleSource{
		{
			Name:        "config/settings.json",
			Description: "Sanitized settings",
			Read: func() ([]byte, error) {
				return []byte(`{"password":"must-not-leak","enabled":true}`), nil
			},
		},
		{
			Name:        "logs/porto.log",
			Description: "Recent logs",
			Read: func() ([]byte, error) {
				return nil, errors.New("synthetic log failure")
			},
		},
	}, RedactionContext{})
	if err != nil {
		t.Fatalf("build bundle: %v", err)
	}
	if len(preview.Warnings) != 1 || !strings.Contains(preview.Warnings[0], "synthetic log failure") {
		t.Fatalf("unexpected preview warnings: %+v", preview.Warnings)
	}

	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	contents := map[string]string{}
	for _, file := range reader.File {
		stream, err := file.Open()
		if err != nil {
			t.Fatalf("open %s: %v", file.Name, err)
		}
		data, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil {
			t.Fatalf("read %s: %v", file.Name, err)
		}
		contents[file.Name] = string(data)
	}
	if !strings.Contains(contents["report.json"], `"overall": "healthy"`) {
		t.Fatalf("report missing from bundle: %q", contents["report.json"])
	}
	if strings.Contains(contents["report.json"], "report-secret") {
		t.Fatalf("report diagnostics were not redacted: %q", contents["report.json"])
	}
	if strings.Contains(contents["config/settings.json"], "must-not-leak") ||
		!strings.Contains(contents["config/settings.json"], "[REDACTED]") {
		t.Fatalf("settings were not redacted: %q", contents["config/settings.json"])
	}
	if !strings.Contains(contents["bundle-warnings.txt"], "synthetic log failure") {
		t.Fatalf("partial failure warning missing: %+v", contents)
	}
}
