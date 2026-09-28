package dockercli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectReportsExplicitUnsupportedPlatform(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "VERSIONS")
	if err := os.WriteFile(manifest, []byte(
		"docker not available for windows/arm64\n"+
			"docker-compose not available because Docker CLI is not bundled for windows/arm64\n"+
			"docker-buildx not available because Docker CLI is not bundled for windows/arm64\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	statuses := inspect(
		context.Background(),
		manifest,
		func(string) (string, error) { return "", errors.New("must not be required") },
		func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("must not run")
		},
	)
	for _, status := range statuses {
		if status.Supported || status.Installed || !strings.Contains(status.Message, "windows/arm64") {
			t.Fatalf("unexpected unsupported status: %+v", status)
		}
	}
}

func TestInspectChecksBundledPluginVersions(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "VERSIONS")
	if err := os.WriteFile(manifest, []byte(
		"docker 29.7.2\n"+
			"docker-compose v5.5.1 (asset)\n"+
			"docker-buildx v0.37.1 (asset)\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	statuses := inspect(
		context.Background(),
		manifest,
		func(string) (string, error) { return "/porto/runtime/bin/docker", nil },
		func(_ context.Context, _ string, args ...string) ([]byte, error) {
			switch strings.Join(args, " ") {
			case "--version":
				return []byte("Docker version 29.7.2"), nil
			case "compose version --short":
				return []byte("5.5.1"), nil
			case "buildx version":
				return []byte("github.com/docker/buildx v0.37.1"), nil
			default:
				return nil, errors.New("unexpected command")
			}
		},
	)
	for _, status := range statuses {
		if !status.Supported || !status.Installed || status.Message != "" {
			t.Fatalf("unexpected toolchain status: %+v", status)
		}
	}
}

func TestInspectDistinguishesBrokenPluginExecutable(t *testing.T) {
	statuses := inspect(
		context.Background(),
		"",
		func(string) (string, error) { return "/porto/runtime/bin/docker", nil },
		func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "compose" {
				return []byte("permission denied"), errors.New("exit status 1")
			}
			return []byte("v1"), nil
		},
	)
	if statuses[1].Installed || !strings.Contains(statuses[1].Message, "permission denied") {
		t.Fatalf("broken Compose status = %+v", statuses[1])
	}
}
