package imageanalysis

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

const (
	SyftVersion      = "1.55.0"
	GrypeVersion     = "0.121.0"
	MaxArchiveBytes  = int64(8 * 1024 * 1024 * 1024)
	MaxDatabaseBytes = int64(512 * 1024 * 1024)
	DefaultMaxAge    = 120 * time.Hour
)

var (
	ErrInvalid = errors.New("invalid image analysis request")
	ErrBusy    = errors.New("image analysis queue is full")
)

type Image struct {
	Digest         string `json:"digest"`
	Platform       string `json:"platform"`
	ManifestDigest string `json:"manifestDigest,omitempty"`
}

type Source interface {
	ResolveAnalysisImage(context.Context, string, string) (Image, error)
	ExportAnalysisImage(context.Context, Image, io.Writer) error
}

type Options struct {
	MaxAgeHours int  `json:"maxAgeHours"`
	AllowStale  bool `json:"allowStale"`
	Force       bool `json:"force"`
}

type Database struct {
	State         string `json:"state"`
	SchemaVersion string `json:"schemaVersion,omitempty"`
	Built         string `json:"built,omitempty"`
	Checksum      string `json:"checksum,omitempty"`
	Location      string `json:"location"`
	Bytes         int64  `json:"bytes"`
	MaxAgeHours   int    `json:"maxAgeHours"`
	Offline       bool   `json:"offline"`
	Message       string `json:"message,omitempty"`
}

type Package struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Type      string   `json:"type"`
	PURL      string   `json:"purl,omitempty"`
	Licenses  []string `json:"licenses"`
	Locations []string `json:"locations"`
	Layers    []string `json:"layers"`
}

type Finding struct {
	ID            string   `json:"id"`
	PackageID     string   `json:"packageId"`
	Package       string   `json:"package"`
	Version       string   `json:"version"`
	Severity      string   `json:"severity"`
	FixState      string   `json:"fixState"`
	FixedVersions []string `json:"fixedVersions"`
	Advisory      string   `json:"advisory,omitempty"`
	Namespace     string   `json:"namespace,omitempty"`
	MatchTypes    []string `json:"matchTypes"`
	Remediation   string   `json:"remediation"`
}

type Report struct {
	ID           string    `json:"id"`
	CacheKey     string    `json:"cacheKey,omitempty"`
	SBOMID       string    `json:"sbomId,omitempty"`
	Image        Image     `json:"image"`
	Status       string    `json:"status"`
	Phase        string    `json:"phase"`
	StartedAt    string    `json:"startedAt"`
	CompletedAt  string    `json:"completedAt,omitempty"`
	SyftVersion  string    `json:"syftVersion,omitempty"`
	GrypeVersion string    `json:"grypeVersion,omitempty"`
	Database     Database  `json:"database"`
	Options      Options   `json:"options"`
	Packages     []Package `json:"packages,omitempty"`
	Findings     []Finding `json:"findings,omitempty"`
	Layers       []string  `json:"layers,omitempty"`
	Distribution string    `json:"distribution,omitempty"`
	BaseImage    string    `json:"baseImage"`
	Warnings     []string  `json:"warnings,omitempty"`
	Error        string    `json:"error,omitempty"`
	Cached       bool      `json:"cached"`
}

func SeverityRank(value string) int {
	switch strings.ToLower(value) {
	case "negligible":
		return 0
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "critical":
		return 4
	default:
		return -1
	}
}

func (r Report) ExitCode(threshold string) int {
	if r.Status != "succeeded" {
		return 2
	}
	if threshold == "" {
		return 0
	}
	rank := SeverityRank(threshold)
	if rank < 0 {
		return 2
	}
	for _, finding := range r.Findings {
		if SeverityRank(finding.Severity) >= rank {
			return 1
		}
	}
	return 0
}
