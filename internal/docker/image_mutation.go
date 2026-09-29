package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const imageTransferTimeout = 30 * time.Minute

func (m *Manager) TagImage(ctx context.Context, source, target string) error {
	for label, value := range map[string]string{"source": source, "target": target} {
		if err := validateObjectID(value); err != nil {
			return fmt.Errorf("image tag %s: %w", label, err)
		}
	}
	_, err := m.run(ctx, "tag Porto image", "tag", normalizeNerdctlReference(source), normalizeNerdctlReference(target))
	return err
}

func (m *Manager) StreamLoadImages(
	ctx context.Context,
	archive io.Reader,
	quiet bool,
	emit func(runtimes.OutputChunk) error,
) error {
	if archive == nil {
		return errors.New("image archive is required")
	}
	args := []string{"load"}
	if quiet {
		args = append(args, "--quiet")
	}
	return m.runStreamingReader(ctx, imageTransferTimeout, "load Porto images", archive, emit, args...)
}

func (m *Manager) StreamImportImage(
	ctx context.Context,
	archive io.Reader,
	reference string,
	platform string,
	message string,
	emit func(runtimes.OutputChunk) error,
) error {
	if archive == nil {
		return errors.New("root filesystem archive is required")
	}
	if err := validateObjectID(reference); err != nil {
		return err
	}
	if strings.ContainsAny(message, "\r\n\x00") {
		return errors.New("invalid image import message")
	}
	args := []string{"image", "import"}
	args = appendStringFlag(args, "--platform", platform)
	args = appendStringFlag(args, "--message", message)
	args = append(args, "-", normalizeNerdctlReference(reference))
	return m.runStreamingReader(ctx, imageTransferTimeout, "import Porto image", archive, emit, args...)
}

func (m *Manager) StreamPushImageWithAuth(
	ctx context.Context,
	reference string,
	platform string,
	registryAuth *RegistryAuth,
	emit func(runtimes.OutputChunk) error,
) error {
	if err := validateObjectID(reference); err != nil {
		return err
	}
	normalized := normalizeNerdctlReference(reference)
	if registryAuth == nil {
		var err error
		registryAuth, err = m.resolveRegistryAuth(ctx, normalized)
		if err != nil {
			return fmt.Errorf("resolve registry credentials for %q: %w", normalized, err)
		}
	}
	args := []string{"push"}
	args = appendStringFlag(args, "--platform", platform)
	args = append(args, normalized)
	config, authenticated, err := registryDockerConfig(normalized, registryAuth)
	if err != nil {
		return err
	}
	if !authenticated {
		return m.runStreaming(ctx, imageTransferTimeout, "push Porto image", nil, emit, args...)
	}
	backend, err := m.backend(ctx)
	if err != nil {
		return err
	}
	return m.runBackendStreamingWithDockerConfig(
		ctx,
		backend,
		imageTransferTimeout,
		"push Porto image",
		config,
		emit,
		args...,
	)
}

func (m *Manager) runBackendStreamingWithDockerConfig(
	ctx context.Context,
	backend commandBackend,
	timeout time.Duration,
	action string,
	config []byte,
	emit func(runtimes.OutputChunk) error,
	args ...string,
) (err error) {
	if backend.limaInstance != "" {
		const script = `set -eu
config_dir="$(mktemp -d)"
trap 'rm -rf "$config_dir"' EXIT
trap 'exit 1' HUP INT TERM
umask 077
cat > "$config_dir/config.json"
DOCKER_CONFIG="$config_dir" "$@"
`
		commandArgs := []string{
			"shell", backend.limaInstance, "--",
			"sh", "-c", script, "porto-registry-auth", "nerdctl",
		}
		commandArgs = append(commandArgs, args...)
		return m.runStreamingCommand(ctx, timeout, action, runtimes.Command{
			Name: backend.name, Args: commandArgs, Stdin: config,
		}, emit)
	}
	configDir, err := os.MkdirTemp("", "porto-registry-auth-*")
	if err != nil {
		return fmt.Errorf("create temporary registry auth directory: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(configDir); removeErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary registry auth directory: %w", removeErr))
		}
	}()
	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		return fmt.Errorf("write temporary registry auth config: %w", err)
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		return fmt.Errorf("protect temporary registry auth config: %w", err)
	}
	return m.runStreamingCommand(ctx, timeout, action, runtimes.Command{
		Name: backend.name,
		Args: append(append([]string(nil), backend.prefix...), args...),
		Env:  []string{"DOCKER_CONFIG=" + configDir},
	}, emit)
}
