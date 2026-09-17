package app

import "time"

type Project struct {
	ID                int64     `json:"id"`
	Name              string    `json:"name"`
	Path              string    `json:"path"`
	Strategy          string    `json:"strategy"`
	Command           string    `json:"command"`
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
	CleanupLocalMerged     bool     `json:"cleanupLocalMerged"`
	CleanupRemoteMerged    bool     `json:"cleanupRemoteMerged"`
	PruneRemoteTracking    bool     `json:"pruneRemoteTracking"`
	ProtectedBranches      []string `json:"protectedBranches"`
	SQLNotSoLiteEnabled    bool     `json:"sqlNotSoLiteEnabled"`
	KillSwitchEnabled      bool     `json:"killSwitchEnabled"`
	SendboxEnabled         bool     `json:"sendboxEnabled"`
	DockerEnabled          bool     `json:"dockerEnabled"`
	DockerAutoPruneEnabled bool     `json:"dockerAutoPruneEnabled"`
	KubernetesEnabled      bool     `json:"kubernetesEnabled"`
	VMsEnabled             bool     `json:"vmsEnabled"`
	InterfaceDensity       string   `json:"interfaceDensity"`
	ReduceMotion           bool     `json:"reduceMotion"`
	TerminalFontSize       int      `json:"terminalFontSize"`
	TerminalLineHeight     float64  `json:"terminalLineHeight"`
	TerminalCursorBlink    bool     `json:"terminalCursorBlink"`
	TerminalScrollback     int      `json:"terminalScrollback"`
}

const (
	DefaultInterfaceDensity   = "compact"
	DefaultTerminalFontSize   = 12
	DefaultTerminalLineHeight = 1.35
	DefaultTerminalScrollback = 5000
)

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
