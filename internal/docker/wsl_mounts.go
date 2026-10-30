package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/wsl"
	"gopkg.in/yaml.v3"
)

func wslMountPlan(configuration []byte, previous, desired []wsl.Share) (string, error) {
	var document struct {
		Mounts []map[string]any `yaml:"mounts"`
	}
	if err := yaml.Unmarshal(configuration, &document); err != nil {
		return "", fmt.Errorf("read owned Lima mounts: %w", err)
	}
	mounts := make([]map[string]any, 0, len(document.Mounts)+len(desired))
	for _, mount := range document.Mounts {
		owned := false
		for _, old := range previous {
			if mount["location"] == old.WindowsPath && mount["mountPoint"] == old.GuestPath {
				owned = true
				break
			}
		}
		if !owned {
			mounts = append(mounts, mount)
		}
	}
	for _, share := range desired {
		for _, mount := range mounts {
			if mount["mountPoint"] == share.GuestPath {
				return "", fmt.Errorf("WSL share conflicts with an unrelated Lima mount at %s; no mount was replaced", share.GuestPath)
			}
		}
		mounts = append(mounts, map[string]any{
			"location": share.WindowsPath, "mountPoint": share.GuestPath, "writable": true,
			"sshfs": map[string]any{"cache": false},
		})
	}
	data, err := json.Marshal(mounts)
	if err != nil {
		return "", err
	}
	// yq expression literals unescape backslashes again, destroying UNC roots.
	return `.mounts = ("` + base64.StdEncoding.EncodeToString(data) + `" | @base64d | from_json)`, nil
}

func (m *Manager) ApplyWSLMounts(ctx context.Context, restartConfirmed bool) (err error) {
	if !restartConfirmed {
		return errors.New("explicit restartEngine consent is required; applying WSL shares interrupts the owned Linux engine")
	}
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return err
	}
	defer release()
	if m.wslShares == nil {
		return fmt.Errorf("%w: selected-distribution WSL integration is not configured", ErrUnsupported)
	}
	if m.nativeClose != nil {
		if err := m.nativeClose(ctx); err != nil {
			return err
		}
	}
	m.installMu.Lock()
	defer m.installMu.Unlock()
	lease, err := acquireEngineInstallLock(filepath.Join(m.stateDir, engineLockFile))
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, lease.Close())
		m.invalidateContainerInventory()
	}()
	state, err := m.readEngineState()
	if err != nil {
		return err
	}
	if state.Mode != "lima" || state.Instance != engineInstanceName || state.OwnerID == "" {
		return fmt.Errorf("%w: only Porto's existing owned Lima/QEMU engine can receive WSL shares", ErrUnsupported)
	}
	output, err := m.runStdoutCommand(ctx, 20*time.Second, "inspect owned engine mount configuration", "limactl", "list", state.Instance, "--json")
	if err != nil {
		return err
	}
	var instance struct {
		Dir    string `json:"dir"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &instance); err != nil || instance.Dir == "" || filepath.Base(instance.Dir) != state.Instance {
		return errors.New("Lima did not return the owned engine's configuration directory")
	}
	wasRunning := strings.EqualFold(instance.Status, "running")
	if !wasRunning {
		if _, err := m.startLimaRuntime(ctx, "start owned engine to verify WSL mount ownership", state.Instance); err != nil {
			return err
		}
	}
	if err := m.verifyLimaOwnership(ctx, state.OwnerID); err != nil {
		return err
	}
	configurationPath := filepath.Join(instance.Dir, "lima.yaml")
	info, err := os.Lstat(configurationPath)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("owned Lima configuration is not a regular file")
	}
	original, err := os.ReadFile(configurationPath)
	if err != nil {
		return err
	}
	shares, err := m.wslShares()
	if err != nil {
		return err
	}
	expression, err := wslMountPlan(original, state.WSLMounts, shares)
	if err != nil {
		return err
	}
	if _, err := m.runCommand(ctx, 5*time.Minute, "stop owned engine to apply WSL shares", nil, "limactl", "stop", state.Instance); err != nil {
		return err
	}
	changed := false
	defer func() {
		if err == nil {
			return
		}
		if changed {
			err = errors.Join(err, writeLimaConfiguration(configurationPath, original, info.Mode().Perm()), m.writeEngineState(state))
		}
		if wasRunning {
			_, restartErr := m.startLimaRuntime(context.Background(), "restore owned engine after failed WSL mount setup", state.Instance)
			err = errors.Join(err, restartErr)
		}
	}()
	if _, err := m.runCommand(ctx, 30*time.Second, "apply only owned WSL shares", nil,
		"limactl", "edit", "--tty=false", "--set="+expression, state.Instance); err != nil {
		return err
	}
	changed = true
	next := state
	next.WSLMounts = shares
	if err := m.writeEngineState(next); err != nil {
		return err
	}
	if wasRunning {
		if _, err := m.startLimaRuntime(ctx, "restart owned engine with selected WSL shares", state.Instance); err != nil {
			return err
		}
		if err := m.verifyLimaOwnership(ctx, state.OwnerID); err != nil {
			return err
		}
	}
	return nil
}

func writeLimaConfiguration(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".porto-lima-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
