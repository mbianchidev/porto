package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/store"
)

func TestSettingsUpdateAppliesLogRetention(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings.LogRetentionDays = 30
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	applied := 0
	server := &Server{
		store: st,
		updateLogRetention: func(days int) error {
			applied = days
			return nil
		},
	}
	response := httptest.NewRecorder()
	server.setSettings(
		response,
		httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewReader(body)),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("settings response = %d: %s", response.Code, response.Body.String())
	}
	if applied != 30 {
		t.Fatalf("applied retention = %d, want 30", applied)
	}
	saved, err := st.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if saved.LogRetentionDays != 30 {
		t.Fatalf("saved retention = %d, want 30", saved.LogRetentionDays)
	}
}

func TestKubeconfigStorageSettingPersistsBeforeRestartAndMovesExistingClustersOnStartup(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PORTO_HOME", root)
	st, err := store.Open(filepath.Join(root, "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	server := New(st, nil)
	original, err := config.KubernetesConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	name := "synthetic-storage"
	token := config.KubernetesClusterFileToken(name)
	metadata, err := json.Marshal(kubernetes.ClusterRequest{Name: name, Provider: "k3s"})
	if err != nil {
		t.Fatal(err)
	}
	for file, data := range map[string][]byte{
		token + ".json": metadata,
		token + ".yaml": []byte(`{"current-context":"porto-k3s-synthetic-storage"}`),
	} {
		if err := os.WriteFile(filepath.Join(original, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings.KubeconfigDirectory = filepath.Join(t.TempDir(), "synthetic moved kubeconfigs")
	settings.KubeconfigActiveDirectory = "/synthetic/forged-active-folder"
	settings.KubeconfigStorageError = "synthetic forged status"
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.setSettings(response, httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("save kubeconfig folder = %d: %s", response.Code, response.Body.String())
	}
	var saved app.Settings
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.KubeconfigRestartRequired || saved.KubeconfigActiveDirectory != original {
		t.Fatalf("pending storage setting lost its active folder: %+v", saved)
	}
	if saved.KubeconfigStorageError != "" {
		t.Fatal("client-provided storage status was trusted")
	}
	if _, err := os.Stat(settings.KubeconfigDirectory); !os.IsNotExist(err) {
		t.Fatalf("save moved credentials before restart: %v", err)
	}
	reloaded, err := st.Settings(context.Background())
	if err != nil || reloaded.KubeconfigDirectory != settings.KubeconfigDirectory {
		t.Fatalf("saved storage setting = %+v, %v", reloaded, err)
	}
	if err := server.applyKubeconfigStorage(context.Background(), reloaded); err != nil {
		t.Fatal(err)
	}
	if contextName, err := server.clusters.ContextName(name); err != nil || contextName != "porto-k3s-"+name {
		t.Fatalf("relocated cluster ownership = %q, %v", contextName, err)
	}
	if active, err := config.KubernetesConfigDir(); err != nil || active != settings.KubeconfigDirectory {
		t.Fatalf("CLI and daemon storage disagree: %q, %v", active, err)
	}
	if result := server.kubeconfigSettings(reloaded); result.KubeconfigRestartRequired || result.KubeconfigStorageError != "" {
		t.Fatalf("completed storage change still pending: %+v", result)
	}
}

func TestKubeconfigStorageSettingRejectsConflictsWithoutSaving(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PORTO_HOME", root)
	st, err := store.Open(filepath.Join(root, "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(destination, "unrelated"), []byte("synthetic sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := New(st, nil)
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"relative/path", destination} {
		settings.KubeconfigDirectory = directory
		body, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.setSettings(response, httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("unsafe folder accepted: %d %s", response.Code, response.Body.String())
		}
	}
	saved, err := st.Settings(context.Background())
	if err != nil || saved.KubeconfigDirectory != "" {
		t.Fatalf("invalid folder was persisted: %+v, %v", saved, err)
	}
}

func TestKubeconfigStorageStartupFailureKeepsTheActiveFolderAndReportsTheError(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PORTO_HOME", root)
	st, err := store.Open(filepath.Join(root, "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	server := New(st, nil)
	original, err := config.KubernetesConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings.KubeconfigDirectory = t.TempDir()
	if err := os.WriteFile(filepath.Join(settings.KubeconfigDirectory, "unexpected-file"), []byte("synthetic sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.applyKubeconfigStorage(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	result := server.kubeconfigSettings(settings)
	if result.KubeconfigStorageError == "" || !result.KubeconfigRestartRequired || result.KubeconfigActiveDirectory != original {
		t.Fatalf("failed migration was disguised as success: %+v", result)
	}
}
