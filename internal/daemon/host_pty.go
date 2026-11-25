package daemon

import (
	"context"
	"os/exec"

	"github.com/porto-containers/porto/internal/runtimes"
)

type hostPTY = runtimes.PTY

func startHostPTY(ctx context.Context, command *exec.Cmd) (*hostPTY, error) {
	return runtimes.StartPTY(ctx, command, 120, 32)
}

func hostPTYSupported() bool { return runtimes.PTYSupported() }
