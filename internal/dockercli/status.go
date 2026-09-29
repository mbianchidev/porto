package dockercli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Status struct {
	Name            string `json:"name"`
	Command         string `json:"command"`
	Supported       bool   `json:"supported"`
	Installed       bool   `json:"installed"`
	BundledExpected bool   `json:"bundledExpected"`
	Version         string `json:"version,omitempty"`
	ExpectedVersion string `json:"expectedVersion,omitempty"`
	Path            string `json:"path,omitempty"`
	Message         string `json:"message,omitempty"`
}

type commandProbe func(context.Context, string, ...string) ([]byte, error)

func Inspect(ctx context.Context, manifestPath string) []Status {
	return inspect(
		ctx,
		manifestPath,
		exec.LookPath,
		func(ctx context.Context, name string, args ...string) ([]byte, error) {
			commandContext, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			command := exec.CommandContext(commandContext, name, args...)
			command.Env = os.Environ()
			output, err := command.CombinedOutput()
			if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
				return output, context.DeadlineExceeded
			}
			return output, err
		},
	)
}

func inspect(
	ctx context.Context,
	manifestPath string,
	lookPath func(string) (string, error),
	run commandProbe,
) []Status {
	versions := readVersions(manifestPath)
	components := []struct {
		name         string
		manifestName string
		binary       string
		command      string
		args         []string
	}{
		{name: "docker", manifestName: "docker", binary: "docker", command: "docker", args: []string{"--version"}},
		{name: "compose", manifestName: "docker-compose", binary: "docker", command: "docker compose", args: []string{"compose", "version", "--short"}},
		{name: "buildx", manifestName: "docker-buildx", binary: "docker", command: "docker buildx", args: []string{"buildx", "version"}},
		{name: "dive", manifestName: "dive", binary: "dive", command: "dive", args: []string{"--version"}},
	}
	statuses := make([]Status, 0, len(components))
	for _, component := range components {
		expected := versions[component.manifestName]
		path, pathErr := lookPath(component.binary)
		status := Status{
			Name:            component.name,
			Command:         component.command,
			Supported:       !strings.HasPrefix(expected, "not available"),
			BundledExpected: expected != "" && !strings.HasPrefix(expected, "not available"),
			ExpectedVersion: versionToken(expected),
			Path:            path,
		}
		if expected != "" && !status.Supported {
			status.Message = expected
			statuses = append(statuses, status)
			continue
		}
		if pathErr != nil {
			status.Supported = true
			status.Message = component.command + " is not installed"
			statuses = append(statuses, status)
			continue
		}
		output, err := run(ctx, path, component.args...)
		status.Version = strings.TrimSpace(string(output))
		if err != nil {
			status.Message = fmt.Sprintf("%s failed: %v", component.command, err)
			if status.Version != "" {
				status.Message += ": " + status.Version
			}
			statuses = append(statuses, status)
			continue
		}
		status.Installed = true
		if status.ExpectedVersion != "" && !strings.Contains(status.Version, status.ExpectedVersion) {
			status.Message = fmt.Sprintf(
				"%s reported %q; bundled metadata expects %s",
				component.command,
				status.Version,
				status.ExpectedVersion,
			)
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func readVersions(path string) map[string]string {
	versions := map[string]string{}
	file, err := os.Open(path)
	if err != nil {
		return versions
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		name, value, ok := strings.Cut(line, " ")
		if ok && name != "" && value != "" {
			versions[name] = strings.TrimSpace(value)
		}
	}
	return versions
}

func versionToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "not available") {
		return ""
	}
	token, _, _ := strings.Cut(value, " ")
	return strings.TrimPrefix(strings.TrimSpace(token), "v")
}
