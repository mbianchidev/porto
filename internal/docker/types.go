package docker

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
	Context        string             `json:"context"`
	ContextArchive string             `json:"-"`
	Dockerfile     string             `json:"dockerfile"`
	Tag            string             `json:"tag"`
	Tags           []string           `json:"tags,omitempty"`
	Target         string             `json:"target"`
	Platform       string             `json:"platform"`
	Network        string             `json:"network,omitempty"`
	NoCache        bool               `json:"noCache"`
	Pull           bool               `json:"pull,omitempty"`
	BuildArgs      map[string]*string `json:"buildArgs,omitempty"`
	Labels         map[string]string  `json:"labels,omitempty"`
	CacheFrom      []string           `json:"cacheFrom,omitempty"`
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
	Name     string            `json:"name"`
	Driver   string            `json:"driver"`
	Subnet   string            `json:"subnet"`
	Gateway  string            `json:"gateway"`
	Internal bool              `json:"internal"`
	Labels   map[string]string `json:"labels,omitempty"`
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
