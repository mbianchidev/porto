package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestContainerInspectionAndIdentityRequestUncontaminatedStdout(t *testing.T) {
	document := []byte(`[{"Id":"synthetic-container","Name":"/synthetic-app","Config":{"Labels":{}},"State":{"Status":"running","Running":true,"Pid":42}}]`)
	captured := 0
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if command.Name != "nerdctl" || strings.Join(command.Args, " ") != "container inspect synthetic-container" {
			return nil, fmt.Errorf("unexpected synthetic inspection command")
		}
		if !command.StdoutOnly {
			return append([]byte("mesg: synthetic advisory\n"), document...), nil
		}
		captured++
		return document, nil
	}}
	manager := New(runner)
	response := httptest.NewRecorder()
	NewAPI(manager, "/synthetic/socket").ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/v1.47/containers/synthetic-container/json", nil))
	if response.Code != http.StatusOK || !json.Valid(response.Body.Bytes()) {
		t.Fatalf("Compose inspection was contaminated by diagnostics: %d: %s", response.Code, response.Body.String())
	}
	name, err := manager.ContainerName(context.Background(), "synthetic-container")
	if err != nil || name != "synthetic-app" || captured != 2 {
		t.Fatalf("container identity did not use independent structured stdout: name=%q captured=%d err=%v", name, captured, err)
	}
}

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

func TestDockerInspectNormalizesExplicitEmptyCompatibilityState(t *testing.T) {
	for _, test := range []struct {
		name  string
		state string
		want  string
	}{
		{"metadata-only", `{"Status":"","Running":false,"Pid":0,"StartedAt":"","FinishedAt":""}`, "created"},
		{"running", `{"Status":"","Running":true,"Pid":123}`, "running"},
		{"paused", `{"Status":"","Running":true,"Paused":true,"Pid":123}`, "paused"},
		{"restarting", `{"Status":"","Restarting":true}`, "restarting"},
		{"dead", `{"Status":"","Dead":true}`, "dead"},
		{"previously-started", `{"Status":"","StartedAt":"2026-01-01T00:00:00Z","ExitCode":7}`, "exited"},
		{"finished", `{"Status":"","FinishedAt":"2026-01-01T00:00:01Z"}`, "exited"},
		{"explicit-state", `{"Status":"removing","Running":false}`, "removing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := []byte(`[{"Id":"synthetic-container","Config":{"Labels":{"nerdctl/name":"synthetic-container"}},"State":` + test.state + `}]`)
			runner := &fakeRunner{outputs: map[string][]byte{"nerdctl container inspect synthetic-container": document}}
			response := httptest.NewRecorder()
			NewAPI(New(runner), "/synthetic/socket").ServeHTTP(response,
				httptest.NewRequest(http.MethodGet, "/v1.47/containers/synthetic-container/json", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("inspect failed: %d: %s", response.Code, response.Body.String())
			}
			var inspected struct {
				State map[string]json.RawMessage
			}
			if err := json.Unmarshal(response.Body.Bytes(), &inspected); err != nil {
				t.Fatal(err)
			}
			if string(inspected.State["Status"]) != `"`+test.want+`"` {
				t.Fatalf("Compose received an unusable compatibility state: %s", response.Body.String())
			}
			delete(inspected.State, "Status")
			var original map[string]json.RawMessage
			if err := json.Unmarshal([]byte(test.state), &original); err != nil {
				t.Fatal(err)
			}
			delete(original, "Status")
			for key, value := range original {
				if !bytes.Equal(value, inspected.State[key]) {
					t.Fatalf("state normalization changed %s: %s", key, response.Body.String())
				}
			}
		})
	}
}
