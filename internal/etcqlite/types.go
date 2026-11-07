package etcqlite

import (
	"errors"
	"time"

	"github.com/mbianchidev/porto/internal/resources"
)

var (
	ErrDisabled    = errors.New("Etcqlite integration is disabled; enable it explicitly first")
	ErrConflict    = errors.New("Etcqlite lifecycle conflict")
	ErrInvalid     = errors.New("invalid Etcqlite request")
	ErrUnavailable = errors.New("Etcqlite integration is unavailable")
	ErrUnsupported = errors.New("Etcqlite managed native lifecycle currently supports macOS and Linux only")
)

const OperationTimeout = 2 * time.Minute

type CreateRequest struct {
	Name          string `json:"name"`
	Members       int    `json:"members"`
	AdvertiseHost string `json:"advertiseHost"`
	ListenHost    string `json:"listenHost"`
}

type ProcessIdentity struct {
	PID        int    `json:"pid"`
	Birth      string `json:"birth"`
	Executable string `json:"executable"`
}

type Member struct {
	Name        string           `json:"name"`
	ID          string           `json:"id"`
	ClientURL   string           `json:"clientURL"`
	PeerURL     string           `json:"peerURL"`
	Listen      string           `json:"listen"`
	PeerListen  string           `json:"peerListen"`
	Initialized bool             `json:"initialized"`
	Process     *ProcessIdentity `json:"process,omitempty"`
	State       string           `json:"state,omitempty"`
	Message     string           `json:"message,omitempty"`
	Usage       *resources.Usage `json:"usage,omitempty"`
}

type Instance struct {
	Format         int       `json:"format"`
	Name           string    `json:"name"`
	ClusterID      string    `json:"clusterID"`
	Members        []Member  `json:"members"`
	CreatedAt      time.Time `json:"createdAt"`
	DesiredRunning bool      `json:"desiredRunning"`
	BackupID       string    `json:"backupID,omitempty"`
	State          string    `json:"state"`
	Message        string    `json:"message"`
}

type Status struct {
	Supported bool       `json:"supported"`
	State     string     `json:"state"`
	Message   string     `json:"message"`
	Instances []Instance `json:"instances"`
}

type Discovery struct {
	Format        uint32   `json:"format"`
	Kind          string   `json:"kind"`
	ClusterID     string   `json:"cluster_id"`
	MemberID      string   `json:"member_id"`
	Revision      int64    `json:"revision"`
	SchemaVersion uint32   `json:"schema_version"`
	Capabilities  []string `json:"capabilities"`
	SnapshotPath  string   `json:"snapshot_path"`
	ETag          string   `json:"-"`
}

type SnapshotStatus struct {
	Format          uint32 `json:"format"`
	SchemaVersion   uint32 `json:"schema_version"`
	ClusterID       uint64 `json:"cluster_id"`
	MemberID        uint64 `json:"member_id"`
	Revision        int64  `json:"revision"`
	CompactRevision int64  `json:"compact_revision"`
	AppliedIndex    uint64 `json:"applied_index"`
	ConfigurationID uint32 `json:"configuration_id"`
	Bytes           uint64 `json:"bytes"`
	SHA256          string `json:"sha256"`
}

type Snapshot struct {
	Member string         `json:"member"`
	Path   string         `json:"path"`
	Status SnapshotStatus `json:"status"`
}

type Backup struct {
	ID        string     `json:"id"`
	Path      string     `json:"path"`
	Name      string     `json:"name"`
	ClusterID string     `json:"clusterID"`
	CreatedAt time.Time  `json:"createdAt"`
	Snapshots []Snapshot `json:"snapshots"`
}

type Connection struct {
	ClusterID   string
	Fingerprint string
	Endpoints   []string
	CA          []byte
	Certificate []byte
	Key         []byte
}

type IdentityRequest struct {
	ClusterID string `json:"clusterID"`
	Confirm   bool   `json:"confirm"`
}
