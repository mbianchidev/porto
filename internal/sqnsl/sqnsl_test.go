package sqnsl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
	if runner.runs[2].name != filepath.Join("/tmp/bin", "sqnsl") {
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

func sqliteProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.db"), append([]byte("SQLite format 3\x00"), make([]byte, 32)...), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
