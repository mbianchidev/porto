package imageanalysis

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

func hash(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cacheKey(report Report) string {
	return hash(struct {
		Image                                Image
		Syft, Grype, Schema, Built, Database string
		AllowStale                           bool
		MaxAgeHours                          int
	}{report.Image, report.SyftVersion, report.GrypeVersion, report.Database.SchemaVersion, report.Database.Built, report.Database.Checksum, report.Options.AllowStale, report.Options.MaxAgeHours})
}

func lookupKey(report Report) string {
	report.Image.ManifestDigest = ""
	return cacheKey(report)
}

func decodeSBOM(data []byte, report *Report) error {
	var document struct {
		Artifacts *[]struct {
			ID, Name, Version, Type, PURL string
			Licenses                      []struct{ Value, SPDXExpression string }
			Locations                     []struct{ Path, LayerID string }
		}
		Distro struct{ Name, Version string }
	}
	if err := json.Unmarshal(data, &document); err != nil || document.Artifacts == nil {
		return errors.New("Syft did not return a complete package document")
	}
	report.Packages = []Package{}
	for _, artifact := range *document.Artifacts {
		if artifact.ID == "" || artifact.Name == "" {
			return errors.New("Syft returned a package without an identity")
		}
		pkg := Package{ID: artifact.ID, Name: artifact.Name, Version: artifact.Version, Type: artifact.Type, PURL: artifact.PURL, Licenses: []string{}, Locations: []string{}, Layers: []string{}}
		for _, license := range artifact.Licenses {
			value := license.SPDXExpression
			if value == "" {
				value = license.Value
			}
			if value != "" {
				pkg.Licenses = append(pkg.Licenses, value)
			}
		}
		for _, location := range artifact.Locations {
			pkg.Locations = append(pkg.Locations, location.Path)
			if location.LayerID != "" {
				pkg.Layers = append(pkg.Layers, location.LayerID)
			}
		}
		report.Packages = append(report.Packages, pkg)
	}
	report.Distribution = strings.TrimSpace(document.Distro.Name + " " + document.Distro.Version)
	if report.Distribution == "" {
		report.Warnings = append(report.Warnings, "No operating-system distribution identified; OS package coverage is unknown.")
	}
	if len(report.Packages) == 0 {
		report.Warnings = append(report.Warnings, "No recognized packages; a zero match count does not establish vulnerability coverage.")
	}
	return nil
}

func decodeFindings(data []byte, report *Report) error {
	var document struct {
		Matches *[]struct {
			Artifact      struct{ ID, Name, Version string }
			Vulnerability struct {
				ID, Severity, Namespace, DataSource string
				Fix                                 struct {
					State    string
					Versions []string
				}
			}
			MatchDetails []struct{ Type string }
		}
	}
	if err := json.Unmarshal(data, &document); err != nil || document.Matches == nil {
		return errors.New("Grype did not return a complete vulnerability document")
	}
	report.Findings = []Finding{}
	packages := map[string]Package{}
	for _, pkg := range report.Packages {
		packages[pkg.ID] = pkg
	}
	for _, match := range *document.Matches {
		vulnerability := match.Vulnerability
		if vulnerability.ID == "" || match.Artifact.ID == "" {
			return errors.New("Grype returned a finding without advisory/package identity")
		}
		if pkg, ok := packages[match.Artifact.ID]; !ok || pkg.Name != match.Artifact.Name || pkg.Version != match.Artifact.Version {
			return errors.New("Grype finding does not match the locally generated SBOM package identity")
		}
		finding := Finding{ID: vulnerability.ID, PackageID: match.Artifact.ID, Package: match.Artifact.Name, Version: match.Artifact.Version, Severity: vulnerability.Severity, Namespace: vulnerability.Namespace, FixState: vulnerability.Fix.State, FixedVersions: vulnerability.Fix.Versions, MatchTypes: []string{}}
		if finding.FixedVersions == nil {
			finding.FixedVersions = []string{}
		}
		if SeverityRank(finding.Severity) < 0 {
			report.Warnings = append(report.Warnings, "An advisory has unknown severity; threshold coverage is incomplete.")
		}
		if advisory, err := url.Parse(vulnerability.DataSource); err == nil && advisory.Scheme == "https" && advisory.Host != "" && advisory.User == nil {
			finding.Advisory = advisory.String()
		}
		for _, detail := range match.MatchDetails {
			finding.MatchTypes = append(finding.MatchTypes, detail.Type)
		}
		finding.Remediation = "No fixed version is recorded in this database; review the advisory and rebuild when a verified fix is available."
		if len(finding.FixedVersions) > 0 && finding.FixState == "fixed" {
			finding.Remediation = fmt.Sprintf("Update %s to a recorded fixed version (%s), rebuild the image, and rescan. No replacement image tag has been verified.", finding.Package, strings.Join(finding.FixedVersions, ", "))
		}
		report.Findings = append(report.Findings, finding)
	}
	return nil
}

func cleanSBOM(data []byte) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, errors.New("invalid Syft document")
	}
	if source, ok := document["source"].(map[string]any); ok {
		if metadata, ok := source["metadata"].(map[string]any); ok {
			delete(metadata, "rawConfig")
			delete(metadata, "rawManifest")
			delete(metadata, "repoTags")
			delete(metadata, "imageConfig")
		}
	}
	return json.Marshal(document)
}
