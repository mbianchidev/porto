package datafiles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func ReplaceDirectory(ctx context.Context, destination string, populate func(string) error, beforeCommit func() error) (err error) {
	info, err := os.Lstat(destination)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: restore destination must be a real directory", ErrInvalid)
	}
	parent := filepath.Dir(destination)
	stage, err := os.MkdirTemp(parent, ".porto-restore-")
	if err != nil {
		return err
	}
	removeStage := true
	defer func() {
		if removeStage {
			err = errors.Join(err, os.RemoveAll(stage))
		}
	}()
	if err := populate(stage); err != nil {
		return err
	}
	if err := os.Chmod(stage, info.Mode().Perm()); err != nil {
		return err
	}
	stageDirectory, err := os.Open(stage)
	if err != nil {
		return err
	}
	if err := errors.Join(preserveFileOwnership(stageDirectory, info), stageDirectory.Close()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if beforeCommit != nil {
		if err := beforeCommit(); err != nil {
			return err
		}
	}
	current, err := os.Lstat(destination)
	if err != nil || !os.SameFile(info, current) {
		return errors.Join(ErrConflict, err)
	}
	if err := exchangeDirectories(stage, destination); err != nil {
		return fmt.Errorf("publish verified restore; original data is unchanged: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		if rollbackErr := exchangeDirectories(stage, destination); rollbackErr != nil {
			removeStage = false
			return errors.Join(err, fmt.Errorf("restore rollback failed; original data remains at %s: %w", stage, rollbackErr))
		}
		return err
	}
	return nil
}

func syncDirectory(name string) error {
	directory, err := os.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
