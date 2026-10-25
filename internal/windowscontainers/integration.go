package windowscontainers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
)

const ContextName = "porto-windows"
const RuntimeName = "io.containerd.runhcs.v1"
const Namespace = "porto-windows"
const Snapshotter = "windows"
const RuntimeDirectoryEnv = "PORTO_WINDOWS_RUNTIME_DIR"

var ErrUnsupported = errors.New("native Windows containers are not supported on this platform")
var ErrOwnership = errors.New("native Windows runtime ownership could not be verified")
var ErrActive = errors.New("native Windows workloads are active; stop them explicitly before changing the runtime")

type Capability struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"`
}

type Capabilities struct {
	Process Capability `json:"process"`
	HyperV  Capability `json:"hyperv"`
}

type Status struct {
	Supported    bool         `json:"supported"`
	Installed    bool         `json:"installed"`
	Running      bool         `json:"running"`
	Owned        bool         `json:"owned"`
	Context      string       `json:"context"`
	Endpoint     string       `json:"endpoint"`
	Host         Host         `json:"host"`
	Message      string       `json:"message,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
}

type Network struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Subnet string `json:"subnet"`
}

type Installation struct {
	Version    int               `json:"version"`
	OwnerSID   string            `json:"ownerSid"`
	Generation string            `json:"generation"`
	Root       string            `json:"root"`
	Host       Host              `json:"host"`
	Hashes     map[string]string `json:"hashes"`
}

func OwnerKey(sid string) string {
	sum := sha256.Sum256([]byte(sid))
	return hex.EncodeToString(sum[:8])
}

func (i Installation) ServiceName() string { return "PortoWindows-" + OwnerKey(i.OwnerSID) }
func (i Installation) Pipe() string {
	return `\\.\pipe\porto-windows-` + OwnerKey(i.OwnerSID)
}
func (i Installation) ContainerdPipe() string {
	return `\\.\pipe\porto-windows-containerd-` + OwnerKey(i.OwnerSID)
}
func (i Installation) Endpoint() string {
	return "npipe:////./pipe/porto-windows-" + OwnerKey(i.OwnerSID)
}
func (i Installation) Deployment() string { return filepath.Join(i.Root, "versions", i.Generation) }
func (i Installation) Bin() string        { return filepath.Join(i.Deployment(), "bin") }
func (i Installation) Binary() string     { return filepath.Join(i.Deployment(), "porto.exe") }
func (i Installation) Data() string       { return filepath.Join(i.Root, "data") }
func (i Installation) DefaultNetwork() string {
	return "porto-" + OwnerKey(i.OwnerSID) + "-nat"
}

func (i Installation) NerdctlArgs() []string {
	return []string{"--config", filepath.Join(i.Deployment(), "nerdctl.toml"),
		"--address", i.ContainerdPipe(), "--namespace", Namespace, "--snapshotter", Snapshotter,
		"--runtime", RuntimeName, "--data-root", filepath.Join(i.Data(), "nerdctl"),
		"--cni-path", i.Bin(), "--cni-netconfpath", filepath.Join(i.Data(), "cni")}
}

func statusForHost(host Host) Status {
	status := Status{Supported: host.PotentiallySupported(), Context: ContextName, Host: host}
	if _, err := host.Isolation("process"); err != nil {
		status.Capabilities.Process.Reason = err.Error()
	} else {
		status.Capabilities.Process.Supported = true
	}
	if _, err := host.Isolation("hyperv"); err != nil {
		status.Capabilities.HyperV.Reason = err.Error()
	} else {
		status.Capabilities.HyperV.Supported = true
	}
	if err := host.Validate(); err != nil {
		status.Message = err.Error()
	}
	return status
}

func validateNetworkName(i Installation, name string) error {
	if !strings.HasPrefix(name, "porto-"+OwnerKey(i.OwnerSID)+"-") ||
		len(name) > 100 || strings.ContainsAny(name, `/\ :`+"\r\n\x00") {
		return fmt.Errorf("%w: native NAT network names must start with porto-%s-", ErrOwnership, OwnerKey(i.OwnerSID))
	}
	return nil
}

func ValidateNetworkName(i Installation, name string) error { return validateNetworkName(i, name) }

func Client() (*http.Client, error) {
	installation, err := Current()
	if err != nil {
		return nil, err
	}
	return ClientFor(installation), nil
}

func ClientFor(installation Installation) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialOwned(ctx, installation)
		},
	}}
}

// Serve is supplied by the daemon package so the privileged service never
// initializes the ordinary user's configuration, project database, or runtimes.
type Serve func(context.Context, Installation, chan<- struct{}) error
