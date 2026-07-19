package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mbianchidev/porto/internal/app"
)

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
