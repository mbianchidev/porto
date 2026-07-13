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
		t.Fatal("integration should default to disabled")
	}
}
