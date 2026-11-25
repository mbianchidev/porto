package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/porto-containers/porto/internal/runtimes"
)

func writeLimaPrefix(t *testing.T, prefix, contents string) string {
	t.Helper()
	for _, dir := range []string{"bin", "share/lima", "libexec/lima"} {
		if err := os.MkdirAll(filepath.Join(prefix, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(prefix, "share/lima/marker"), []byte("share"), 0o644); err != nil {
		t.Fatal(err)
	}
	limactl := filepath.Join(prefix, "bin", "limactl")
	replacement := limactl + ".new"
	if err := os.WriteFile(replacement, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, limactl); err != nil {
		t.Fatal(err)
	}
	return limactl
}

func assertLauncherMatches(t *testing.T, launcher, limactl string) {
	t.Helper()
	launcherInfo, err := os.Stat(launcher)
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(limactl)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(launcherInfo, sourceInfo) {
		t.Fatalf("launcher %s is not a hard link to %s", launcher, limactl)
	}
}

func TestEngineLauncherLinksLimactlUnderTheEngineNameAndFollowsUpdates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the engine launcher is used only on macOS; Windows symlinks need extra privileges")
	}
	root := t.TempDir()
	prefix := filepath.Join(root, "lima")
	limactl := writeLimaPrefix(t, prefix, "synthetic limactl v1")
	onPath := filepath.Join(root, "path", "limactl")
	if err := os.MkdirAll(filepath.Dir(onPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(limactl, onPath); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")

	launcher, err := prepareEngineLauncher(onPath, state)
	if err != nil {
		t.Fatal(err)
	}
	if launcher != filepath.Join(state, "bin", EngineLauncherName) {
		t.Fatalf("launcher = %s", launcher)
	}
	assertLauncherMatches(t, launcher, limactl)
	if contents, err := os.ReadFile(filepath.Join(state, "share", "lima", "marker")); err != nil || string(contents) != "share" {
		t.Fatalf("Lima share directory is not reachable beside the launcher: %q, %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(state, "libexec", "lima")); err != nil {
		t.Fatalf("Lima libexec directory is not reachable beside the launcher: %v", err)
	}

	if again, err := prepareEngineLauncher(onPath, state); err != nil || again != launcher {
		t.Fatalf("repeat preparation = %s, %v", again, err)
	}

	writeLimaPrefix(t, prefix, "synthetic limactl v2")
	if _, err := prepareEngineLauncher(onPath, state); err != nil {
		t.Fatal(err)
	}
	assertLauncherMatches(t, launcher, limactl)
	if contents, _ := os.ReadFile(launcher); string(contents) != "synthetic limactl v2" {
		t.Fatalf("launcher kept the replaced limactl: %q", contents)
	}
}

func TestEngineLauncherKeepsAnIdenticalCopyAndReplacesADifferentOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the engine launcher is used only on macOS; Windows symlinks need extra privileges")
	}
	root := t.TempDir()
	limactl := writeLimaPrefix(t, filepath.Join(root, "lima"), "synthetic limactl")
	state := filepath.Join(root, "state")
	launcher := filepath.Join(state, "bin", EngineLauncherName)
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyExecutable(limactl, launcher, 0o755); err != nil {
		t.Fatal(err)
	}
	copied, _ := os.Stat(launcher)
	if _, err := prepareEngineLauncher(limactl, state); err != nil {
		t.Fatal(err)
	}
	if current, _ := os.Stat(launcher); !os.SameFile(current, copied) {
		t.Fatal("an identical copied launcher was replaced")
	}

	if err := os.WriteFile(launcher, []byte("different limactl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareEngineLauncher(limactl, state); err != nil {
		t.Fatal(err)
	}
	assertLauncherMatches(t, launcher, limactl)
}

type recordingStartRunner struct{ names []string }

func (r *recordingStartRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	r.names = append(r.names, command.Name)
	return nil, nil
}

func TestEngineStartUsesTheNamedLauncherAndFallsBackToLimactl(t *testing.T) {
	runner := &recordingStartRunner{}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.lookPath = func(string) (string, error) { return "/synthetic/bin/limactl", nil }
	manager.engineLauncher = func(limactl string) (string, error) {
		if limactl != "/synthetic/bin/limactl" {
			t.Errorf("launcher prepared from %s", limactl)
		}
		return "/synthetic/launcher/bin/" + EngineLauncherName, nil
	}
	if _, err := manager.startLimaRuntime(context.Background(), "start synthetic engine", engineInstanceName); err != nil {
		t.Fatal(err)
	}
	manager.engineLauncher = func(string) (string, error) { return "", errors.New("synthetic launcher failure") }
	if _, err := manager.startLimaRuntime(context.Background(), "start synthetic engine", engineInstanceName); err != nil {
		t.Fatal(err)
	}
	want := []string{"/synthetic/launcher/bin/" + EngineLauncherName, "limactl"}
	if len(runner.names) != 2 || runner.names[0] != want[0] || runner.names[1] != want[1] {
		t.Fatalf("engine start commands = %v, want %v", runner.names, want)
	}
}
