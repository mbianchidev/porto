//go:build !windows

package docker

import (
	"context"
	"fmt"
	"net"
	"strings"
)

func dialMigrationSource(ctx context.Context, endpoint string) (net.Conn, error) {
	if !strings.HasPrefix(endpoint, "unix://") {
		return nil, fmt.Errorf("%w: source is not a Unix socket", ErrUnsupported)
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(endpoint, "unix://"))
}
