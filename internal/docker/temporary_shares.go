package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// macOSTemporaryShares are the macOS temporary roots Docker Desktop shares by default. Lima
// shares only the home directory, so binds from /tmp or mktemp would mount empty guest folders.
var macOSTemporaryShares = []string{"/private/tmp", "/private/var/folders"}

// macOSTemporaryAliases map the unresolved paths clients send onto the shared /private roots.
var macOSTemporaryAliases = [][2]string{{"/tmp", "/private/tmp"}, {"/var/folders", "/private/var/folders"}}

func missingTemporaryShares(configuration []byte, shares []string) ([]string, error) {
	var document struct {
		Mounts []map[string]any `yaml:"mounts"`
	}
	if err := yaml.Unmarshal(configuration, &document); err != nil {
		return nil, fmt.Errorf("read owned Lima mounts: %w", err)
	}
	missing := make([]string, 0, len(shares))
	for _, share := range shares {
		present := false
		for _, mount := range document.Mounts {
			location, _ := mount["location"].(string)
			mountPoint, _ := mount["mountPoint"].(string)
			if mountPoint == "" {
				mountPoint = location
			}
			if mountPoint == share {
				present = true
				break
			}
		}
		if !present {
			missing = append(missing, share)
		}
	}
	return missing, nil
}

// temporaryShareExpression appends shares without replacing Lima's default home mount.
func temporaryShareExpression(shares []string) (string, error) {
	mounts := make([]map[string]any, 0, len(shares))
	for _, share := range shares {
		mounts = append(mounts, map[string]any{"location": share, "mountPoint": share, "writable": true})
	}
	data, err := json.Marshal(mounts)
	if err != nil {
		return "", err
	}
	return `.mounts = (.mounts // []) + ("` + base64.StdEncoding.EncodeToString(data) + `" | @base64d | from_json)`, nil
}

// shareTemporaryDirectories adds missing temporary shares to a stopped owned engine before it
// boots. A failure only logs, because the engine still works for home-directory binds.
func (m *Manager) shareTemporaryDirectories(ctx context.Context, instance string) {
	if len(m.temporaryShares) == 0 {
		return
	}
	if err := m.addTemporaryShares(ctx, instance); err != nil {
		slog.Warn("Could not share macOS temporary directories with the Porto engine", "instance", instance, "error", err)
	}
}

func (m *Manager) addTemporaryShares(ctx context.Context, instance string) error {
	output, err := m.runStdoutCommand(ctx, 20*time.Second, "inspect owned engine mount configuration", "limactl", "list", instance, "--json")
	if err != nil {
		return err
	}
	var listed struct {
		Dir string `json:"dir"`
	}
	if err := json.Unmarshal(output, &listed); err != nil || listed.Dir == "" || filepath.Base(listed.Dir) != instance {
		return errors.New("Lima did not return the owned engine's configuration directory")
	}
	configuration, err := os.ReadFile(filepath.Join(listed.Dir, "lima.yaml"))
	if err != nil {
		return err
	}
	missing, err := missingTemporaryShares(configuration, m.temporaryShares)
	if err != nil || len(missing) == 0 {
		return err
	}
	expression, err := temporaryShareExpression(missing)
	if err != nil {
		return err
	}
	_, err = m.runCommand(ctx, 30*time.Second, "share macOS temporary directories with the engine", nil,
		"limactl", "edit", "--tty=false", "--set="+expression, instance)
	return err
}

func temporaryShareSource(source string) (string, bool) {
	for _, alias := range macOSTemporaryAliases {
		if source == alias[0] || strings.HasPrefix(source, alias[0]+"/") {
			return alias[1] + strings.TrimPrefix(source, alias[0]), true
		}
	}
	for _, share := range macOSTemporaryShares {
		if source == share || strings.HasPrefix(source, share+"/") {
			return source, true
		}
	}
	return source, false
}

// translateTemporaryMounts rewrites bind sources under macOS temporary roots to their shared
// /private paths and verifies the running engine actually mounts them.
func (m *Manager) translateTemporaryMounts(ctx context.Context, binds []string, mounts []dockerStructuredMount) ([]string, []dockerStructuredMount, error) {
	if len(m.temporaryShares) == 0 {
		return binds, mounts, nil
	}
	used := false
	binds = append([]string(nil), binds...)
	for index, bind := range binds {
		parts := strings.SplitN(bind, ":", 3)
		if len(parts) < 2 || !path.IsAbs(parts[0]) {
			continue
		}
		if source, shared := temporaryShareSource(parts[0]); shared {
			parts[0], used = source, true
			binds[index] = strings.Join(parts, ":")
		}
	}
	mounts = append([]dockerStructuredMount(nil), mounts...)
	for index := range mounts {
		if !strings.EqualFold(mounts[index].Type, "bind") {
			continue
		}
		if source, shared := temporaryShareSource(mounts[index].Source); shared {
			mounts[index].Source, used = source, true
		}
	}
	if used && m.temporaryShareGuard != nil {
		if err := m.temporaryShareGuard(ctx); err != nil {
			return nil, nil, err
		}
	}
	return binds, mounts, nil
}

func (m *Manager) verifyTemporaryShares(ctx context.Context) error {
	backend, err := m.backendForCommand(ctx)
	if err != nil {
		return err
	}
	if backend.limaInstance == "" {
		return nil
	}
	arguments := []string{"shell", "--workdir=/", backend.limaInstance, "--", "sh", "-c",
		`for share in "$@"; do mountpoint -q -- "$share" || exit 1; done`, "porto-temporary-shares"}
	if _, err := m.runCommand(ctx, 10*time.Second, "verify macOS temporary directory shares", nil, "limactl", append(arguments, m.temporaryShares...)...); err != nil {
		return fmt.Errorf("%w: /tmp and $TMPDIR are not shared with the running Porto engine yet; restart it with 'porto docker engine-stop' and 'porto docker engine-start' after saving work: %w", ErrUnavailable, err)
	}
	return nil
}
