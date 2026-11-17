package app

import (
	"fmt"
	"time"
)

type Project struct {
	ID                int64     `json:"id"`
	Name              string    `json:"name"`
	Path              string    `json:"path"`
	Strategy          string    `json:"strategy"`
	Command           string    `json:"command"`
	ComposeFile       string    `json:"-"`
	Port              int       `json:"port"`
	PinnedPort        int       `json:"pinnedPort"`
	Hostname          string    `json:"hostname"`
	BaseHostname      string    `json:"baseHostname"`
	HTTPSURL          string    `json:"httpsUrl"`
	SourcePath        string    `json:"sourcePath"`
	ManagedInstance   bool      `json:"managedInstance"`
	DefaultBranch     string    `json:"defaultBranch"`
	PID               int       `json:"pid"`
	Status            string    `json:"status"`
	Branch            string    `json:"branch"`
	Dirty             bool      `json:"dirty"`
	AutoStart         bool      `json:"autoStart"`
	LastStarted       time.Time `json:"lastStarted,omitempty"`
	UpdatedAt         time.Time `json:"updatedAt"`
	SendboxConfigured bool      `json:"sendboxConfigured"`
	SendboxStatus     string    `json:"sendboxStatus"`
	SendboxMessage    string    `json:"sendboxMessage"`
}

type LogLine struct {
	ProjectID int64     `json:"projectId"`
	Stream    string    `json:"stream"`
	Line      string    `json:"line"`
	CreatedAt time.Time `json:"createdAt"`
}

type Settings struct {
	CleanupLocalMerged        bool     `json:"cleanupLocalMerged"`
	CleanupRemoteMerged       bool     `json:"cleanupRemoteMerged"`
	PruneRemoteTracking       bool     `json:"pruneRemoteTracking"`
	ProtectedBranches         []string `json:"protectedBranches"`
	SQLNotSoLiteEnabled       bool     `json:"sqlNotSoLiteEnabled"`
	EtcqliteEnabled           bool     `json:"etcqliteEnabled"`
	KillSwitchEnabled         bool     `json:"killSwitchEnabled"`
	SendboxEnabled            bool     `json:"sendboxEnabled"`
	DockerEnabled             bool     `json:"dockerEnabled"`
	DockerAutoPruneEnabled    bool     `json:"dockerAutoPruneEnabled"`
	KubernetesEnabled         bool     `json:"kubernetesEnabled"`
	KubeconfigDirectory       string   `json:"kubeconfigDirectory"`
	KubeconfigActiveDirectory string   `json:"kubeconfigActiveDirectory,omitempty"`
	KubeconfigStorageError    string   `json:"kubeconfigStorageError,omitempty"`
	KubeconfigRestartRequired bool     `json:"kubeconfigRestartRequired,omitempty"`
	VMsEnabled                bool     `json:"vmsEnabled"`
	InterfaceDensity          string   `json:"interfaceDensity"`
	Theme                     string   `json:"theme"`
	ReduceMotion              bool     `json:"reduceMotion"`
	TerminalFontSize          int      `json:"terminalFontSize"`
	TerminalLineHeight        float64  `json:"terminalLineHeight"`
	TerminalCursorBlink       bool     `json:"terminalCursorBlink"`
	TerminalScrollback        int      `json:"terminalScrollback"`
	LogRetentionDays          int      `json:"logRetentionDays"`
	LogMaxFileMB              int      `json:"logMaxFileMB"`
	LogMaxTotalMB             int      `json:"logMaxTotalMB"`
	// LogSizeLimitsFromEnvironment reports that PORTO_LOG_MAX_FILE_MB or
	// PORTO_LOG_MAX_TOTAL_MB override the stored limits for this daemon.
	LogSizeLimitsFromEnvironment bool `json:"logSizeLimitsFromEnvironment,omitempty"`
}

const (
	DefaultInterfaceDensity   = "compact"
	DefaultTheme              = "dark"
	DefaultTerminalFontSize   = 12
	DefaultTerminalLineHeight = 1.35
	DefaultTerminalScrollback = 5000
	DefaultLogRetentionDays   = 7
	MaximumLogRetentionDays   = 365
	DefaultLogMaxFileMB       = 10
	DefaultLogMaxTotalMB      = 50
	MaximumLogSizeMB          = 100_000
	// LogMegabyte is the decimal megabyte used by log size limits.
	LogMegabyte = 1_000_000
)

// ValidateLogSizeLimits checks the live log cap and the total log cap in MB.
// The total must exceed the live cap so archives always have room.
func ValidateLogSizeLimits(fileMB, totalMB int) error {
	if fileMB < 1 || fileMB > MaximumLogSizeMB {
		return fmt.Errorf("log file limit must be between 1 and %d MB, got %d", MaximumLogSizeMB, fileMB)
	}
	if totalMB < 1 || totalMB > MaximumLogSizeMB {
		return fmt.Errorf("total log limit must be between 1 and %d MB, got %d", MaximumLogSizeMB, totalMB)
	}
	if totalMB <= fileMB {
		return fmt.Errorf("total log limit (%d MB) must be larger than the log file limit (%d MB) to leave room for archives", totalMB, fileMB)
	}
	return nil
}

type RegistryProfile struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Provider         string `json:"provider"`
	Server           string `json:"server"`
	Username         string `json:"username"`
	TestImage        string `json:"testImage"`
	Enabled          bool   `json:"enabled"`
	Verified         bool   `json:"verified"`
	CredentialStored bool   `json:"credentialStored"`
	LastVerifiedAt   string `json:"lastVerifiedAt,omitempty"`
	LastError        string `json:"lastError,omitempty"`
	CreatedAt        string `json:"createdAt"`
	UpdatedAt        string `json:"updatedAt"`
}

type BranchCleanupResult struct {
	LocalDeleted  []string `json:"localDeleted"`
	RemoteDeleted []string `json:"remoteDeleted"`
	Pruned        bool     `json:"pruned"`
}
