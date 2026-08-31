package docker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const buildTimeout = 2 * time.Hour

func (m *Manager) Build(ctx context.Context, request BuildRequest) ([]byte, error) {
	var output []byte
	err := m.StreamBuild(ctx, request, func(chunk runtimes.OutputChunk) error {
		output = append(output, chunk.Data...)
		return nil
	})
	return output, err
}

func (m *Manager) StreamBuild(
	ctx context.Context,
	request BuildRequest,
	emit func(runtimes.OutputChunk) error,
) error {
	if err := validateBuildRequest(request); err != nil {
		return err
	}
	args := []string{"build", "--progress", "plain"}
	args = appendStringFlag(args, "--file", request.Dockerfile)
	for _, tag := range request.Tags {
		if err := validateObjectID(tag); err != nil {
			return fmt.Errorf("image tag: %w", err)
		}
		args = append(args, "--tag", tag)
	}
	if request.Tag != "" {
		if err := validateObjectID(request.Tag); err != nil {
			return fmt.Errorf("image tag: %w", err)
		}
		args = append(args, "--tag", request.Tag)
	}
	args = appendStringFlag(args, "--target", request.Target)
	args = appendStringFlag(args, "--platform", request.Platform)
	args = appendStringFlag(args, "--network", request.Network)
	if request.NoCache {
		args = append(args, "--no-cache")
	}
	if request.Pull {
		args = append(args, "--pull")
	}
	for _, key := range sortedBuildArgumentKeys(request.BuildArgs) {
		value := key
		if request.BuildArgs[key] != nil {
			value += "=" + *request.BuildArgs[key]
		}
		args = append(args, "--build-arg", value)
	}
	for _, key := range sortedStringKeys(request.Labels) {
		args = append(args, "--label", key+"="+request.Labels[key])
	}
	for _, source := range request.CacheFrom {
		args = append(args, "--cache-from", source)
	}
	contextArgument := request.Context
	if request.ContextReader != nil {
		contextArgument = "-"
	}
	args = append(args, contextArgument)
	if request.ContextReader != nil {
		return m.runStreamingReader(ctx, buildTimeout, "build Porto image", request.ContextReader, emit, args...)
	}
	return m.runStreaming(ctx, buildTimeout, "build Porto image", nil, emit, args...)
}

func validateBuildRequest(request BuildRequest) error {
	if strings.TrimSpace(request.Context) == "" && request.ContextReader == nil {
		return errors.New("build context is required")
	}
	dockerfile := request.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if filepath.IsAbs(dockerfile) || filepath.VolumeName(dockerfile) != "" {
		return errors.New("Dockerfile path must be relative to the build context")
	}
	cleanDockerfile := filepath.Clean(dockerfile)
	if cleanDockerfile == "." || cleanDockerfile == ".." || strings.HasPrefix(cleanDockerfile, ".."+string(filepath.Separator)) {
		return errors.New("Dockerfile path must stay within the build context")
	}
	if err := validateBuildPlatforms(request.Platform); err != nil {
		return err
	}
	return nil
}

func validateBuildPlatforms(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	for _, platform := range strings.Split(value, ",") {
		parts := strings.Split(strings.TrimSpace(platform), "/")
		if len(parts) < 2 || len(parts) > 3 || parts[0] != "linux" {
			return fmt.Errorf("invalid or unsupported build platform %q", platform)
		}
		for _, part := range parts {
			if !validPlatformComponent(part) {
				return fmt.Errorf("invalid build platform %q", platform)
			}
		}
	}
	return nil
}

func validPlatformComponent(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func sortedBuildArgumentKeys(values map[string]*string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
