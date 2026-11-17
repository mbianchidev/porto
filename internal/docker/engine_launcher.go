package docker

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// EngineLauncherName is the name macOS shows for the engine's VM. The
// Virtualization framework names its service after the process that created
// the VM ("Virtual Machine Service for <name>"), and Lima creates it from a
// hostagent it re-executes from its own path. Starting the engine through an
// alias of limactl with this name therefore labels both processes. A symlink
// is not enough because macOS reports the resolved file name.
const EngineLauncherName = "Porto Docker Engine"

// prepareEngineLauncher returns root/bin/<EngineLauncherName> as a hard link
// to limactl, or a copy when the link is refused (for example across volumes
// or into a protected app bundle). Lima finds share/ and libexec/ beside its
// executable's parent, so root gets symlinks to the real ones. The alias is
// replaced whenever limactl changes, such as after a Porto update.
func prepareEngineLauncher(limactl, root string) (string, error) {
	source, err := filepath.EvalSymlinks(limactl)
	if err != nil {
		return "", fmt.Errorf("resolve limactl: %w", err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("inspect limactl: %w", err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return "", fmt.Errorf("create engine launcher directory: %w", err)
	}
	prefix := filepath.Dir(filepath.Dir(source))
	for _, name := range []string{"share", "libexec"} {
		if err := linkLauncherResource(filepath.Join(prefix, name), filepath.Join(root, name)); err != nil {
			return "", err
		}
	}
	launcher := filepath.Join(bin, EngineLauncherName)
	if current, err := os.Stat(launcher); err == nil {
		if same, err := sameLauncherBinary(launcher, current, source, sourceInfo); err != nil {
			return "", err
		} else if same {
			return launcher, nil
		}
	}
	temporary := launcher + ".tmp"
	if err := os.Remove(temporary); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("remove stale engine launcher: %w", err)
	}
	if linkErr := os.Link(source, temporary); linkErr != nil {
		if err := copyExecutable(source, temporary, sourceInfo.Mode().Perm()); err != nil {
			return "", fmt.Errorf("link engine launcher: %w; copy fallback: %w", linkErr, err)
		}
	}
	if err := os.Rename(temporary, launcher); err != nil {
		_ = os.Remove(temporary)
		return "", fmt.Errorf("install engine launcher: %w", err)
	}
	return launcher, nil
}

func linkLauncherResource(target, link string) error {
	current, readErr := os.Readlink(link)
	if readErr == nil && current == target {
		return nil
	}
	if readErr == nil {
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("replace engine launcher link %s: %w", link, err)
		}
	} else if _, err := os.Lstat(link); err == nil {
		return fmt.Errorf("engine launcher path %s exists and is not a symlink", link)
	}
	if _, err := os.Stat(target); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect Lima resources %s: %w", target, err)
	}
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("link Lima resources for the engine launcher: %w", err)
	}
	return nil
}

func sameLauncherBinary(launcher string, launcherInfo fs.FileInfo, source string, sourceInfo fs.FileInfo) (bool, error) {
	if os.SameFile(launcherInfo, sourceInfo) {
		return true, nil
	}
	if launcherInfo.Size() != sourceInfo.Size() {
		return false, nil
	}
	launcherDigest, err := fileDigest(launcher)
	if err != nil {
		return false, err
	}
	sourceDigest, err := fileDigest(source)
	if err != nil {
		return false, err
	}
	return bytes.Equal(launcherDigest, sourceDigest), nil
}

func fileDigest(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return digest.Sum(nil), nil
}

func copyExecutable(source, destination string, mode fs.FileMode) (err error) {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, output.Close())
		if err != nil {
			_ = os.Remove(destination)
		}
	}()
	_, err = io.Copy(output, input)
	return err
}
