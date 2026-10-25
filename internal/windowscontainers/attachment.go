package windowscontainers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	cni "github.com/containerd/go-cni"
)

func ownedCNIPath(i Installation, name string) (string, error) {
	if err := validateNetworkName(i, name); err != nil {
		return "", err
	}
	return filepath.Join(i.Data(), "cni", Namespace, "nerdctl-"+name+".conflist"), nil
}

func readOwnedCNIConfiguration(i Installation, name string, protect func(string) error) ([]byte, error) {
	path, err := ownedCNIPath(i, name)
	if err != nil {
		return nil, err
	}
	if err := protect(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("validate native CNI namespace directory: %w", err)
	}
	if err := protect(path); err != nil {
		return nil, fmt.Errorf("validate native CNI configuration for %s: %w", name, err)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read native CNI configuration for %s: %w", name, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err := errors.Join(readErr, file.Close()); err != nil {
		return nil, fmt.Errorf("read native CNI configuration for %s: %w", name, err)
	}
	if len(data) > 1024*1024 {
		return nil, fmt.Errorf("%w: native CNI configuration exceeds 1 MiB", ErrOwnership)
	}
	var config struct {
		Name    string
		Plugins []struct {
			Type string
			IPAM struct{ Type string }
		}
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("%w: decode native CNI configuration for %s: %v", ErrOwnership, name, err)
	}
	if config.Name != name {
		return nil, fmt.Errorf("%w: native CNI configuration name %q does not match %q", ErrOwnership, config.Name, name)
	}
	if len(config.Plugins) != 1 || config.Plugins[0].Type != "nat" || config.Plugins[0].IPAM.Type != "" {
		return nil, fmt.Errorf("%w: native network must use only the bundled NAT plugin", ErrOwnership)
	}
	return data, nil
}

type Attachment struct {
	Container string            `json:"container"`
	Network   string            `json:"network"`
	NetworkID string            `json:"networkId,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Endpoints []string          `json:"endpoints,omitempty"`
	Address   string            `json:"address,omitempty"`
	Gateway   string            `json:"gateway,omitempty"`
	Config    []byte            `json:"config,omitempty"`
	Ports     []cni.PortMapping `json:"ports,omitempty"`
}

func ParsePublishedPorts(values []string) ([]cni.PortMapping, error) {
	result := make([]cni.PortMapping, 0, len(values))
	for _, value := range values {
		spec, protocol, hasProtocol := strings.Cut(value, "/")
		if !hasProtocol {
			protocol = "tcp"
		}
		if protocol != "tcp" && protocol != "udp" {
			return nil, errors.New("invalid native published-port protocol")
		}
		fields := strings.Split(spec, ":")
		if len(fields) < 2 || len(fields) > 3 {
			return nil, errors.New("native Windows publishing requires explicit host and container ports")
		}
		ip := "127.0.0.1"
		if len(fields) == 3 {
			ip = fields[0]
			fields = fields[1:]
		}
		if ip == "" {
			ip = "127.0.0.1"
		}
		if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
			return nil, errors.New("invalid native Windows published IPv4 address")
		}
		host, hostErr := strconv.ParseUint(fields[0], 10, 16)
		container, containerErr := strconv.ParseUint(fields[1], 10, 16)
		if hostErr != nil || containerErr != nil || host == 0 || container == 0 {
			return nil, fmt.Errorf("invalid native published port %q", value)
		}
		result = append(result, cni.PortMapping{HostIP: ip, HostPort: int32(host), ContainerPort: int32(container), Protocol: protocol})
	}
	return result, nil
}
