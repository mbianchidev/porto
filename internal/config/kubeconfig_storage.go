package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const kubeconfigLocationFile = "kubeconfig-location.json"

func NormalizeKubeconfigDirectory(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("kubeconfig folder must not contain control characters")
	}
	if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve kubeconfig folder home: %w", err)
		}
		value = filepath.Join(home, strings.TrimLeft(value[1:], `/\`))
	}
	if !filepath.IsAbs(value) {
		return "", errors.New("kubeconfig folder must be an absolute path or start with ~/")
	}
	return filepath.Clean(value), nil
}

func desiredKubeconfigDirectory(value string) (string, error) {
	directory, err := NormalizeKubeconfigDirectory(value)
	if err != nil || directory != "" {
		return directory, err
	}
	root, err := Dir()
	return filepath.Join(root, "kubernetes"), err
}

func activeKubeconfigDirectory() (string, error) {
	root, err := Dir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(root, kubeconfigLocationFile))
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Join(root, "kubernetes"), nil
	}
	if err != nil {
		return "", fmt.Errorf("read active kubeconfig folder: %w", err)
	}
	var location struct {
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(data, &location); err != nil {
		return "", fmt.Errorf("decode active kubeconfig folder: %w", err)
	}
	directory, err := NormalizeKubeconfigDirectory(location.Directory)
	if err != nil || directory == "" {
		return "", errors.Join(errors.New("active kubeconfig folder is invalid"), err)
	}
	return directory, nil
}

func validateKubeconfigDestination(destination string) (err error) {
	if filepath.Dir(destination) == destination {
		return errors.New("choose a dedicated kubeconfig folder, not a filesystem root")
	}
	parent, err := os.OpenRoot(filepath.Dir(destination))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	name := filepath.Base(destination)
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("kubeconfig destination must be an empty real directory")
	}
	directory, err := parent.Open(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	opened, err := directory.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, opened) || !opened.IsDir() {
		return errors.New("kubeconfig destination changed during validation")
	}
	entries, err := directory.ReadDir(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) != 0 {
		return errors.New("kubeconfig destination must be empty; existing files will not be overwritten")
	}
	return nil
}

func ValidateKubeconfigDirectoryChange(value string) error {
	current, err := activeKubeconfigDirectory()
	if err != nil {
		return err
	}
	destination, err := desiredKubeconfigDirectory(value)
	if err != nil {
		return err
	}
	if current == destination {
		return nil
	}
	if info, err := os.Lstat(current); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("managed kubeconfig source must be a real directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Resolve existing ancestors too, so aliases cannot move a folder inside itself.
	canonical := func(path string) (string, error) {
		parent := path
		var suffix []string
		for {
			resolved, err := filepath.EvalSymlinks(parent)
			if err == nil {
				for index := len(suffix) - 1; index >= 0; index-- {
					resolved = filepath.Join(resolved, suffix[index])
				}
				return resolved, nil
			}
			if !errors.Is(err, os.ErrNotExist) || filepath.Dir(parent) == parent {
				return "", err
			}
			suffix = append(suffix, filepath.Base(parent))
			parent = filepath.Dir(parent)
		}
	}
	source, err := canonical(current)
	if err != nil {
		return err
	}
	target, err := canonical(destination)
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{{source, target}, {target, source}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))) {
			return errors.New("kubeconfig folders must not overlap, including through symbolic links")
		}
	}
	if err := validateKubeconfigDestination(destination); err != nil {
		return err
	}
	entries, err := os.ReadDir(current)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".upgrade-") {
			return errors.New("finish or recover Kubernetes upgrades before moving kubeconfigs")
		}
	}
	return nil
}

// ApplyKubeconfigDirectory runs before daemon workers start. The active locator
// changes only after every file is copied; cleanup never removes unknown files.
func ApplyKubeconfigDirectory(ctx context.Context, value string) (directory string, err error) {
	current, err := activeKubeconfigDirectory()
	if err != nil {
		return "", err
	}
	directory, err = desiredKubeconfigDirectory(value)
	if err != nil {
		return current, err
	}
	if directory == current {
		return KubernetesConfigDir()
	}
	if err := ValidateKubeconfigDirectoryChange(value); err != nil {
		return current, err
	}
	if _, err := KubernetesConfigDir(); err != nil {
		return current, err
	}
	if info, err := os.Lstat(current); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return current, errors.Join(errors.New("managed kubeconfig source must be a real directory"), err)
	}
	type entry struct {
		path string
		info fs.FileInfo
	}
	var created, originals []entry
	committed := false
	remove := func(entries []entry, unchanged bool) error {
		var failures []error
		for index := len(entries) - 1; index >= 0; index-- {
			item := entries[index]
			info, err := os.Lstat(item.path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				failures = append(failures, err)
				continue
			}
			if !os.SameFile(info, item.info) || (unchanged && !info.IsDir() &&
				(info.Size() != item.info.Size() || !info.ModTime().Equal(item.info.ModTime()))) {
				failures = append(failures, fmt.Errorf("preserving kubeconfig file changed during migration: %s", item.path))
				continue
			}
			if err := os.Remove(item.path); err != nil {
				failures = append(failures, fmt.Errorf("remove migrated kubeconfig path %s: %w", item.path, err))
			}
		}
		return errors.Join(failures...)
	}
	defer func() {
		if !committed {
			err = errors.Join(err, remove(created, false))
		}
	}()
	if err := os.MkdirAll(filepath.Dir(directory), 0o700); err != nil {
		return current, err
	}
	if _, err := os.Stat(directory); err == nil {
		if err := os.Remove(directory); err != nil {
			return current, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return current, err
	}
	err = filepath.WalkDir(current, func(source string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular kubeconfig storage entry: %s", source)
		}
		relative, err := filepath.Rel(current, source)
		if err != nil {
			return err
		}
		target := filepath.Join(directory, relative)
		if info.IsDir() {
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
		} else {
			input, err := os.Open(source)
			if err != nil {
				return err
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return errors.Join(err, input.Close())
			}
			targetInfo, err := output.Stat()
			if err == nil {
				created = append(created, entry{target, targetInfo})
			}
			n, copyErr := io.Copy(output, input)
			err = errors.Join(err, copyErr, output.Sync(), output.Close(), input.Close())
			after, statErr := os.Stat(source)
			if err = errors.Join(err, statErr); err != nil {
				return err
			}
			if n != info.Size() || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
				return fmt.Errorf("kubeconfig changed during migration: %s", source)
			}
		}
		if info.IsDir() {
			targetInfo, err := os.Lstat(target)
			if err != nil {
				return err
			}
			created = append(created, entry{target, targetInfo})
		}
		originals = append(originals, entry{source, info})
		return nil
	})
	if err != nil {
		return current, err
	}
	root, err := Dir()
	if err != nil {
		return current, err
	}
	data, err := json.Marshal(struct {
		Directory string `json:"directory"`
	}{directory})
	if err != nil {
		return current, err
	}
	if err := ctx.Err(); err != nil {
		return current, err
	}
	if err := WriteFileAtomic(filepath.Join(root, kubeconfigLocationFile), data, 0o600); err != nil {
		return current, err
	}
	committed = true
	return directory, remove(originals, true)
}
