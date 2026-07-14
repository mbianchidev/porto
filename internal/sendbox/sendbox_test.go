package sendbox

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
)

type fakeResolver struct {
	path  string
	err   error
	calls int
}

func (f *fakeResolver) LookPath(string) (string, error) {
	f.calls++
	return f.path, f.err
}

func TestConfigPath(t *testing.T) {
	root := t.TempDir()
	if path, err := ConfigPath(root); err != nil || path != "" {
		t.Fatalf("missing config = %q, %v", path, err)
	}

	want := filepath.Join(root, ConfigFile)
	if err := os.WriteFile(want, []byte("name: app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, err := ConfigPath(root); err != nil || path != want {
		t.Fatalf("config = %q, %v; want %q", path, err, want)
	}
}

func TestConfigPathRejectsNonFile(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ConfigFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigPath(root); err == nil {
		t.Fatal("expected non-file error")
	}
}

func TestStatusSkipsBinaryWithoutConfiguredProjects(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("not found")}
	integration := newIntegration(resolver, "linux", "amd64", fixedNow)

	status := integration.Status([]app.Project{{Name: "app", Path: t.TempDir()}})

	if status.State != "idle" || resolver.calls != 0 {
		t.Fatalf("status = %+v, resolver calls = %d", status, resolver.calls)
	}
}

func TestStatusReportsUnsupportedPlatform(t *testing.T) {
	project := configuredProject(t)
	resolver := &fakeResolver{path: "/usr/local/bin/sendbox"}
	integration := newIntegration(resolver, "linux", "amd64", fixedNow)

	status := integration.Status([]app.Project{project})

	if status.State != "error" || !strings.Contains(status.Message, "macOS 26") {
		t.Fatalf("status = %+v", status)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d", resolver.calls)
	}
}

func TestStatusRequiresInstalledBinary(t *testing.T) {
	project := configuredProject(t)
	integration := newIntegration(&fakeResolver{err: errors.New("not found")}, "darwin", "arm64", fixedNow)

	status := integration.Status([]app.Project{project})

	if status.State != "error" || !strings.Contains(status.Message, "not installed") {
		t.Fatalf("status = %+v", status)
	}
}

func TestStatusReady(t *testing.T) {
	project := configuredProject(t)
	integration := newIntegration(&fakeResolver{path: "/usr/local/bin/sendbox"}, "darwin", "arm64", fixedNow)

	status := integration.Status([]app.Project{project})

	if status.State != "ready" || status.UpdatedAt != fixedNow().UTC() {
		t.Fatalf("status = %+v", status)
	}
}

func TestLaunchUsesConfigAndProjectPaths(t *testing.T) {
	project := configuredProject(t)
	integration := newIntegration(&fakeResolver{path: "/opt/sendbox"}, "darwin", "arm64", fixedNow)

	launch, err := integration.Launch(project)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	wantArgs := []string{"run", "--config", filepath.Join(project.Path, ConfigFile), "--project", project.Path}
	if launch.Binary != "/opt/sendbox" || launch.Dir != project.Path || !reflect.DeepEqual(launch.Args, wantArgs) {
		t.Fatalf("launch = %+v, want args %v", launch, wantArgs)
	}
}

func configuredProject(t *testing.T) app.Project {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ConfigFile), []byte("name: app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return app.Project{Name: "app", Path: root}
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC)
}
