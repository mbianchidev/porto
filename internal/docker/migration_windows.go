//go:build windows

package docker

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

func dialMigrationSource(ctx context.Context, endpoint string) (net.Conn, error) {
	if !strings.HasPrefix(endpoint, "npipe://") {
		return nil, fmt.Errorf("%w: source must be a local Docker named pipe", ErrUnsupported)
	}
	name := strings.TrimPrefix(endpoint, "npipe://")
	name = strings.ReplaceAll(name, "/", `\`)
	return winio.DialPipeContext(ctx, name)
}
