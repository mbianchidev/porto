package docker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagedContainerInspectRetainsHealthcheckForComposeReadiness(t *testing.T) {
	document, err := json.Marshal([]any{map[string]any{
		"Id": "synthetic-container",
		"Config": map[string]any{"Labels": map[string]string{
			portoManagedLabel:       portoRuntimeVersion,
			nerdctlHealthcheckLabel: `{"Test":["CMD","/fixture","--selftest"],"Interval":30000000000,"StartInterval":1000000000}`,
			nerdctlHealthStateLabel: `{"Status":"healthy","FailingStreak":0,"Log":[{"ExitCode":0,"Output":"synthetic healthy"}]}`,
		}},
		"State": map[string]any{"Running": true, "Pid": 123},
	}})
	if err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{outputs: map[string][]byte{"nerdctl container inspect synthetic-container": document}}
	for _, version := range []string{"1.43", "1.47"} {
		response := httptest.NewRecorder()
		NewAPI(New(runner), "/synthetic/socket").ServeHTTP(response,
			httptest.NewRequest(http.MethodGet, "/v"+version+"/containers/synthetic-container/json", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("managed inspection failed: %d: %s", response.Code, response.Body.String())
		}
		var inspected struct {
			Config struct {
				Healthcheck *struct {
					Test          []string
					StartInterval int64
				}
			}
			State struct {
				PID    int
				Health struct{ Status string }
			}
		}
		if err := json.Unmarshal(response.Body.Bytes(), &inspected); err != nil {
			t.Fatal(err)
		}
		if inspected.Config.Healthcheck == nil || strings.Join(inspected.Config.Healthcheck.Test, " ") != "CMD /fixture --selftest" ||
			inspected.State.Health.Status != "healthy" || inspected.State.PID != 123 {
			t.Fatalf("owned health metadata disappeared from Compose's inspect response: %s", response.Body.String())
		}
		if (inspected.Config.Healthcheck.StartInterval > 0) != (version == "1.47") {
			t.Fatalf("owned health configuration ignored Docker API version: %s", response.Body.String())
		}
	}
}

func TestManagedHealthInspectionRejectsMalformedMetadataWithoutChangingLegacyHealth(t *testing.T) {
	for _, label := range []string{`invalid-json`, `{"Test":["CMD","true"],"Timeout":-1}`} {
		document, err := json.Marshal(map[string]any{"Config": map[string]any{"Labels": map[string]string{
			portoManagedLabel: portoRuntimeVersion, nerdctlHealthcheckLabel: label,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := inspectOwnedContainerHealth(document); err == nil {
			t.Fatalf("invalid owned health metadata was exposed as valid: %s", label)
		}
	}
	legacy := json.RawMessage(`{"Config":{"Healthcheck":{"Test":["CMD","true"]}},"State":{"Health":{"Status":"healthy"}}}`)
	projected, err := inspectOwnedContainerHealth(legacy)
	if err != nil || !bytes.Equal(projected, legacy) {
		t.Fatalf("legacy runtime's health observation was overwritten: %s, %v", projected, err)
	}
}

func TestConfiguredCompatibilityHealthIsStartingBeforeItsFirstProbe(t *testing.T) {
	document, err := json.Marshal(map[string]any{
		"Config": map[string]any{"Labels": map[string]string{
			nerdctlHealthcheckLabel: `{"Test":["CMD-SHELL","/fixture --selftest"],"Interval":10000000000}`,
		}},
		"State": map[string]any{"Running": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := inspectOwnedContainerHealth(document)
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		State struct{ Health *struct{ Status string } }
	}
	if err := json.Unmarshal(output, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.State.Health == nil || observed.State.Health.Status != "starting" {
		t.Fatalf("Compose saw no healthcheck during the first-probe race: %s", output)
	}
}
