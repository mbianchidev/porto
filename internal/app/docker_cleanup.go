package app

import "time"

const (
	DockerCleanupInterval = 7 * 24 * time.Hour
	DockerCleanupTimeout  = 10 * time.Minute
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
	BuildCache DockerCleanupStep `json:"buildCache"`
	Images     DockerCleanupStep `json:"images"`
}

func NewDockerCleanupResult() DockerCleanupResult {
	return DockerCleanupResult{
		BuildCache: DockerCleanupStep{Status: CleanupNotRun},
		Images:     DockerCleanupStep{Status: CleanupNotRun},
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
