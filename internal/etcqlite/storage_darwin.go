//go:build darwin

package etcqlite

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func requireLocalStorage(path string) error {
	var filesystem unix.Statfs_t
	if err := unix.Statfs(path, &filesystem); err != nil {
		return fmt.Errorf("inspect Etcqlite node-local filesystem: %w", err)
	}
	if filesystem.Flags&unix.MNT_LOCAL == 0 {
		return fmt.Errorf("%w: Etcqlite data must use node-local storage, not a shared/network filesystem", ErrInvalid)
	}
	return nil
}
