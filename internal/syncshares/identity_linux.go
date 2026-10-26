package syncshares

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func directoryIdentity(path string) (string, error) {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_INO|unix.STATX_BTIME|unix.STATX_TYPE, &stat); err != nil {
		return "", err
	}
	if stat.Mask&unix.STATX_BTIME == 0 || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return "", errors.New("synchronized roots require a directory with observable birth-time identity")
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d", stat.Dev_major, stat.Dev_minor, stat.Ino, stat.Btime.Sec, stat.Btime.Nsec), nil
}
