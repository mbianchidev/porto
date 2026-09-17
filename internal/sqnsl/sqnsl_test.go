package sqnsl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
)

type fakeRunner struct {
	paths map[string]string
	runs  []fakeRun
	run   func(name string, args []string) ([]byte, error)
}

type fakeRun struct {
	name string
	args []string
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	path, ok := f.paths[name]
	if !ok {
		return "", errors.New("not found")
	}
	return path, nil
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.runs = append(f.runs, fakeRun{name: name, args: append([]string(nil), args...)})
	if f.run != nil {
		return f.run(name, args)
	}
	return nil, nil
}

func TestHasSQLiteDatabaseValidatesHeader(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fake.db"), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.sqlite"), append([]byte("SQLite format 3\x00"), make([]byte, 32)...), 0o600); err != nil {
		t.Fatal(err)
	}

	found, err := HasSQLiteDatabase(root)
	if err != nil {
		t.Fatalf("detect SQLite: %v", err)
	}
	if !found {
		t.Fatal("expected a valid SQLite database")
	}
}

func TestSyncSkipsInstallWithoutSQLite(t *testing.T) {
	runner := &fakeRunner{paths: map[string]string{}}
	manager := NewManager(runner)

	result, err := manager.Sync(context.Background(), []app.Project{{Name: "app", Path: t.TempDir()}})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(result.ProjectPaths) != 0 || len(runner.runs) != 0 {
		t.Fatalf("unexpected work: result=%+v runs=%+v", result, runner.runs)
	}
}

func TestSyncUsesInstalledBinary(t *testing.T) {
	root := sqliteProject(t)
	runner := &fakeRunner{
		paths: map[string]string{"sqnsl": "/usr/local/bin/sqnsl"},
		run: func(_ string, _ []string) ([]byte, error) {
			return []byte("Found 1 database"), nil
		},
	}
	manager := NewManager(runner)

	result, err := manager.Sync(context.Background(), []app.Project{{Name: "app", Path: root}})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	wantRun := fakeRun{name: "/usr/local/bin/sqnsl", args: []string{"scan", root}}
	if !reflect.DeepEqual(runner.runs, []fakeRun{wantRun}) {
		t.Fatalf("runs = %+v, want %+v", runner.runs, []fakeRun{wantRun})
	}
	if result.Output != "Found 1 database" {
		t.Fatalf("output = %q", result.Output)
	}
}

func TestSyncInstallsPinnedRevision(t *testing.T) {
	root := sqliteProject(t)
	runner := &fakeRunner{
		paths: map[string]string{"go": "/usr/bin/go"},
		run: func(_ string, args []string) ([]byte, error) {
			if reflect.DeepEqual(args, []string{"env", "GOBIN"}) {
				return []byte("/tmp/bin\n"), nil
			}
			return nil, nil
		},
	}
	manager := NewManager(runner)

	if _, err := manager.Sync(context.Background(), []app.Project{{Name: "app", Path: root}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(runner.runs) != 3 {
		t.Fatalf("runs = %+v", runner.runs)
	}
	if got := strings.Join(runner.runs[0].args, " "); got != "install "+installTarget {
		t.Fatalf("install args = %q", got)
	}
	if runner.runs[2].name != filepath.Join("/tmp/bin", binaryName()) {
		t.Fatalf("sqnsl path = %q", runner.runs[2].name)
	}
}

func TestSyncReportsScanFailure(t *testing.T) {
	root := sqliteProject(t)
	runner := &fakeRunner{
		paths: map[string]string{"sqnsl": "sqnsl"},
		run: func(_ string, _ []string) ([]byte, error) {
			return []byte("catalog locked"), errors.New("exit status 1")
		},
	}

	_, err := NewManager(runner).Sync(context.Background(), []app.Project{{Name: "app", Path: root}})
	if err == nil || !strings.Contains(err.Error(), "catalog locked") {
		t.Fatalf("error = %v", err)
	}
}

func TestSyncSkipsMissingProjectsWithoutLosingHealthyDatabases(t *testing.T) {
	missing := app.Project{Name: "build", Path: filepath.Join(t.TempDir(), "removed-worktree", "build")}
	healthy := app.Project{Name: "healthy", Path: sqliteProject(t)}
	for _, projects := range [][]app.Project{{missing, healthy}, {healthy, missing}} {
		runner := &fakeRunner{paths: map[string]string{"sqnsl": "sqnsl"}}
		result, err := NewManager(runner).Sync(context.Background(), projects)
		if err != nil {
			t.Fatalf("stale project aborted discovery: %v", err)
		}
		if !reflect.DeepEqual(result.ProjectPaths, []string{healthy.Path}) ||
			!reflect.DeepEqual(runner.runs, []fakeRun{{name: "sqnsl", args: []string{"scan", healthy.Path}}}) {
			t.Fatalf("healthy database was not scanned: result=%+v, runs=%+v", result, runner.runs)
		}
	}
}

func TestStartWithOnlyMissingProjectsStaysIdle(t *testing.T) {
	runner := &fakeRunner{paths: map[string]string{}}
	manager := NewManager(runner)
	done := make(chan error, 1)
	if !manager.Start([]app.Project{{Name: "removed", Path: filepath.Join(t.TempDir(), "missing")}}, func(_ Result, err error) {
		done <- err
	}) {
		t.Fatal("scan did not start")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("missing projects should not fail the integration: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish")
	}
	if status := manager.Status(); status.State != "idle" || len(runner.runs) != 0 {
		t.Fatalf("missing projects triggered external work or an error state: %+v, runs=%+v", status, runner.runs)
	}
}

func TestHasSQLiteDatabaseContinuesPastMissingCandidate(t *testing.T) {
	root := sqliteProject(t)
	if err := os.Symlink(filepath.Join(root, "removed.db"), filepath.Join(root, "0-stale.db")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation requires privileges: %v", err)
		}
		t.Fatal(err)
	}
	found, err := HasSQLiteDatabase(root)
	if err != nil || !found {
		t.Fatalf("missing database candidate hid a valid database: found=%t, error=%v", found, err)
	}
}

func TestHasSQLiteDatabaseStillRejectsEmptyRoots(t *testing.T) {
	if _, err := HasSQLiteDatabase(""); err == nil {
		t.Fatal("empty project root was accepted")
	}
}

func TestSQLiteDiscoveryPreservesOtherIOErrors(t *testing.T) {
	for _, cause := range []error{os.ErrPermission, errors.New("synthetic I/O failure")} {
		scanErr := &os.PathError{Op: "open", Path: "synthetic.db", Err: cause}
		if err := sqliteDiscoveryError(scanErr); !errors.Is(err, cause) {
			t.Fatalf("discovery hid a non-missing-path error: got %v, want %v", err, cause)
		}
	}
}

func TestHasSQLiteDatabaseReportsUnreadableFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not Windows ACLs")
	}
	root := sqliteProject(t)
	path := filepath.Join(root, "app.db")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err == nil {
		_ = file.Close()
		t.Skip("the current user can bypass file permissions")
	}
	if _, err := HasSQLiteDatabase(root); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("unreadable database error = %v, want permission error", err)
	}
}

func sqliteProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.db"), append([]byte("SQLite format 3\x00"), make([]byte, 32)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
