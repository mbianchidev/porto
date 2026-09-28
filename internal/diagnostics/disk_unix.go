//go:build !windows

package diagnostics

import "syscall"

func diskCapacity(path string) (uint64, uint64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, 0, err
	}
	blockSize := uint64(stats.Bsize)
	return uint64(stats.Bavail) * blockSize, uint64(stats.Blocks) * blockSize, nil
}
