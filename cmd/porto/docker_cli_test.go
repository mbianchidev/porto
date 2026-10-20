package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/config"
)

func TestDockerShortcutsPreserveArgumentsAndTargetPorto(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := t.TempDir()
	capture := filepath.Join(t.TempDir(), "arguments")
	script := "#!/bin/sh\nprintf '%s\\n' \"$DOCKER_HOST\" \"$DOCKER_CONTEXT\" \"$DOCKER_TLS_VERIFY\" \"$DOCKER_CERT_PATH\" \"$@\" > \"$DOCKER_CAPTURE\"\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PORTO_HOME", t.TempDir())
	t.Setenv("DOCKER_CAPTURE", capture)
	t.Setenv("DOCKER_HOST", "tcp://unrelated.example.test:2376")
	t.Setenv("DOCKER_CONTEXT", "unrelated")
	t.Setenv("DOCKER_TLS_VERIFY", "1")
	t.Setenv("DOCKER_CERT_PATH", "/synthetic/certs")
	endpoint, err := config.DockerEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"docker", "ps", "--last", "2"},
		{"docker", "exec", "-it", "demo", "sh", "-c", "printf 'two words'"},
		{"docker", "compose", "up", "--detach"},
		{"docker", "images", "--all"},
		{"docker", "volume", "ls"},
		{"docker", "container", "ls", "--all"},
		{"docker", "build", "--no-cache", "--tag", "demo", "."},
		{"docker", "cli", "ps", "--size"},
		{"ps", "--all"},
		{"exec", "-it", "demo", "sh"},
		{"compose", "config"},
		{"images", "--all"},
		{"image", "history", "demo"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if err := runCommand(args); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			forwarded := args
			if forwarded[0] == "docker" {
				forwarded = forwarded[1:]
				if forwarded[0] == "cli" {
					forwarded = forwarded[1:]
				}
			}
			want := append([]string{endpoint, "", "", ""}, forwarded...)
			got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("forwarded = %q, want %q", got, want)
			}
		})
	}
}

func TestDockerShortcutPreservesExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nexit 37\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PORTO_HOME", t.TempDir())
	var exit *exec.ExitError
	if err := dockerCmd([]string{"exec", "demo", "false"}); !errors.As(err, &exit) || exit.ExitCode() != 37 {
		t.Fatalf("Docker exit code was lost: %v", err)
	}
}
