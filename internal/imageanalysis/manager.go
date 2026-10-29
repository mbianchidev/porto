package imageanalysis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/localtools"
	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type Manager struct {
	root             string
	source           Source
	runner           localtools.Runner
	mu               sync.Mutex
	active           map[string]*scan
	slots            chan struct{}
	databaseMu       sync.RWMutex
	databaseUpdateMu sync.Mutex
	now              func() time.Time
}

type scan struct {
	report Report
	cancel context.CancelFunc
	done   chan struct{}
}

func New(root string, source Source, runner localtools.Runner) *Manager {
	if runner == nil {
		runner = localtools.ExecRunner{}
	}
	return &Manager{root: root, source: source, runner: runner, active: map[string]*scan{}, slots: make(chan struct{}, 2), now: time.Now}
}

func (m *Manager) Start(ctx context.Context, reference, platform string, options Options) (Report, error) {
	if strings.TrimSpace(reference) == "" || len(reference) > 512 {
		return Report{}, fmt.Errorf("%w: a local image reference is required", ErrInvalid)
	}
	if options.MaxAgeHours == 0 {
		options.MaxAgeHours = int(DefaultMaxAge.Hours())
	}
	if options.MaxAgeHours < 1 || options.MaxAgeHours > 8760 {
		return Report{}, fmt.Errorf("%w: maximum database age must be 1-8760 hours", ErrInvalid)
	}
	image, err := m.source.ResolveAnalysisImage(ctx, reference, platform)
	if err != nil {
		return Report{}, err
	}
	key := hash(struct {
		Image       Image
		AllowStale  bool
		MaxAgeHours int
	}{image, options.AllowStale, options.MaxAgeHours})
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.active[key]; existing != nil {
		return cloneReport(existing.report), nil
	}
	if len(m.active) >= 16 {
		return Report{}, ErrBusy
	}
	if err := m.checkCacheBudget(); err != nil {
		return Report{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		cancel()
		return Report{}, err
	}
	report := Report{ID: hex.EncodeToString(id), Image: image, Options: options, Status: "queued", Phase: "waiting", StartedAt: m.now().UTC().Format(time.RFC3339Nano), BaseImage: "Unknown: image ancestry cannot be inferred reliably from layers."}
	if err := m.save(report); err != nil {
		cancel()
		return Report{}, err
	}
	job := &scan{report: report, cancel: cancel, done: make(chan struct{})}
	m.active[key] = job
	go m.execute(ctx, key, job)
	return report, nil
}

func cloneReport(report Report) Report {
	data, _ := json.Marshal(report)
	var result Report
	_ = json.Unmarshal(data, &result)
	return result
}

func (m *Manager) execute(ctx context.Context, key string, job *scan) {
	defer job.cancel()
	report := cloneReport(job.report)
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	case <-ctx.Done():
		m.finish(key, job, report, ctx.Err())
		return
	}
	phase := func(value string) {
		report.Status, report.Phase = "running", value
		m.mu.Lock()
		job.report = cloneReport(report)
		m.mu.Unlock()
	}
	err := m.analyze(ctx, &report, phase)
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	m.finish(key, job, report, err)
}

func (m *Manager) finish(key string, job *scan, report Report, err error) {
	report.CompletedAt = m.now().UTC().Format(time.RFC3339Nano)
	report.Phase = "complete"
	if err != nil {
		if report.Status != "partial" {
			report.Status = "failed"
		}
		if errors.Is(err, context.Canceled) {
			report.Status = "canceled"
		}
		report.Error = networking.Redact(err.Error())
		slog.Warn("Image analysis did not complete", "id", report.ID, "status", report.Status, "error", report.Error)
	}
	if saveErr := m.save(report); saveErr != nil {
		report.Status, report.Error = "failed", "Unable to persist analysis: "+saveErr.Error()
		slog.Error("Unable to persist image analysis", "id", report.ID, "error", saveErr)
	}
	m.mu.Lock()
	job.report = cloneReport(report)
	delete(m.active, key)
	close(job.done)
	m.mu.Unlock()
}

func (m *Manager) analyze(ctx context.Context, report *Report, phase func(string)) error {
	phase("checking tools and local database")
	work, err := os.MkdirTemp(m.root, ".scan-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(work); err != nil {
			slog.Warn("Unable to remove temporary analysis", "error", err)
		}
	}()
	syft, err := m.toolVersion(ctx, work, "syft")
	if err != nil {
		return err
	}
	grype, err := m.toolVersion(ctx, work, "grype")
	if err != nil {
		return err
	}
	report.SyftVersion, report.GrypeVersion = syft, grype
	m.databaseMu.RLock()
	defer m.databaseMu.RUnlock()
	report.Database, err = m.databaseStatus(ctx)
	if err != nil {
		return err
	}
	report.Database.MaxAgeHours = report.Options.MaxAgeHours
	if report.Database.State == "ready" || report.Database.State == "stale" {
		built, err := time.Parse(time.RFC3339, report.Database.Built)
		if err != nil {
			return err
		}
		report.Database.State, report.Database.Message = "ready", ""
		if m.now().Sub(built) > time.Duration(report.Options.MaxAgeHours)*time.Hour {
			report.Database.State, report.Database.Message = "stale", "Database exceeds the selected maximum age; update it or explicitly allow stale matching."
		}
	}
	if report.Database.State != "ready" && !(report.Database.State == "stale" && report.Options.AllowStale) {
		return fmt.Errorf("Grype database is %s: %s", report.Database.State, report.Database.Message)
	}
	if !report.Options.Force {
		cached, err := m.cached(*report)
		if err != nil {
			return err
		}
		if cached != nil {
			id, started := report.ID, report.StartedAt
			*report = *cached
			report.ID, report.StartedAt, report.Cached = id, started, true
			return nil
		}
	}
	phase("exporting and verifying the selected local platform")
	archivePath := filepath.Join(work, "image.tar")
	archive, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer archive.Close()
	if err := m.source.ExportAnalysisImage(ctx, report.Image, archive); err != nil {
		return err
	}
	report.Image.ManifestDigest, report.Layers, err = archiveManifest(archive, report.Image)
	if err != nil {
		return err
	}
	report.CacheKey = cacheKey(*report)
	phase("generating local SBOM")
	sbom, err := m.tool(ctx, work, "syft", "scan", "oci-archive:"+archivePath, "--platform", report.Image.Platform, "--scope", "squashed", "-o", "syft-json")
	if err != nil {
		return err
	}
	if err := decodeSBOM(sbom.Stdout, report); err != nil {
		return err
	}
	if len(sbom.Stderr) > 0 {
		report.Warnings = append(report.Warnings, "Syft emitted diagnostics: "+networking.Redact(strings.TrimSpace(string(sbom.Stderr))))
	}
	cleaned, err := cleanSBOM(sbom.Stdout)
	if err != nil {
		return err
	}
	report.SBOMID = hash(struct {
		Image Image
		Syft  string
	}{report.Image, syft})
	sbomPath := filepath.Join(m.root, "sboms", report.SBOMID+".json")
	if err := config.WriteFileAtomic(sbomPath, cleaned, 0o600); err != nil {
		return err
	}
	phase("matching packages with the local Grype database")
	findings, err := m.tool(ctx, work, "grype", "sbom:"+sbomPath, "-o", "json")
	if err != nil {
		report.Status = "partial"
		report.Warnings = append(report.Warnings, "SBOM is available, but vulnerability matching failed.")
		return err
	}
	if err := decodeFindings(findings.Stdout, report); err != nil {
		return err
	}
	if len(findings.Stderr) > 0 {
		report.Warnings = append(report.Warnings, "Grype emitted diagnostics: "+networking.Redact(strings.TrimSpace(string(findings.Stderr))))
	}
	report.Status = "succeeded"
	if len(report.Warnings) > 0 {
		report.Status = "partial"
	}
	if report.Database.State == "stale" {
		report.Status = "stale"
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if err := boundedReport(data); err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(m.root, "cache", lookupKey(*report)+".json"), data, 0o600)
}

func (m *Manager) toolVersion(ctx context.Context, work, tool string) (string, error) {
	output, err := m.tool(ctx, work, tool, "version", "-o", "json")
	if err != nil {
		return "", err
	}
	var version struct{ Version string }
	if json.Unmarshal(output.Stdout, &version) != nil || version.Version == "" {
		return "", fmt.Errorf("%s returned invalid version metadata", tool)
	}
	return version.Version, nil
}

func (m *Manager) tool(ctx context.Context, work, tool string, args ...string) (localtools.Output, error) {
	configPath := filepath.Join(work, tool+".yaml")
	if err := config.WriteFileAtomic(configPath, []byte("check-for-app-update: false\n"), 0o600); err != nil {
		return localtools.Output{}, err
	}
	output, err := m.runner.Run(ctx, runtimes.Command{
		Name: tool, Dir: work, Args: append([]string{"--config", configPath}, args...),
		Env: localtools.IsolatedEnvironment(work, "SYFT_CHECK_FOR_APP_UPDATE=false", "SYFT_PACKAGE_SEARCH_UNINDEXED_ARCHIVES=false", "SYFT_PACKAGE_SEARCH_INDEXED_ARCHIVES=true", "GRYPE_CHECK_FOR_APP_UPDATE=false", "GRYPE_DB_AUTO_UPDATE=false", "GRYPE_DB_REQUIRE_UPDATE_CHECK=false", "GRYPE_DB_VALIDATE_AGE=false", "GRYPE_DB_VALIDATE_BY_HASH_ON_START=true", "GRYPE_DB_CACHE_DIR="+filepath.Join(m.root, "database"), "GOMAXPROCS=2", "GOMEMLIMIT=1GiB", "GRYPE_EXTERNAL_SOURCES_ENABLE=false", "SYFT_PARALLELISM=2"),
	})
	if err != nil {
		return output, runtimes.CommandError(tool+" analysis tool failed", output.Stderr, err)
	}
	return output, nil
}

func validID(id string) bool {
	if len(id) != 32 && len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (m *Manager) save(report Report) error {
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if err := boundedReport(data); err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(m.root, "reports", report.ID+".json"), data, 0o600)
}

func (m *Manager) Get(id string) (Report, error) {
	if !validID(id) {
		return Report{}, fmt.Errorf("%w: invalid report ID", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.active {
		if job.report.ID == id {
			return cloneReport(job.report), nil
		}
	}
	var report Report
	data, err := os.ReadFile(filepath.Join(m.root, "reports", id+".json"))
	if err != nil {
		return report, err
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return report, errors.New("stored analysis report is invalid")
	}
	if report.Status == "running" || report.Status == "queued" {
		report.Status, report.Error = "failed", "Analysis was interrupted by a daemon restart; rescan the image."
	}
	if (report.Status == "succeeded" || report.Status == "partial") && report.Database.Built != "" {
		maxAge := time.Duration(report.Database.MaxAgeHours) * time.Hour
		if maxAge == 0 {
			maxAge = DefaultMaxAge
		}
		if built, err := time.Parse(time.RFC3339, report.Database.Built); err == nil && m.now().Sub(built) > maxAge {
			report.Status, report.Database.State = "stale", "stale"
		}
	}
	if report.Status == "succeeded" || report.Status == "partial" {
		data, err := os.ReadFile(filepath.Join(m.root, "current-database.json"))
		if err == nil {
			var current Database
			if json.Unmarshal(data, &current) != nil {
				return report, errors.New("current database identity metadata is corrupt")
			}
			if current.Checksum != report.Database.Checksum || current.Built != report.Database.Built {
				report.Status = "stale"
				report.Warnings = append(report.Warnings, "A different local database is now installed; rescan to match against its advisories.")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return report, err
		}
	}
	return report, nil
}

func (m *Manager) List() ([]Report, error) {
	entries, err := os.ReadDir(filepath.Join(m.root, "reports"))
	if errors.Is(err, os.ErrNotExist) {
		return []Report{}, nil
	}
	if err != nil {
		return nil, err
	}
	reports := []Report{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		report, err := m.Get(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		report.Packages, report.Findings = nil, nil
		reports = append(reports, report)
	}
	slices.SortFunc(reports, func(a, b Report) int { return strings.Compare(b.StartedAt, a.StartedAt) })
	return reports, nil
}

func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, job := range m.active {
		if job.report.ID == id {
			job.cancel()
			return nil
		}
	}
	return errors.New("analysis is not running")
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	jobs := make([]*scan, 0, len(m.active))
	for _, job := range m.active {
		job.cancel()
		jobs = append(jobs, job)
	}
	m.mu.Unlock()
	for _, job := range jobs {
		select {
		case <-job.done:
		case <-ctx.Done():
			return fmt.Errorf("wait for image analysis cleanup: %w", ctx.Err())
		}
	}
	return nil
}

func (m *Manager) cached(report Report) (*Report, error) {
	data, err := os.ReadFile(filepath.Join(m.root, "cache", lookupKey(report)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cached Report
	if json.Unmarshal(data, &cached) != nil || cached.CacheKey != cacheKey(cached) || cached.Image.Digest != report.Image.Digest || cached.Image.Platform != report.Image.Platform {
		return nil, errors.New("analysis cache identity is corrupt; request a forced rescan")
	}
	return &cached, nil
}

func (m *Manager) Export(ctx context.Context, id, format string) ([]byte, error) {
	report, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	if format == "report" {
		return json.MarshalIndent(report, "", "  ")
	}
	if !validID(report.SBOMID) {
		return nil, errors.New("this analysis has no complete SBOM to export")
	}
	sbomPath := filepath.Join(m.root, "sboms", report.SBOMID+".json")
	if format == "syft-json" {
		return os.ReadFile(sbomPath)
	}
	if format != "spdx-json" && format != "cyclonedx-json" {
		return nil, fmt.Errorf("%w: export format must be report, syft-json, spdx-json or cyclonedx-json", ErrInvalid)
	}
	work, err := os.MkdirTemp(m.root, ".export-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	output, err := m.tool(ctx, work, "syft", "convert", sbomPath, "-o", format)
	if err != nil {
		return nil, err
	}
	if !json.Valid(output.Stdout) {
		return nil, errors.New("Syft returned an invalid machine-readable export")
	}
	return output.Stdout, nil
}
