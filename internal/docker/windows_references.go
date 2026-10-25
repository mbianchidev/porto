package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/distribution/reference"
	"github.com/mbianchidev/porto/internal/windowscontainers"
)

type windowsImageReferences struct {
	mu     sync.Mutex
	path   string
	values map[string]string
}

func loadWindowsImageReferences(directory string) (*windowsImageReferences, error) {
	state := &windowsImageReferences{path: filepath.Join(directory, "native-image-references.json"), values: make(map[string]string)}
	data, err := os.ReadFile(state.path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &state.values); err != nil {
		return nil, fmt.Errorf("%w: invalid native image reference metadata: %v", windowscontainers.ErrOwnership, err)
	}
	if state.values == nil {
		state.values = make(map[string]string)
	}
	for source, target := range state.values {
		if err := validateWindowsImageReference(source, target); err != nil {
			return nil, err
		}
	}
	return state, nil
}

func validateWindowsImageReference(source, target string) error {
	first, firstErr := reference.ParseNormalizedNamed(source)
	second, secondErr := reference.ParseNormalizedNamed(target)
	if firstErr != nil || secondErr != nil || !strings.Contains(source, "@sha256:") || !strings.Contains(target, "@sha256:") ||
		reference.TrimNamed(first).Name() != reference.TrimNamed(second).Name() {
		return fmt.Errorf("%w: native image resolution must remain an immutable digest in the same repository", windowscontainers.ErrOwnership)
	}
	return nil
}

func (m *Manager) windowsImageReference(value string) string {
	if m.windows == nil {
		return value
	}
	state := m.windows.imageReferences
	state.mu.Lock()
	defer state.mu.Unlock()
	if selected := state.values[normalizeNerdctlReference(value)]; selected != "" {
		return selected
	}
	return value
}

func (m *Manager) recordWindowsImageReference(source, target string) error {
	source, target = normalizeNerdctlReference(source), normalizeNerdctlReference(target)
	if source == target {
		return nil
	}
	if err := validateWindowsImageReference(source, target); err != nil {
		return err
	}
	state := m.windows.imageReferences
	state.mu.Lock()
	defer state.mu.Unlock()
	next := cloneStringMap(state.values)
	if next == nil {
		next = make(map[string]string)
	}
	next[source] = target
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(state.path), 0o700); err != nil {
		return err
	}
	stage := state.path + ".new"
	file, err := os.OpenFile(stage, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(stage)
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(stage, state.path); err != nil {
		return err
	}
	state.values = next
	return nil
}
