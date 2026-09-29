package diagnostics

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

type BundleSource struct {
	Name        string
	Description string
	Read        func() ([]byte, error)
}

type BundleEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Size        int64  `json:"size"`
	Redactions  int    `json:"redactions"`
}

type BundlePreview struct {
	Entries  []BundleEntry `json:"entries"`
	Warnings []string      `json:"warnings,omitempty"`
}

type bundleFile struct {
	name        string
	description string
	data        []byte
	redactions  int
}

func PreviewBundle(report Report, sources []BundleSource, context RedactionContext) BundlePreview {
	preview, _ := prepareBundle(report, sources, context)
	return preview
}

func BuildBundle(
	writer io.Writer,
	report Report,
	sources []BundleSource,
	context RedactionContext,
) (BundlePreview, error) {
	preview, files := prepareBundle(report, sources, context)
	archive := zip.NewWriter(writer)
	var archiveErrors []error
	for _, file := range files {
		header := &zip.FileHeader{Name: file.name, Method: zip.Deflate}
		header.SetModTime(report.GeneratedAt)
		entry, err := archive.CreateHeader(header)
		if err != nil {
			archiveErrors = append(archiveErrors, fmt.Errorf("create diagnostic bundle entry %q: %w", file.name, err))
			continue
		}
		if _, err := entry.Write(file.data); err != nil {
			archiveErrors = append(archiveErrors, fmt.Errorf("write diagnostic bundle entry %q: %w", file.name, err))
		}
	}
	if err := archive.Close(); err != nil {
		archiveErrors = append(archiveErrors, fmt.Errorf("close diagnostic bundle: %w", err))
	}
	return preview, errors.Join(archiveErrors...)
}

func prepareBundle(
	report Report,
	sources []BundleSource,
	context RedactionContext,
) (BundlePreview, []bundleFile) {
	preview := BundlePreview{Entries: make([]BundleEntry, 0, len(sources)+2)}
	files := make([]bundleFile, 0, len(sources)+2)
	reportData, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		preview.Warnings = append(preview.Warnings, "encode diagnostic report: "+err.Error())
	} else {
		reportData = append(reportData, '\n')
		reportData, reportRedactions := Redact(reportData, context)
		files = append(files, bundleFile{
			name: "report.json", description: "Structured diagnostic report", data: reportData,
			redactions: reportRedactions,
		})
		preview.Entries = append(preview.Entries, BundleEntry{
			Name: "report.json", Description: "Structured diagnostic report", Size: int64(len(reportData)),
			Redactions: reportRedactions,
		})
	}
	for _, source := range sources {
		name, nameErr := safeBundleName(source.Name)
		if nameErr != nil {
			preview.Warnings = append(preview.Warnings, nameErr.Error())
			continue
		}
		if source.Read == nil {
			preview.Warnings = append(preview.Warnings, fmt.Sprintf("read %s: source is unavailable", name))
			continue
		}
		data, readErr := source.Read()
		if readErr != nil {
			preview.Warnings = append(preview.Warnings, fmt.Sprintf("read %s: %v", name, readErr))
			continue
		}
		redacted, redactions := Redact(data, context)
		files = append(files, bundleFile{
			name: name, description: source.Description, data: redacted, redactions: redactions,
		})
		preview.Entries = append(preview.Entries, BundleEntry{
			Name: name, Description: source.Description, Size: int64(len(redacted)), Redactions: redactions,
		})
	}
	if len(preview.Warnings) > 0 {
		warningRedactions := 0
		for index, warning := range preview.Warnings {
			redacted, count := Redact([]byte(warning), context)
			preview.Warnings[index] = string(redacted)
			warningRedactions += count
		}
		warnings := []byte(strings.Join(preview.Warnings, "\n") + "\n")
		files = append(files, bundleFile{
			name: "bundle-warnings.txt", description: "Bundle sources that could not be collected",
			data: warnings, redactions: warningRedactions,
		})
		preview.Entries = append(preview.Entries, BundleEntry{
			Name: "bundle-warnings.txt", Description: "Bundle sources that could not be collected",
			Size: int64(len(warnings)), Redactions: warningRedactions,
		})
	}
	return preview, files
}

func safeBundleName(name string) (string, error) {
	cleaned := path.Clean(strings.TrimSpace(name))
	if cleaned == "." || cleaned == "" || strings.HasPrefix(cleaned, "/") ||
		cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("invalid diagnostic bundle entry %q", name)
	}
	return cleaned, nil
}
