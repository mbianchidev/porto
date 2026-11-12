package kubernetes

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func appleBackingFileIdentity(path string) (string, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return "", err
	}
	kind := stat.Mode & unix.S_IFMT
	if kind != unix.S_IFDIR && kind != unix.S_IFREG {
		return "", errors.New("native backing path is not a real directory or regular file")
	}
	return fmt.Sprintf("%d:%d:%d:%d", stat.Dev, stat.Ino, stat.Btim.Sec, stat.Btim.Nsec), nil
}
