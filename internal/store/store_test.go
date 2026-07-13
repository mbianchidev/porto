package store

import (
	"context"
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
