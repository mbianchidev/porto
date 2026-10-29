package imageanalysis

import (
	"strings"
	"testing"
)

func TestSyntheticPackagesAndAdvisories(t *testing.T) {
	sbom := []byte(`{"artifacts":[{"id":"pkg-1","name":"synthetic-lib","version":"1.0","type":"deb","purl":"pkg:deb/synthetic-lib@1.0","licenses":[{"value":"MIT"}],"locations":[{"path":"/usr/lib/synthetic","layerID":"sha256:layer"}]}],"distro":{"name":"synthetic","version":"1"}}`)
	grype := []byte(`{"matches":[{"artifact":{"id":"pkg-1","name":"synthetic-lib","version":"1.0"},"vulnerability":{"id":"CVE-2099-0001","severity":"High","namespace":"synthetic:1","dataSource":"https://example.invalid/advisory","fix":{"state":"fixed","versions":["1.1"]}},"matchDetails":[{"type":"exact-direct-match"}]}]}`)
	report := Report{}
	if err := decodeSBOM(sbom, &report); err != nil {
		t.Fatal(err)
	}
	if err := decodeFindings(grype, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Packages) != 1 || report.Packages[0].Licenses[0] != "MIT" || report.Packages[0].Layers[0] != "sha256:layer" {
		t.Fatalf("package attribution lost: %+v", report.Packages)
	}
	if len(report.Findings) != 1 || report.Findings[0].FixedVersions[0] != "1.1" || report.Findings[0].Severity != "High" {
		t.Fatalf("advisory lost: %+v", report.Findings)
	}
	if !strings.Contains(report.Findings[0].Remediation, "1.1") {
		t.Fatalf("missing evidence-based remediation: %+v", report.Findings)
	}
}

func TestAnalysisFailureCannotPassThreshold(t *testing.T) {
	for _, state := range []string{"queued", "running", "partial", "stale", "failed", "canceled"} {
		if code := (Report{Status: state}).ExitCode("critical"); code != 2 {
			t.Errorf("%s returned %d, want tool failure 2", state, code)
		}
	}
	report := Report{Status: "succeeded", Findings: []Finding{{Severity: "High"}}}
	if report.ExitCode("high") != 1 || report.ExitCode("critical") != 0 || report.ExitCode("invalid") != 2 {
		t.Fatal("threshold exit codes do not distinguish findings and analysis errors")
	}
}

func TestCacheIdentitySeparatesVariantsToolsDatabaseAndOptions(t *testing.T) {
	base := Report{Image: Image{Digest: "sha256:index", ManifestDigest: "sha256:manifest", Platform: "linux/amd64"}, SyftVersion: "1", GrypeVersion: "2", Database: Database{Checksum: "db-1", Built: "time-1", SchemaVersion: "v6"}}
	key := cacheKey(base)
	variants := []Report{base, base, base, base, base, base, base}
	variants[0].Image.Platform = "linux/arm64"
	variants[1].Image.ManifestDigest = "sha256:other"
	variants[2].Database.Checksum = "db-2"
	variants[3].GrypeVersion = "3"
	variants[4].SyftVersion = "4"
	variants[5].Options.AllowStale = true
	variants[6].Database.Built = "time-2"
	for _, variant := range variants {
		if cacheKey(variant) == key {
			t.Fatalf("cache mixed identities: %+v", variant)
		}
	}
}

func TestInvalidScannerDocumentsAreNotCleanReports(t *testing.T) {
	for _, document := range []string{`{}`, `null`, `{"artifacts":null}`, `{"matches":null}`} {
		if decodeSBOM([]byte(document), &Report{}) == nil || decodeFindings([]byte(document), &Report{}) == nil {
			t.Fatalf("accepted incomplete scanner document %s", document)
		}
	}
}

func TestDatabaseArchiveNamesAllowUpstreamTimestampsButNotOtherHostsOrTraversal(t *testing.T) {
	actual, err := databaseArchiveURL("vulnerability-db_v6.1.10_2026-10-29T00:24:34Z_1793255014.tar.zst")
	if err != nil || !strings.HasPrefix(actual, "https://grype.anchore.io/databases/v6/vulnerability-db_") {
		t.Fatalf("valid timestamped Grype archive rejected: %s %v", actual, err)
	}
	for _, invalid := range []string{"https://example.invalid/database.tar.zst", "../database.tar.zst", "%2e%2e/database.tar.zst", "database.tar.zst?token=synthetic", `dir\database.tar.zst`} {
		if _, err := databaseArchiveURL(invalid); err == nil {
			t.Fatalf("accepted unsafe archive URL %s", invalid)
		}
	}
}
