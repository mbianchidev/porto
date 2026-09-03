package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mbianchidev/porto/internal/app"
)

func TestSafeHostPreservesValidDomainLabels(t *testing.T) {
	for name, want := range map[string]string{
		"devoidofbeauty.com": "devoidofbeauty.com",
		"My App.Com":         "my-app.com",
		"..foo..bar..":       "foo.bar",
		"---":                "project",
	} {
		if got := safeHost(name); got != want {
			t.Fatalf("safeHost(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestRuntimeFeatureSettingsDefaultDockerOnAndRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if !settings.DockerEnabled || settings.KubernetesEnabled || settings.VMsEnabled {
		t.Fatalf("Docker must default on while optional runtimes default off: %+v", settings)
	}
	settings.KubernetesEnabled = true
	settings.VMsEnabled = true
	if err := st.SetSettings(context.Background(), settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	reloaded, err := st.Settings(context.Background())
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if !reloaded.DockerEnabled || !reloaded.KubernetesEnabled || !reloaded.VMsEnabled {
		t.Fatalf("runtime settings did not persist: %+v", reloaded)
	}
}

func TestOpenMigratesGeneratedDottedHostname(t *testing.T) {
	path := filepath.Join(t.TempDir(), "porto.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO projects(name,path,strategy,command,hostname,updated_at)
VALUES('devoidofbeauty.com','/tmp/devoidofbeauty','go','go run .','devoidofbeauty-com','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy project: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	project, err := st.GetProject(context.Background(), "devoidofbeauty.com")
	if err != nil {
		t.Fatalf("get migrated project: %v", err)
	}
	if project.Hostname != "devoidofbeauty.com" {
		t.Fatalf("hostname = %q, want dotted hostname", project.Hostname)
	}
	if project.BaseHostname != "devoidofbeauty.com" {
		t.Fatalf("base hostname = %q, want dotted hostname", project.BaseHostname)
	}
}

func TestOpenMergesCanonicalProjectPathDuplicates(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project")
	if err := os.Mkdir(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(root, "project-link")
	if err := os.Symlink(projectPath, aliasPath); err != nil {
		t.Skipf("create symlink: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "porto.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	result, err := st.db.Exec(`INSERT INTO projects(name,path,strategy,command,pinned_port,hostname,auto_start,updated_at)
VALUES('project',?,'go','go run .',42000,'custom-project',1,'2026-01-01T00:00:00Z')`, aliasPath)
	if err != nil {
		t.Fatalf("insert alias project: %v", err)
	}
	aliasID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("alias id: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO projects(name,path,strategy,command,hostname,updated_at)
VALUES('project-copy',?,'go','go run .','project-copy','2026-01-01T00:00:00Z')`, projectPath); err != nil {
		t.Fatalf("insert canonical project: %v", err)
	}
	if err := st.AddLog(context.Background(), aliasID, "stdout", "preserved"); err != nil {
		t.Fatalf("add alias log: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	st, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st.Close()
	projects, err := st.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	canonicalPath, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		t.Fatalf("canonicalize project: %v", err)
	}
	if len(projects) != 1 || projects[0].Path != canonicalPath {
		t.Fatalf("projects = %+v, want one canonical project", projects)
	}
	if projects[0].Hostname != "custom-project" || projects[0].PinnedPort != 42000 || !projects[0].AutoStart {
		t.Fatalf("merged project lost configuration: %+v", projects[0])
	}
	logs, err := st.Logs(context.Background(), projects[0].ID, 10)
	if err != nil {
		t.Fatalf("list logs: %v", err)
	}
	if len(logs) != 1 || logs[0].Line != "preserved" {
		t.Fatalf("logs = %+v, want preserved alias log", logs)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	defer st.Close()

	defaults, err := st.Settings(context.Background())
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if defaults.CleanupLocalMerged || defaults.CleanupRemoteMerged || !defaults.PruneRemoteTracking {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}

	want := app.Settings{
		CleanupLocalMerged:  true,
		CleanupRemoteMerged: true,
		PruneRemoteTracking: false,
		ProtectedBranches:   []string{"main", "release/*"},
		SQLNotSoLiteEnabled: true,
		KillSwitchEnabled:   true,
		SendboxEnabled:      true,
	}
	if err := st.SetSettings(context.Background(), want); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	got, err := st.Settings(context.Background())
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
}

func TestOpenMigratesLegacySettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "porto.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = db.Exec(`CREATE TABLE settings (
 id INTEGER PRIMARY KEY CHECK (id = 1),
 cleanup_local_merged INTEGER NOT NULL DEFAULT 0,
 cleanup_remote_merged INTEGER NOT NULL DEFAULT 0,
 prune_remote_tracking INTEGER NOT NULL DEFAULT 1,
 protected_branches TEXT NOT NULL
);
INSERT INTO settings(id, protected_branches) VALUES(1, '["main"]');`)
	if err != nil {
		t.Fatalf("create legacy settings: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	defer st.Close()
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatalf("load migrated settings: %v", err)
	}
	if settings.SQLNotSoLiteEnabled {
		t.Fatal("sql-not-so-lite integration should default to disabled")
	}
	if settings.SendboxEnabled {
		t.Fatal("Sendbox integration should default to disabled")
	}
	if settings.KillSwitchEnabled {
		t.Fatal("KillSwitch integration should default to disabled")
	}
}

func TestListProjectsReturnsEmptySlice(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	projects, err := st.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if projects == nil || len(projects) != 0 {
		t.Fatalf("projects = %#v, want non-nil empty slice", projects)
	}
}

func TestKubernetesRoutePersistsStableHostname(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	route := KubernetesRoute{
		Context:     "porto-dev",
		Namespace:   "default",
		Service:     "api",
		ServicePort: 8080,
		Hostname:    "api-8080.default.dev",
	}
	if err := st.UpsertKubernetesRoute(context.Background(), route); err != nil {
		t.Fatalf("upsert Kubernetes route: %v", err)
	}
	got, err := st.GetKubernetesRouteByHostname(context.Background(), route.Hostname)
	if err != nil {
		t.Fatalf("get Kubernetes route: %v", err)
	}
	if got != route {
		t.Fatalf("route = %+v, want %+v", got, route)
	}
}

func TestProjectInstancesPreserveMetadataAndUniqueHostnames(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	source := filepath.Join(t.TempDir(), "source")
	instancePath := filepath.Join(t.TempDir(), "instance")
	baseID, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "app",
		Path:     source,
		Strategy: "package",
		Command:  "npm run dev",
		Hostname: "app",
	})
	if err != nil {
		t.Fatal(err)
	}
	instanceID, err := st.UpsertProject(context.Background(), app.Project{
		Name:            "app",
		Path:            instancePath,
		SourcePath:      source,
		Strategy:        "package",
		Command:         "npm run dev",
		Hostname:        "app-feature",
		BaseHostname:    "app",
		ManagedInstance: true,
		Branch:          "feature/work",
	})
	if err != nil {
		t.Fatal(err)
	}
	if baseID == instanceID {
		t.Fatal("instance reused base project ID")
	}
	instance, err := st.GetProjectByID(context.Background(), instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if !instance.ManagedInstance || instance.SourcePath != canonicalProjectPath(source) || instance.Branch != "feature/work" {
		t.Fatalf("instance metadata = %+v", instance)
	}
	if _, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "other",
		Path:     filepath.Join(t.TempDir(), "other"),
		Strategy: "go",
		Command:  "go run .",
		Hostname: "app-feature",
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := st.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, project := range projects {
		if seen[project.Hostname] {
			t.Fatalf("duplicate hostname %q", project.Hostname)
		}
		seen[project.Hostname] = true
	}
}

func TestLogFilteringAndClearing(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	id, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "app",
		Path:     t.TempDir(),
		Strategy: "package",
		Command:  "npm run dev",
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	for _, entry := range []struct {
		stream string
		line   string
	}{
		{stream: "stdout", line: "ready"},
		{stream: "stderr", line: "warning"},
		{stream: "system", line: "started"},
	} {
		if err := st.AddLog(context.Background(), id, entry.stream, entry.line); err != nil {
			t.Fatalf("add %s log: %v", entry.stream, err)
		}
	}

	stdout, err := st.LogsByStream(context.Background(), id, "stdout", 200)
	if err != nil {
		t.Fatalf("load stdout logs: %v", err)
	}
	if len(stdout) != 1 || stdout[0].Stream != "stdout" || stdout[0].Line != "ready" {
		t.Fatalf("stdout logs = %+v", stdout)
	}
	deleted, err := st.ClearLogs(context.Background(), id, "stderr")
	if err != nil {
		t.Fatalf("clear stderr logs: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	all, err := st.Logs(context.Background(), id, 200)
	if err != nil {
		t.Fatalf("load remaining logs: %v", err)
	}
	if len(all) != 2 || all[0].Stream != "stdout" || all[1].Stream != "system" {
		t.Fatalf("remaining logs = %+v", all)
	}
	if _, err := st.ClearLogs(context.Background(), id, ""); err != nil {
		t.Fatalf("clear all logs: %v", err)
	}
	empty, err := st.Logs(context.Background(), id, 200)
	if err != nil {
		t.Fatalf("load empty logs: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty logs = %#v, want non-nil empty slice", empty)
	}
}
