package docker

import (
	"io"
	"os"
	"time"
)

type Status struct {
	Enabled       bool   `json:"enabled"`
	Available     bool   `json:"available"`
	Context       string `json:"context"`
	Endpoint      string `json:"endpoint"`
	ClientVersion string `json:"clientVersion"`
	ServerVersion string `json:"serverVersion"`
	Backend       string `json:"backend,omitempty"`
	ProxySocket   string `json:"proxySocket,omitempty"`
	CanonicalPath string `json:"canonicalPath,omitempty"`
	CanonicalLink string `json:"canonicalLink,omitempty"`
	Canonical     bool   `json:"canonical"`
	PreviousLink  string `json:"previousLink,omitempty"`
	Message       string `json:"message,omitempty"`
	Namespace     string `json:"namespace,omitempty"`
	Inventory     string `json:"inventory,omitempty"`
	Revision      uint64 `json:"revision,omitempty"`
	Stale         bool   `json:"stale,omitempty"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
}

type Container struct {
	ID               string                    `json:"id"`
	Name             string                    `json:"name"`
	Image            string                    `json:"image"`
	ImageID          string                    `json:"imageId,omitempty"`
	Command          string                    `json:"command,omitempty"`
	State            string                    `json:"state"`
	Status           string                    `json:"status"`
	Ports            string                    `json:"ports"`
	Networks         string                    `json:"networks"`
	Mounts           string                    `json:"mounts"`
	CreatedAt        string                    `json:"createdAt"`
	Labels           map[string]string         `json:"labels,omitempty"`
	ComposeProject   string                    `json:"composeProject,omitempty"`
	ComposeService   string                    `json:"composeService,omitempty"`
	TaskPresent      bool                      `json:"taskPresent"`
	PID              uint32                    `json:"pid,omitempty"`
	ExitCode         *uint32                   `json:"exitCode,omitempty"`
	ExitSignal       *uint32                   `json:"exitSignal,omitempty"`
	ExitAt           string                    `json:"exitAt,omitempty"`
	ExitReason       string                    `json:"exitReason,omitempty"`
	OOMKilled        bool                      `json:"oomKilled"`
	RestartPolicy    string                    `json:"restartPolicy,omitempty"`
	RestartCount     int                       `json:"restartCount"`
	Health           ContainerHealth           `json:"health"`
	Resources        ContainerResources        `json:"resources"`
	NetworkDetails   []ContainerNetworkState   `json:"networkDetails,omitempty"`
	MountDetails     []ContainerMount          `json:"mountDetails,omitempty"`
	Annotations      map[string]string         `json:"annotations,omitempty"`
	StopSignal       string                    `json:"stopSignal,omitempty"`
	StopTimeout      int                       `json:"stopTimeout,omitempty"`
	UpdatedAt        string                    `json:"updatedAt,omitempty"`
	LastTransition   string                    `json:"lastTransition,omitempty"`
	LastTransitionAt string                    `json:"lastTransitionAt,omitempty"`
	History          []ContainerLifecycleEvent `json:"history,omitempty"`
	InventoryError   string                    `json:"inventoryError,omitempty"`
}

type ContainerHealth struct {
	Status        string `json:"status"`
	FailingStreak int    `json:"failingStreak"`
	Output        string `json:"output,omitempty"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
}

type ContainerResources struct {
	CPUQuota    int64  `json:"cpuQuota,omitempty"`
	CPUPeriod   uint64 `json:"cpuPeriod,omitempty"`
	CPUShares   uint64 `json:"cpuShares,omitempty"`
	CPUSet      string `json:"cpuSet,omitempty"`
	MemoryLimit int64  `json:"memoryLimit,omitempty"`
	MemorySwap  int64  `json:"memorySwap,omitempty"`
	PIDsLimit   int64  `json:"pidsLimit,omitempty"`
}

type ContainerNetworkState struct {
	Name          string `json:"name"`
	HostIP        string `json:"hostIp,omitempty"`
	HostPort      int32  `json:"hostPort,omitempty"`
	ContainerPort int32  `json:"containerPort,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
}

type ContainerMount struct {
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Destination string   `json:"destination"`
	Options     []string `json:"options,omitempty"`
}

type ContainerLifecycleEvent struct {
	Sequence    uint64    `json:"sequence"`
	Topic       string    `json:"topic"`
	Type        string    `json:"type"`
	ContainerID string    `json:"containerId,omitempty"`
	ExecID      string    `json:"execId,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	ExitCode    *uint32   `json:"exitCode,omitempty"`
	ExitSignal  *uint32   `json:"exitSignal,omitempty"`
	OOM         bool      `json:"oom,omitempty"`
	Reason      string    `json:"reason,omitempty"`
}

type RuntimeCapability struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

type ContainerCapabilities struct {
	DirectInventory   RuntimeCapability `json:"directInventory"`
	LifecycleEvents   RuntimeCapability `json:"lifecycleEvents"`
	CheckpointRestore RuntimeCapability `json:"checkpointRestore"`
}

type ContainerSnapshot struct {
	InstanceID       string                    `json:"instanceId"`
	Revision         uint64                    `json:"revision"`
	Available        bool                      `json:"available"`
	Stale            bool                      `json:"stale"`
	Namespace        string                    `json:"namespace,omitempty"`
	Backend          string                    `json:"backend,omitempty"`
	Message          string                    `json:"message,omitempty"`
	ConnectedAt      time.Time                 `json:"connectedAt,omitempty"`
	LastEventAt      time.Time                 `json:"lastEventAt,omitempty"`
	LastReconciledAt time.Time                 `json:"lastReconciledAt,omitempty"`
	Containers       []Container               `json:"containers"`
	Events           []ContainerLifecycleEvent `json:"events,omitempty"`
	Capabilities     ContainerCapabilities     `json:"capabilities"`
}

type Image struct {
	ID         string            `json:"id"`
	Repository string            `json:"repository"`
	Tag        string            `json:"tag"`
	Digest     string            `json:"digest"`
	Size       string            `json:"size"`
	CreatedAt  string            `json:"createdAt"`
	Labels     map[string]string `json:"labels,omitempty"`
}

type Network struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Driver   string            `json:"driver"`
	Scope    string            `json:"scope"`
	Internal string            `json:"internal"`
	IPv6     string            `json:"ipv6"`
	Created  string            `json:"createdAt"`
	Labels   map[string]string `json:"labels,omitempty"`
}

type Volume struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Mountpoint string            `json:"mountpoint"`
	Scope      string            `json:"scope"`
	CreatedAt  string            `json:"createdAt"`
	Labels     map[string]string `json:"labels,omitempty"`
}

type Build struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
	Duration  string `json:"duration"`
	Platform  string `json:"platform"`
}

type BuildRequest struct {
	Context       string             `json:"context"`
	ContextReader io.Reader          `json:"-"`
	Dockerfile    string             `json:"dockerfile"`
	Tag           string             `json:"tag"`
	Tags          []string           `json:"tags,omitempty"`
	Target        string             `json:"target"`
	Platform      string             `json:"platform"`
	Network       string             `json:"network,omitempty"`
	NoCache       bool               `json:"noCache"`
	Pull          bool               `json:"pull,omitempty"`
	BuildArgs     map[string]*string `json:"buildArgs,omitempty"`
	Labels        map[string]string  `json:"labels,omitempty"`
	CacheFrom     []string           `json:"cacheFrom,omitempty"`
}

type ContainerStats struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	CPU           string `json:"cpu"`
	Memory        string `json:"memory"`
	MemoryPC      string `json:"memoryPercent"`
	Network       string `json:"network"`
	BlockIO       string `json:"blockIO"`
	PIDs          string `json:"pids"`
	CPUMillicores int64  `json:"cpuMillicores"`
	MemoryBytes   int64  `json:"memoryBytes"`
}

type CreateNetworkRequest struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	Subnets    []string          `json:"subnets,omitempty"`
	Gateways   []string          `json:"gateways,omitempty"`
	Internal   bool              `json:"internal"`
	EnableIPv6 bool              `json:"enableIPv6"`
	Options    map[string]string `json:"options,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
}

type CreateContainerRequest struct {
	Name        string
	Image       string
	Platform    string
	Command     []string
	Entrypoint  []string
	Environment []string
	Labels      map[string]string
	WorkingDir  string
	User        string
	Hostname    string
	StopSignal  string
	StopTimeout *int
	Healthcheck *ContainerHealthcheck
	Privileged  bool
	SecurityOpt []string
	Tmpfs       map[string]string
	Sysctls     map[string]string
	Devices     []ContainerDevice
	Cgroupns    string
	Userns      string
	Init        bool
	ShmSize     int64
	Networks    []ContainerNetwork
	Volumes     []string
	Publish     []string
	Restart     string
	TTY         bool
	Interactive bool
	Remove      bool
}

type ContainerHealthcheck struct {
	Test          []string
	Interval      time.Duration
	Timeout       time.Duration
	StartPeriod   time.Duration
	StartInterval time.Duration
	Retries       int
}

type ContainerNetwork struct {
	Name    string
	Aliases []string
}

type ContainerDevice struct {
	HostPath      string
	ContainerPath string
	Permissions   string
}

type ContainerUpdate struct {
	Memory     int64
	MemorySwap int64
	NanoCPUs   int64
}

type ExecRequest struct {
	ContainerID  string
	Command      []string
	Environment  []string
	WorkingDir   string
	User         string
	Privileged   bool
	AttachStdin  bool
	AttachStdout bool
	AttachStderr bool
	TTY          bool
}

type PathStat struct {
	Name       string      `json:"name"`
	Size       int64       `json:"size"`
	Mode       os.FileMode `json:"mode"`
	ModTime    time.Time   `json:"mtime"`
	LinkTarget string      `json:"linkTarget"`
}
