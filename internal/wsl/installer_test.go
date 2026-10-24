//go:build !windows

package wsl

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type installerRunner struct {
	home, name string
	extraEnv   []string
	calls      []runtimes.Command
}

func (r *installerRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	r.calls = append(r.calls, command)
	if len(command.Args) < 7 || command.Args[1] != r.name {
		return nil, errors.New("attempted to mutate a non-opted-in distribution")
	}
	if command.Args[5] == probeScript {
		return []byte(strings.Join([]string{"x86_64", r.home, "/mnt/c/Porto's caf\u00e9/porto.exe",
			r.home, `\\wsl.localhost\synthetic\home\dev`, "/mnt/c/Users/Synthetic User", `C:\Users\Synthetic User`, ""}, "\x00")), nil
	}
	args := append([]string{"-c", command.Args[5], "porto-wsl"}, command.Args[7:]...)
	child := exec.CommandContext(ctx, "sh", args...)
	child.Env = process.WithEnvironment(os.Environ(), append([]string{"HOME=" + r.home}, r.extraEnv...)...)
	child.Stdin = command.StdinReader
	return child.CombinedOutput()
}

func installerFixture(t *testing.T) (*Manager, *installerRunner) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home with quote's-\u03b4")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &installerRunner{home: home, name: "Synthetic Ubuntu's \u03b4"}
	manager := New(filepath.Join(root, "state"), runner)
	manager.platform = "windows"
	manager.runtimeDir = filepath.Join(root, "runtime")
	manager.executable = func() (string, error) { return `C:\Porto's caf` + "\u00e9" + `\porto.exe`, nil }
	manager.home = func() (string, error) { return `C:\Users\Synthetic User`, nil }
	manager.discover = func(context.Context) ([]Distribution, error) {
		return []Distribution{{Name: runner.name, ID: "synthetic-owned-distribution", Version: 2, Running: true},
			{Name: "Not selected", ID: "synthetic-other-distribution", Version: 2}}, nil
	}
	for _, name := range []string{"bin/docker", "bin/porto-wsl", "docker/cli-plugins/docker-compose", "docker/cli-plugins/docker-buildx"} {
		path := filepath.Join(manager.runtimeDir, "amd64", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' synthetic-tool\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return manager, runner
}

func TestWSLInstallRepairAndRemovalPreserveUserDockerConfiguration(t *testing.T) {
	manager, runner := installerFixture(t)
	original := []byte("# existing user profile\n")
	if err := os.WriteFile(filepath.Join(runner.home, ".profile"), original, 0o600); err != nil {
		t.Fatal(err)
	}
	dockerConfig := filepath.Join(runner.home, ".docker", "config.json")
	if err := os.MkdirAll(filepath.Dir(dockerConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	userConfig := []byte(`{"currentContext":"synthetic-other-engine","syntheticPreserve":true}`)
	if err := os.WriteFile(dockerConfig, userConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := manager.Enable(context.Background(), runner.name, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Enable(context.Background(), runner.name, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Owner != second.Owner {
		t.Fatal("repair replaced the integration identity")
	}
	windowsHomeShared := false
	for _, share := range second.Shares {
		if share.LinuxPath == "/mnt/c/Users/Synthetic User" {
			windowsHomeShared = true
			if share.WindowsPath != `C:\Users\Synthetic User` {
				t.Fatalf("Windows drive was routed back through the WSL network provider: %q", share.WindowsPath)
			}
		}
	}
	if !windowsHomeShared {
		t.Fatal("selected Windows home was not shared")
	}
	profile, err := os.ReadFile(filepath.Join(runner.home, ".profile"))
	if err != nil || strings.Count(string(profile), "# Porto WSL "+first.Owner) != 1 {
		t.Fatalf("repair duplicated shell hooks: %s, %v", profile, err)
	}
	if err := manager.Disable(context.Background(), runner.name); err != nil {
		t.Fatal(err)
	}
	preserved, err := os.ReadFile(dockerConfig)
	if err != nil || string(preserved) != string(userConfig) {
		t.Fatalf("unrelated Docker config changed: %s, %v", preserved, err)
	}
	profile, err = os.ReadFile(filepath.Join(runner.home, ".profile"))
	if err != nil || strings.Contains(string(profile), "# Porto WSL ") || !strings.Contains(string(profile), string(original)) {
		t.Fatalf("user profile was not preserved after removal: %s, %v", profile, err)
	}
	for _, command := range runner.calls {
		if command.Args[1] != runner.name {
			t.Fatal("another distribution was executed")
		}
	}
}

func TestWSLPartialInstallRollsBackPublishedTools(t *testing.T) {
	manager, runner := installerFixture(t)
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	failed := filepath.Join(t.TempDir(), "failed-once")
	script := `#!/bin/sh
set -eu
/bin/mv "$@"
for destination do :; done
if [ "$destination" = "$PORTO_TEST_MV_DESTINATION" ] && [ ! -e "$PORTO_TEST_MV_FAILED" ]; then
  : > "$PORTO_TEST_MV_FAILED"
  exit 27
fi
`
	if err := os.WriteFile(filepath.Join(fakeBin, "mv"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	toolRoot := filepath.Join(runner.home, ".local", "share", "porto", "wsl")
	runner.extraEnv = []string{"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PORTO_TEST_MV_DESTINATION=" + toolRoot, "PORTO_TEST_MV_FAILED=" + failed}
	if _, err := manager.Enable(context.Background(), runner.name, nil); err == nil {
		t.Fatal("partial publication failure was hidden")
	}
	if _, err := os.Stat(toolRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed installation left published tooling: %v", err)
	}
	state, err := manager.state()
	if err != nil || len(state) != 0 {
		t.Fatalf("failed installation stayed opted in: %+v, %v", state, err)
	}
}
