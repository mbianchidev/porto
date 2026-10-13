//go:build darwin || linux

package datafiles

import (
	"errors"
	"fmt"
	"os"
	"path"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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

func restoreSymlinkTime(root *os.Root, relative string, modified time.Time) error {
	parent, err := root.Open(path.Dir(relative))
	if err != nil {
		return err
	}
	timestamp := unix.NsecToTimespec(modified.UnixNano())
	err = unix.UtimesNanoAt(int(parent.Fd()), path.Base(relative), []unix.Timespec{timestamp, timestamp}, unix.AT_SYMLINK_NOFOLLOW)
	return errors.Join(err, parent.Close())
}

func AvailableSpace(directory string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(directory, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
