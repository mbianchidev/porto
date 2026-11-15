//go:build !windows

package docker

import (
	"fmt"
	"os"
	"syscall"
)

// allocatedFileBytes reports the blocks a sparse file occupies on the host.
func allocatedFileBytes(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%w: allocation of %s is not reported", ErrUnsupported, path)
	}
	return int64(stat.Blocks) * 512, nil
}
