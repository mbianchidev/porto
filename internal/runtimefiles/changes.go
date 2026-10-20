package runtimefiles

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/containerd/continuity/fs"
	"github.com/mbianchidev/porto/internal/datafiles"
)

func CompareDirectories(ctx context.Context, lower, upper string, output io.Writer) error {
	for _, directory := range []string{lower, upper} {
		info, err := os.Lstat(directory)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(directory) || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: filesystem comparison requires absolute directories", datafiles.ErrInvalid)
		}
	}
	changes := make([]FilesystemChange, 0)
	err := fs.Changes(ctx, lower, upper, func(kind fs.ChangeKind, path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if kind == fs.ChangeKindUnmodified {
			return nil
		}
		if len(changes) >= datafiles.MaxEntries {
			return datafiles.ErrLimit
		}
		change := FilesystemChange{Path: filepath.ToSlash(path)}
		switch kind {
		case fs.ChangeKindAdd:
			change.Kind = 1
		case fs.ChangeKindModify:
			change.Kind = 0
		case fs.ChangeKindDelete:
			change.Kind = 2
		default:
			return fmt.Errorf("invalid filesystem change kind %d", kind)
		}
		changes = append(changes, change)
		return nil
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(changes)
}
