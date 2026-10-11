//go:build darwin || linux

package datafiles

import (
	"fmt"
	"os"
	"syscall"
)

func fileMetadata(info os.FileInfo) (int, int, int64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid), int(stat.Gid), int64(stat.Blocks) * 512
	}
	return 0, 0, -1
}

func fileIdentity(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 && info.Mode().IsRegular() {
		return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	}
	return ""
}

func preserveFileOwnership(file *os.File, original os.FileInfo) error {
	uid, gid, _ := fileMetadata(original)
	return file.Chown(uid, gid)
}

func AvailableSpace(directory string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(directory, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
