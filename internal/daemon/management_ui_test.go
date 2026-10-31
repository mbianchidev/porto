package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/extensions"
)

func TestExtensionUIUsesStaticOpaqueSandboxAndVerifiedGeneration(t *testing.T) {
	server, handler, _ := managementServer(t)
	module := []byte{0, 97, 115, 109, 1, 0, 0, 0, 1, 4, 1, 96, 0, 0, 3, 2, 1, 0, 7, 10, 1, 6, 95, 115, 116, 97, 114, 116, 0, 0, 10, 4, 1, 2, 0, 11}
	p := extensions.Package{Content: extensions.Content{
		Manifest: extensions.Manifest{APIVersion: extensions.APIVersion, ID: "dev.synthetic.ui", Name: "Synthetic view", Version: "1.0.0", Permissions: []string{}, Views: []extensions.View{{ID: "view", Title: "Synthetic", Location: "tools"}}, Actions: []extensions.Action{{ID: "noop", Title: "Synthetic action"}}},
		Module:   module, UI: `<!doctype html><p>Synthetic view</p><script>fetch("/api/registries")</script>`,
	}}
	p.SHA256 = extensions.Digest(p.Content)
	value, err := server.extensions.Install(extensions.InstallRequest{Package: p, ExpectedSHA256: p.SHA256, Developer: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.extensions.Configure(value.ID, extensions.Configuration{Enabled: true, Approve: true}); err != nil {
		t.Fatal(err)
	}
	generation := server.extensions.List()[0].Generation
	base := "/api/extensions/" + value.ID + "/ui?generation="
	response := managementCall(handler, "GET", base+strconv.FormatUint(generation, 10), nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Security-Policy"), "script-src 'none'") || !strings.Contains(response.Header().Get("Content-Security-Policy"), "; sandbox;") {
		t.Fatalf("extension isolation policy: %d %v", response.Code, response.Header())
	}
	if response := managementCall(handler, "GET", base+strconv.FormatUint(generation-1, 10), nil); response.Code != 403 {
		t.Fatal("stale UI handle was accepted")
	}
	p.Content.UI += "synthetic altered artifact"
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(server.store.Directory(), "extensions", "packages", p.SHA256+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if response := managementCall(handler, "GET", base+strconv.FormatUint(generation, 10), nil); response.Code != 403 {
		t.Fatal("altered artifact served unverified UI")
	}
	if _, err := server.extensions.Invoke(context.Background(), value.ID, extensions.Invocation{Action: "noop", EnvironmentID: "local"}); err == nil {
		t.Fatal("altered artifact executed")
	}
}

func TestHeadlessUnknownAPIReportsUnsupportedInsteadOfDashboardSuccess(t *testing.T) {
	_, handler, _ := managementServer(t)
	response := managementCall(handler, "GET", "/api/docker/unsupported-synthetic-feature", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unsupported API returned successful dashboard: %d %s", response.Code, response.Body)
	}
}
