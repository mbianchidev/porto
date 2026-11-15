package app

import "time"

const (
	DockerCleanupInterval = 24 * time.Hour
	DockerCleanupTimeout  = 20 * time.Minute
	// DockerCleanupMinimumAge keeps recently stopped containers and recently
	// pulled or built images out of automatic and manual cleanup.
	DockerCleanupMinimumAge = 24 * time.Hour
)

type CleanupStatus string

const (
	CleanupNotRun      CleanupStatus = "not_run"
	CleanupRunning     CleanupStatus = "running"
	CleanupSucceeded   CleanupStatus = "succeeded"
	CleanupFailed      CleanupStatus = "failed"
	CleanupSkipped     CleanupStatus = "skipped"
	CleanupInterrupted CleanupStatus = "interrupted"
)

type DockerCleanupTrigger string

const (
	CleanupManual    DockerCleanupTrigger = "manual"
	CleanupScheduled DockerCleanupTrigger = "scheduled"
)

type DockerCleanupStep struct {
	Status         CleanupStatus `json:"status"`
	ItemsRemoved   int64         `json:"itemsRemoved"`
	BytesReclaimed *int64        `json:"bytesReclaimed,omitempty"`
	Output         string        `json:"output,omitempty"`
	Error          string        `json:"error,omitempty"`
}

type DockerCleanupResult struct {
	Containers DockerCleanupStep `json:"containers"`
	BuildCache DockerCleanupStep `json:"buildCache"`
	Images     DockerCleanupStep `json:"images"`
	Volumes    DockerCleanupStep `json:"volumes"`
	HostDisk   DockerCleanupStep `json:"hostDisk"`
}

func NewDockerCleanupResult() DockerCleanupResult {
	return DockerCleanupResult{
		Containers: DockerCleanupStep{Status: CleanupNotRun},
		BuildCache: DockerCleanupStep{Status: CleanupNotRun},
		Images:     DockerCleanupStep{Status: CleanupNotRun},
		Volumes:    DockerCleanupStep{Status: CleanupNotRun},
		HostDisk:   DockerCleanupStep{Status: CleanupNotRun},
	}
}

// Normalize marks steps that results saved before they existed did not run.
func (r *DockerCleanupResult) Normalize() {
	for _, step := range []*DockerCleanupStep{&r.Containers, &r.BuildCache, &r.Images, &r.Volumes, &r.HostDisk} {
		if step.Status == "" {
			step.Status = CleanupNotRun
		}
	}
}

type DockerCleanupRun struct {
	ID          int64                `json:"id"`
	Trigger     DockerCleanupTrigger `json:"trigger"`
	Status      CleanupStatus        `json:"status"`
	StartedAt   string               `json:"startedAt"`
	CompletedAt string               `json:"completedAt,omitempty"`
	Result      DockerCleanupResult  `json:"result"`
	Error       string               `json:"error,omitempty"`
}

type DockerCleanupStatus struct {
	Enabled       bool               `json:"enabled"`
	DockerEnabled bool               `json:"dockerEnabled"`
	NextRunAt     string             `json:"nextRunAt,omitempty"`
	Runs          []DockerCleanupRun `json:"runs"`
}
