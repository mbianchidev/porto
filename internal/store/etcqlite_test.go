package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestEtcqliteSettingDefaultsOffAndPersistsWithoutChangingRuntimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "porto.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	settings, err := st.Settings(context.Background())
	if err != nil || settings.EtcqliteEnabled {
		t.Fatalf("Etcqlite must default off: %+v %v", settings, err)
	}
	settings.EtcqliteEnabled = true
	if err := st.SetSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	persisted, err := st.Settings(context.Background())
	if err != nil || !persisted.EtcqliteEnabled || persisted.DockerEnabled != settings.DockerEnabled || persisted.KubernetesEnabled != settings.KubernetesEnabled {
		t.Fatalf("Etcqlite setting lost persistence or changed defaults: %+v %v", persisted, err)
	}
}

func TestExistingSettingsGainDisabledEtcqliteWithoutRetargetingRuntimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings.SQLNotSoLiteEnabled, settings.KubernetesEnabled, settings.VMsEnabled = true, true, true
	if err := st.SetSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec("ALTER TABLE settings DROP COLUMN etcqlite_enabled"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	migrated, err := st.Settings(context.Background())
	if err != nil || migrated.EtcqliteEnabled || !migrated.KubernetesEnabled || !migrated.VMsEnabled || !migrated.SQLNotSoLiteEnabled {
		t.Fatalf("additive opt-in migration changed existing settings: %+v %v", migrated, err)
	}
}
