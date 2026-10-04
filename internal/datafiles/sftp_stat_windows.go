//go:build windows

package datafiles

import (
	"github.com/pkg/sftp"
	"golang.org/x/sys/windows"
)

func nativeFilesystemStat(directory string) (*sftp.StatVFS, error) {
	name, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return nil, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(name, &available, &total, &free); err != nil {
		return nil, err
	}
	return &sftp.StatVFS{Bsize: 4096, Frsize: 4096, Blocks: total / 4096, Bfree: free / 4096, Bavail: available / 4096, Namemax: 255}, nil
}
