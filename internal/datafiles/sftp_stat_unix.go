//go:build linux || darwin

package datafiles

import (
	"syscall"

	"github.com/pkg/sftp"
)

func nativeFilesystemStat(directory string) (*sftp.StatVFS, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(directory, &stat); err != nil {
		return nil, err
	}
	return &sftp.StatVFS{
		Bsize: uint64(stat.Bsize), Frsize: uint64(stat.Bsize), Blocks: uint64(stat.Blocks),
		Bfree: uint64(stat.Bfree), Bavail: uint64(stat.Bavail), Files: uint64(stat.Files),
		Ffree: uint64(stat.Ffree), Favail: uint64(stat.Ffree), Namemax: 255,
	}, nil
}
