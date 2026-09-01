//go:build windows

package docker

import (
	"context"
	"net"
	"os"
	"strings"

	"github.com/Microsoft/go-winio"
)

func localContainerdAddresses() []string {
	var addresses []string
	for _, name := range []string{"CONTAINERD_ADDRESS", "NERDCTL_ADDRESS"} {
		if value := normalizeWindowsContainerdAddress(os.Getenv(name)); value != "" {
			addresses = append(addresses, value)
		}
	}
	addresses = append(addresses, `\\.\pipe\containerd-containerd`)
	return uniqueStrings(addresses)
}

func normalizeWindowsContainerdAddress(address string) string {
	address = normalizeContainerdAddress(address)
	if strings.HasPrefix(address, "//./pipe/") {
		return `\\.\pipe\` + strings.TrimPrefix(address, "//./pipe/")
	}
	return address
}

func dialLocalContainerd(ctx context.Context, address string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, address)
}
