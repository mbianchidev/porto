package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type EndpointState struct {
	CanonicalPath string    `json:"canonicalPath"`
	TargetPath    string    `json:"targetPath"`
	Upstream      string    `json:"upstream"`
	PreviousLink  string    `json:"previousLink,omitempty"`
	ActivatedAt   time.Time `json:"activatedAt"`
}

func ActivateEndpoint(canonicalPath, targetPath, upstream, statePath string, replace bool) (EndpointState, error) {
	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		return EndpointState{}, fmt.Errorf("inspect Porto Docker socket: %w", err)
	}
	if targetInfo.Mode()&os.ModeSocket == 0 {
		return EndpointState{}, fmt.Errorf("Porto Docker endpoint target is not a socket: %s", targetPath)
	}
	state := EndpointState{
		CanonicalPath: canonicalPath,
		TargetPath:    targetPath,
		Upstream:      upstream,
		ActivatedAt:   time.Now().UTC(),
	}
	info, err := os.Lstat(canonicalPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return EndpointState{}, fmt.Errorf("inspect canonical Docker endpoint: %w", err)
	case info.Mode()&os.ModeSymlink != 0:
		current, readErr := os.Readlink(canonicalPath)
		if readErr != nil {
			return EndpointState{}, fmt.Errorf("read canonical Docker endpoint: %w", readErr)
		}
		if samePath(current, targetPath) {
			if existing, stateErr := ReadEndpointState(statePath); stateErr == nil {
				return existing, nil
			}
			if state.Upstream == "" || sameDockerEndpoint(state.Upstream, targetPath) || sameDockerEndpoint(state.Upstream, canonicalPath) {
				return EndpointState{}, errors.New("refusing to activate Docker endpoint without a distinct upstream runtime")
			}
			return state, writeEndpointState(statePath, state)
		}
		if !replace {
			return EndpointState{}, fmt.Errorf("%s already points to %s; pass --replace to switch explicitly", canonicalPath, current)
		}
		state.PreviousLink = current
		if state.Upstream == "" || sameDockerEndpoint(state.Upstream, targetPath) || sameDockerEndpoint(state.Upstream, canonicalPath) {
			state.Upstream = "unix://" + current
		}
		if err := os.Remove(canonicalPath); err != nil {
			return EndpointState{}, fmt.Errorf("remove previous Docker endpoint link: %w", err)
		}
	default:
		return EndpointState{}, fmt.Errorf("refusing to replace existing non-symlink Docker endpoint %s", canonicalPath)
	}
	if state.Upstream == "" || sameDockerEndpoint(state.Upstream, targetPath) || sameDockerEndpoint(state.Upstream, canonicalPath) {
		if state.PreviousLink != "" {
			_ = os.Symlink(state.PreviousLink, canonicalPath)
		}
		return EndpointState{}, errors.New("refusing to activate Docker endpoint without a distinct upstream runtime")
	}
	if err := os.Symlink(targetPath, canonicalPath); err != nil {
		if state.PreviousLink != "" {
			_ = os.Symlink(state.PreviousLink, canonicalPath)
		}
		return EndpointState{}, fmt.Errorf("activate canonical Docker endpoint: %w", err)
	}
	if err := writeEndpointState(statePath, state); err != nil {
		_ = os.Remove(canonicalPath)
		if state.PreviousLink != "" {
			_ = os.Symlink(state.PreviousLink, canonicalPath)
		}
		return EndpointState{}, err
	}
	return state, nil
}

func sameDockerEndpoint(endpoint, socketPath string) bool {
	return strings.HasPrefix(endpoint, "unix://") && samePath(strings.TrimPrefix(endpoint, "unix://"), socketPath)
}

func samePath(left, right string) bool {
	leftPath, leftErr := filepath.Abs(left)
	rightPath, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftPath) == filepath.Clean(rightPath)
}

func DeactivateEndpoint(statePath string) error {
	data, err := os.ReadFile(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("Porto does not own an active canonical Docker endpoint")
		}
		return fmt.Errorf("read Docker endpoint state: %w", err)
	}
	var state EndpointState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode Docker endpoint state: %w", err)
	}
	current, err := os.Readlink(state.CanonicalPath)
	if err != nil {
		return fmt.Errorf("inspect active Docker endpoint: %w", err)
	}
	if !samePath(current, state.TargetPath) {
		return fmt.Errorf("refusing to remove Docker endpoint changed outside Porto: %s now points to %s", state.CanonicalPath, current)
	}
	if err := os.Remove(state.CanonicalPath); err != nil {
		return fmt.Errorf("remove Porto Docker endpoint: %w", err)
	}
	if state.PreviousLink != "" {
		if err := os.Symlink(state.PreviousLink, state.CanonicalPath); err != nil {
			return fmt.Errorf("restore previous Docker endpoint %s: %w", state.PreviousLink, err)
		}
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Docker endpoint state: %w", err)
	}
	return nil
}

func ReadEndpointState(statePath string) (EndpointState, error) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		return EndpointState{}, err
	}
	var state EndpointState
	if err := json.Unmarshal(data, &state); err != nil {
		return EndpointState{}, fmt.Errorf("decode Docker endpoint state: %w", err)
	}
	return state, nil
}

func AddEndpointStatus(status Status, canonicalPath, statePath string) Status {
	status.CanonicalPath = canonicalPath
	if target, err := os.Readlink(canonicalPath); err == nil {
		status.CanonicalLink = target
		status.Canonical = samePath(target, status.ProxySocket)
	}
	if state, err := ReadEndpointState(statePath); err == nil {
		status.PreviousLink = state.PreviousLink
	}
	return status
}

func writeEndpointState(path string, state EndpointState) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("Docker endpoint state path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Docker endpoint state directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Docker endpoint state: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".docker-endpoint.*")
	if err != nil {
		return fmt.Errorf("create Docker endpoint state: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("protect Docker endpoint state: %w", err)
	}
	if uidValue, gidValue := os.Getenv("SUDO_UID"), os.Getenv("SUDO_GID"); uidValue != "" && gidValue != "" {
		uid, uidErr := strconv.Atoi(uidValue)
		gid, gidErr := strconv.Atoi(gidValue)
		if uidErr != nil || gidErr != nil {
			_ = temp.Close()
			return errors.New("invalid SUDO_UID or SUDO_GID while writing Docker endpoint state")
		}
		if err := temp.Chown(uid, gid); err != nil {
			_ = temp.Close()
			return fmt.Errorf("restore Docker endpoint state ownership: %w", err)
		}
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write Docker endpoint state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync Docker endpoint state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close Docker endpoint state: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install Docker endpoint state: %w", err)
	}
	return nil
}
