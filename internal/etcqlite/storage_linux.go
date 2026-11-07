//go:build linux

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
	// Linux has no MNT_LOCAL flag. Accept the explicitly local development filesystems.
	switch uint64(filesystem.Type) {
	case 0xef53, 0x58465342, 0x9123683e, 0x2fc12fc1, 0x794c7630, 0x01021994, 0x858458f6:
		return nil // ext, XFS, Btrfs, ZFS, overlay, tmpfs, ramfs
	default:
		return fmt.Errorf("%w: Etcqlite needs a supported node-local filesystem; shared/network/FUSE storage is not accepted (type %#x)", ErrInvalid, filesystem.Type)
	}
}
