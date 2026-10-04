//go:build darwin

package datafiles

import "golang.org/x/sys/unix"

func exchangeDirectories(left, right string) error {
	return unix.RenamexNp(left, right, unix.RENAME_SWAP)
}
