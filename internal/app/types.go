package app

import "time"

type Project struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	Strategy    string    `json:"strategy"`
	Command     string    `json:"command"`
	Port        int       `json:"port"`
	PinnedPort  int       `json:"pinnedPort"`
	Hostname    string    `json:"hostname"`
	PID         int       `json:"pid"`
	Status      string    `json:"status"`
	Branch      string    `json:"branch"`
	Dirty       bool      `json:"dirty"`
	AutoStart   bool      `json:"autoStart"`
	LastStarted time.Time `json:"lastStarted,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type LogLine struct {
	ProjectID int64     `json:"projectId"`
	Stream    string    `json:"stream"`
	Line      string    `json:"line"`
	CreatedAt time.Time `json:"createdAt"`
}

type Settings struct {
	CleanupLocalMerged  bool     `json:"cleanupLocalMerged"`
	CleanupRemoteMerged bool     `json:"cleanupRemoteMerged"`
	PruneRemoteTracking bool     `json:"pruneRemoteTracking"`
	ProtectedBranches   []string `json:"protectedBranches"`
}

type BranchCleanupResult struct {
	LocalDeleted  []string `json:"localDeleted"`
	RemoteDeleted []string `json:"remoteDeleted"`
	Pruned        bool     `json:"pruned"`
}
