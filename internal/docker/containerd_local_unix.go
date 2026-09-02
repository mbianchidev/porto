//go:build !windows

package docker

import (
	"context"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

func localContainerdAddresses() []string {
	var addresses []string
	for _, name := range []string{"CONTAINERD_ADDRESS", "NERDCTL_ADDRESS"} {
		if value := normalizeContainerdAddress(os.Getenv(name)); value != "" {
			addresses = append(addresses, value)
		}
	}
	if runtimeDirectory := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDirectory != "" {
		addresses = append(addresses,
			filepath.Join(runtimeDirectory, "containerd-rootless", "containerd.sock"),
			filepath.Join(runtimeDirectory, "containerd", "containerd.sock"),
		)
	}
	if current, err := user.Current(); err == nil && current.Uid != "" {
		runtimeDirectory := filepath.Join("/run/user", current.Uid)
		addresses = append(addresses,
			filepath.Join(runtimeDirectory, "containerd-rootless", "containerd.sock"),
			filepath.Join(runtimeDirectory, "containerd", "containerd.sock"),
		)
	}
	addresses = append(addresses, "/run/containerd/containerd.sock")
	return uniqueStrings(addresses)
}

func dialLocalContainerd(ctx context.Context, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", address)
}
