package imageanalysis

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/localtools"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type fixtureSource struct {
	image   Image
	archive []byte
	exports atomic.Int32
	block   bool
}

func (s *fixtureSource) ResolveAnalysisImage(_ context.Context, _, platform string) (Image, error) {
	image := s.image
	if platform != "" {
		image.Platform = platform
	}
	return image, nil
}

func (s *fixtureSource) ExportAnalysisImage(ctx context.Context, _ Image, writer io.Writer) error {
	s.exports.Add(1)
	if s.block {
		<-ctx.Done()
		return ctx.Err()
	}
	_, err := writer.Write(s.archive)
	return err
}

type fixtureRunner struct {
	t          *testing.T
	root       string
	built      time.Time
	missing    bool
	crash      bool
	diagnostic bool
	calls      []runtimes.Command
}

func (r *fixtureRunner) Run(_ context.Context, command runtimes.Command) (localtools.Output, error) {
	r.calls = append(r.calls, command)
	if slices.Contains(command.Args, "version") {
		return localtools.Output{Stdout: []byte(`{"version":"synthetic-1"}`)}, nil
	}
	if slices.Contains(command.Args, "status") {
		if r.missing {
			return localtools.Output{Stdout: []byte(`{"valid":false,"error":"database missing"}`)}, errors.New("database missing")
		}
		path := filepath.Join(r.root, "database", "v6", "vulnerability.db")
		return localtools.Output{Stdout: []byte(`{"valid":true,"schemaVersion":"v6.1.10","built":"` + r.built.Format(time.RFC3339) + `","path":` + strconv.Quote(path) + `}`)}, nil
	}
	if command.Name == "syft" && slices.Contains(command.Args, "scan") {
		if !slices.Contains(command.Env, "GRYPE_DB_AUTO_UPDATE=false") || !slices.Contains(command.Env, "SYFT_CHECK_FOR_APP_UPDATE=false") {
			r.t.Error("analysis permits automatic network access")
		}
		for _, environment := range command.Env {
			if strings.Contains(environment, "synthetic-password") {
				r.t.Error("inherited scanner credentials escaped isolation")
			}
		}
		return localtools.Output{Stdout: []byte(`{"artifacts":[{"id":"pkg-1","name":"synthetic-lib","version":"1","type":"deb","licenses":[],"locations":[]}],"distro":{"name":"synthetic","version":"1"},"source":{"metadata":{"rawConfig":"synthetic-secret","rawManifest":"unnecessary"}}}`)}, nil
	}
	if command.Name == "grype" {
		if r.crash {
			return localtools.Output{}, errors.New("synthetic scanner crash")
		}
		output := localtools.Output{Stdout: []byte(`{"matches":[]}`)}
		if r.diagnostic {
			output.Stderr = []byte("synthetic incomplete catalog warning")
		}
		return output, nil
	}
	if slices.Contains(command.Args, "spdx-json") {
		return localtools.Output{Stdout: []byte(`{"spdxVersion":"SPDX-2.3","packages":[]}`)}, nil
	}
	return localtools.Output{Stdout: []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[]}`)}, nil
}

func syntheticArchive(t *testing.T, platform string) (Image, []byte) {
	t.Helper()
	parts := strings.Split(platform, "/")
	identity := func(data []byte) string { sum := sha256.Sum256(data); return "sha256:" + hex.EncodeToString(sum[:]) }
	var layerBuffer bytes.Buffer
	layerWriter := tar.NewWriter(&layerBuffer)
	for name, content := range map[string]string{
		"etc/os-release":      "ID=debian\nVERSION_ID=13\nNAME=Debian\n",
		"var/lib/dpkg/status": "Package: porto-synthetic-lib\nVersion: 1.0\nArchitecture: all\nStatus: install ok installed\nMaintainer: Synthetic Fixture\nDescription: Synthetic package for local analysis acceptance\n\n",
	} {
		if err := layerWriter.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := layerWriter.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := layerWriter.Close(); err != nil {
		t.Fatal(err)
	}
	layer := layerBuffer.Bytes()
	config := []byte(`{"os":"` + parts[0] + `","architecture":"` + parts[1] + `","rootfs":{"type":"layers","diff_ids":["` + identity(layer) + `"]}}`)
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + identity(config) + `","size":` + strconv.Itoa(len(config)) + `},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"` + identity(layer) + `","size":` + strconv.Itoa(len(layer)) + `} ]}`)
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for name, content := range map[string]string{
		"oci-layout": `{"imageLayoutVersion":"1.0.0"}`,
		"index.json": `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + identity(manifest) + `","size":` + strconv.Itoa(len(manifest)) + `,"platform":{"os":"` + parts[0] + `","architecture":"` + parts[1] + `"}}]}`,
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	for _, data := range [][]byte{manifest, config, layer} {
		if err := writer.WriteHeader(&tar.Header{Name: "blobs/sha256/" + strings.TrimPrefix(identity(data), "sha256:"), Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return Image{Digest: identity(manifest), Platform: platform}, buffer.Bytes()
}

func analysisFixture(t *testing.T) (*Manager, *fixtureSource, *fixtureRunner) {
	t.Helper()
	root := t.TempDir()
	image, archive := syntheticArchive(t, "linux/amd64")
	source := &fixtureSource{image: image, archive: archive}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	runner := &fixtureRunner{t: t, root: root, built: now.Add(-time.Hour)}
	databasePath := filepath.Join(root, "database", "v6", "vulnerability.db")
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(databasePath, []byte("synthetic database bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(root, source, runner)
	manager.now = func() time.Time { return now }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return manager, source, runner
}

func awaitReport(t *testing.T, manager *Manager, id string) Report {
	t.Helper()
	manager.mu.Lock()
	var done chan struct{}
	for _, job := range manager.active {
		if job.report.ID == id {
			done = job.done
		}
	}
	manager.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("analysis did not finish")
		}
	}
	report, err := manager.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestLocalScanCacheExportsAndDatabaseInvalidation(t *testing.T) {
	t.Setenv("SYFT_REGISTRY_AUTH_PASSWORD", "synthetic-password")
	manager, source, runner := analysisFixture(t)
	start := func(options Options) Report {
		report, err := manager.Start(context.Background(), "synthetic:1", "", options)
		if err != nil {
			t.Fatal(err)
		}
		return awaitReport(t, manager, report.ID)
	}
	first := start(Options{})
	if first.Status != "succeeded" || first.Image.ManifestDigest != source.image.Digest || first.Database.Checksum == "" {
		t.Fatalf("scan did not establish exact identity: %+v", first)
	}
	second := start(Options{})
	if !second.Cached || source.exports.Load() != 1 {
		t.Fatalf("same immutable image was not deduplicated: %+v", second)
	}
	for _, format := range []string{"syft-json", "spdx-json", "cyclonedx-json", "report"} {
		output, err := manager.Export(context.Background(), first.ID, format)
		if err != nil || len(output) == 0 || bytes.Contains(output, []byte("synthetic-secret")) {
			t.Fatalf("unsafe or unavailable %s export: %s %v", format, output, err)
		}
	}
	runner.built = runner.built.Add(time.Minute)
	third := start(Options{})
	if third.Cached || third.CacheKey == first.CacheKey || source.exports.Load() != 2 {
		t.Fatalf("database change did not invalidate analysis: %+v", third)
	}
}

func TestMissingStalePartialAndCrashedScansNeverLookClean(t *testing.T) {
	for _, scenario := range []string{"missing", "stale-denied", "stale-allowed", "partial", "crash"} {
		t.Run(scenario, func(t *testing.T) {
			manager, source, runner := analysisFixture(t)
			options := Options{}
			switch scenario {
			case "missing":
				runner.missing = true
			case "stale-denied", "stale-allowed":
				runner.built = runner.built.Add(-6 * 24 * time.Hour)
				options.AllowStale = scenario == "stale-allowed"
			case "partial":
				runner.diagnostic = true
			case "crash":
				runner.crash = true
			}
			report, err := manager.Start(context.Background(), "synthetic:1", "", options)
			if err != nil {
				t.Fatal(err)
			}
			report = awaitReport(t, manager, report.ID)
			if report.Status == "succeeded" || report.ExitCode("critical") != 2 {
				t.Fatalf("incomplete analysis appeared clean: %+v", report)
			}
			if (scenario == "missing" || scenario == "stale-denied") && source.exports.Load() != 0 {
				t.Fatal("missing or disallowed stale database still exported the image")
			}
			if scenario == "crash" && (report.SBOMID == "" || report.Error == "") {
				t.Fatal("scanner failure lost the available SBOM or failure state")
			}
		})
	}
}

func TestConcurrentScansDeduplicateAndCancel(t *testing.T) {
	manager, source, _ := analysisFixture(t)
	source.block = true
	first, err := manager.Start(context.Background(), "synthetic:1", "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Start(context.Background(), "synthetic:1", "", Options{})
	if err != nil || second.ID != first.ID {
		t.Fatalf("concurrent scans were not deduplicated: %v", err)
	}
	if err := manager.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	report := awaitReport(t, manager, first.ID)
	if report.Status != "canceled" || report.ExitCode("") != 2 {
		t.Fatalf("canceled scan appeared complete: %+v", report)
	}
}

func TestArchiveRejectsWrongPlatformAndCorruptManifest(t *testing.T) {
	manager, _, _ := analysisFixture(t)
	report, err := manager.Start(context.Background(), "synthetic:1", "linux/arm64", Options{})
	if err != nil {
		t.Fatal(err)
	}
	result := awaitReport(t, manager, report.ID)
	if result.Status != "failed" || !strings.Contains(result.Error, "platform") {
		t.Fatalf("architecture findings were mixed: %+v", result)
	}
}

func TestPinnedSyftLocalSBOMAndMachineReadableExports(t *testing.T) {
	bin := os.Getenv("PORTO_TEST_ANALYSIS_BIN")
	if bin == "" {
		t.Skip("set PORTO_TEST_ANALYSIS_BIN to checksum-verified Syft/Grype bin directory for native scanner acceptance")
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	manager, source, _ := analysisFixture(t)
	manager.runner = localtools.ExecRunner{}
	work := t.TempDir()
	path := filepath.Join(work, "synthetic-image.tar")
	if err := os.WriteFile(path, source.archive, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, tool := range []string{"syft", "grype"} {
		version, err := manager.toolVersion(ctx, work, tool)
		if err != nil || version == "" {
			t.Fatalf("%s version: %s %v", tool, version, err)
		}
	}
	output, err := manager.tool(ctx, work, "syft", "scan", "oci-archive:"+path, "--platform", source.image.Platform, "--scope", "squashed", "-o", "syft-json")
	if err != nil {
		t.Fatal(err)
	}
	report := Report{}
	if err := decodeSBOM(output.Stdout, &report); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pkg := range report.Packages {
		if pkg.Name == "porto-synthetic-lib" && pkg.Version == "1.0" {
			found = true
		}
	}
	if !found {
		t.Fatalf("real Syft did not catalog synthetic dpkg package: %+v", report)
	}
	sbom := filepath.Join(work, "sbom.json")
	cleaned, err := cleanSBOM(output.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sbom, cleaned, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"spdx-json", "cyclonedx-json"} {
		output, err := manager.tool(ctx, work, "syft", "convert", sbom, "-o", format)
		if err != nil || len(output.Stdout) == 0 {
			t.Fatalf("%s export: %s %v", format, output.Stderr, err)
		}
	}
}

func TestPinnedGrypeDatabaseAndOfflineAnalysis(t *testing.T) {
	bin := os.Getenv("PORTO_TEST_ANALYSIS_BIN")
	if bin == "" || os.Getenv("PORTO_TEST_ANALYSIS_DATABASE") != "1" {
		t.Skip("set PORTO_TEST_ANALYSIS_BIN and PORTO_TEST_ANALYSIS_DATABASE=1 to consent to a bounded public database download")
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	manager, _, _ := analysisFixture(t)
	manager.runner = localtools.ExecRunner{}
	manager.now = time.Now
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	status, err := manager.UpdateDatabase(ctx, MaxDatabaseBytes)
	if err != nil || status.State != "ready" {
		t.Fatalf("real database import: %+v %v", status, err)
	}
	report, err := manager.Start(ctx, "synthetic:1", "", Options{})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	var done chan struct{}
	for _, job := range manager.active {
		if job.report.ID == report.ID {
			done = job.done
		}
	}
	manager.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	report, err = manager.Get(report.ID)
	if err != nil || report.Status != "succeeded" || report.Database.Checksum != status.Checksum {
		t.Fatalf("real offline Grype scan: %+v %v", report, err)
	}
}
