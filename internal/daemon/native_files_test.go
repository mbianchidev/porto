package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mbianchidev/porto/internal/nativefiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestNativeCapabilitiesNeverProbeDisabledRuntimes(t *testing.T) {
	server, _ := cleanupServerFixture(t)
	settings, err := server.store.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings.DockerEnabled, settings.VMsEnabled = false, false
	if err := server.store.SetSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	probes := 0
	server.nativeFiles = nativefiles.New(t.TempDir(), runtimeRunnerFunc(func(context.Context, runtimes.Command) ([]byte, error) {
		probes++
		return nil, errors.New("disabled runtime must not be probed")
	}))
	for _, kind := range []string{"container", "image", "volume", "vm", "host"} {
		response := httptest.NewRecorder()
		server.nativeFileCapabilities(response, httptest.NewRequest(http.MethodGet, "/api/files/capabilities?kind="+kind+"&name=synthetic", nil))
		if kind == "host" {
			if response.Code != http.StatusBadRequest {
				t.Fatalf("unrelated host access was accepted: %d %s", response.Code, response.Body.String())
			}
			continue
		}
		var capability nativefiles.Capability
		if err := json.Unmarshal(response.Body.Bytes(), &capability); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || capability.Supported || capability.Message == "" || capability.Fallback == "" || probes != 0 {
			t.Fatalf("disabled runtime received success-shaped capability: %+v, probes=%d", capability, probes)
		}
	}
}
