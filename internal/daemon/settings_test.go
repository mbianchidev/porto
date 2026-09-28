package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
