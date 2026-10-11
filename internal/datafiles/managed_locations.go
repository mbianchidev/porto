package datafiles

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbianchidev/porto/internal/config"
)

func ManagedLocation(value string) (string, string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", "", err
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(value) {
		return "", "", fmt.Errorf("%w: managed archive location must be absolute", ErrInvalid)
	}
	relative, err := filepath.Rel(base, filepath.Clean(value))
	if err != nil || !filepath.IsLocal(relative) {
		return "", "", fmt.Errorf("%w: archive paths must remain inside Porto's managed data directories", ErrInvalid)
	}
	first, _, _ := strings.Cut(filepath.ToSlash(relative), "/")
	if first != "transfers" && first != "backups" {
		return "", "", fmt.Errorf("%w: only managed transfers and backups locations are permitted", ErrInvalid)
	}
	return base, relative, nil
}

func OpenManagedArchive(value string) (*os.Root, *os.File, error) {
	base, relative, err := ManagedLocation(value)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, nil, err
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, nil, errors.Join(err, root.Close())
	}
	return root, file, nil
}
