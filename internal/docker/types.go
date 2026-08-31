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
}

type Container struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	ImageID        string            `json:"imageId,omitempty"`
	Command        string            `json:"command,omitempty"`
	State          string            `json:"state"`
	Status         string            `json:"status"`
	Ports          string            `json:"ports"`
	Networks       string            `json:"networks"`
	Mounts         string            `json:"mounts"`
	CreatedAt      string            `json:"createdAt"`
	Labels         map[string]string `json:"labels,omitempty"`
	ComposeProject string            `json:"composeProject,omitempty"`
	ComposeService string            `json:"composeService,omitempty"`
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
	ID       string `json:"id"`
	Name     string `json:"name"`
	CPU      string `json:"cpu"`
	Memory   string `json:"memory"`
	MemoryPC string `json:"memoryPercent"`
	Network  string `json:"network"`
	BlockIO  string `json:"blockIO"`
	PIDs     string `json:"pids"`
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
