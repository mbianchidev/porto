package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestMachineReadableRuntimeQueriesKeepDiagnosticsSeparate(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		output string
		read   func(*Manager) error
	}{
		{"runtime information", `{"OSType":"linux","NCPU":2}`, func(m *Manager) error { _, err := m.BackendInfo(ctx); return err }},
		{"containers", "{\"ID\":\"synthetic\",\"Names\":\"synthetic\",\"State\":\"running\"}\n", func(m *Manager) error { _, err := m.Containers(ctx); return err }},
		{"images", "{\"ID\":\"sha256:synthetic\",\"Repository\":\"synthetic\",\"Tag\":\"one\"}\n", func(m *Manager) error { _, err := m.Images(ctx); return err }},
		{"networks", "{\"ID\":\"synthetic\",\"Name\":\"synthetic\"}\n", func(m *Manager) error { _, err := m.Networks(ctx); return err }},
		{"volumes", "{\"Name\":\"synthetic\"}\n", func(m *Manager) error { _, err := m.Volumes(ctx); return err }},
		{"statistics", "{\"ID\":\"synthetic\",\"CPUPerc\":\"1.0%\",\"MemUsage\":\"1MiB / 2MiB\",\"PIDs\":\"1\"}\n", func(m *Manager) error { _, err := m.ContainerStats(ctx); return err }},
		{"paused state", `[{"State":{"Status":"paused","Paused":true}}]`, func(m *Manager) error {
			paused, err := m.containerPaused(ctx, "synthetic")
			if err == nil && !paused {
				return fmt.Errorf("actual paused state was lost")
			}
			return err
		}},
		{"compatibility enrichment", "{\"ID\":\"synthetic\",\"Names\":\"synthetic\",\"State\":\"running\"}\n", func(m *Manager) error {
			_, err := m.containerInventoryEnricher(commandBackend{name: "nerdctl"})(ctx)
			return err
		}},
		{"runtime helper", `{"cni":true}`, func(m *Manager) error {
			runtime := &grpcContainerRuntime{runner: m.runner, helperPath: "/synthetic/helper", namespace: "default"}
			output, err := runtime.runRuntimeHelper(ctx, "probe")
			if err == nil && !json.Valid(output) {
				return fmt.Errorf("runtime helper output was contaminated")
			}
			return err
		}},
		{"guest ownership", "synthetic-owner\n", func(m *Manager) error { return m.verifyLimaOwnershipUnlocked(ctx, "synthetic-owner") }},
		{"containerd discovery", "/run/synthetic.sock\ndefault\n", func(m *Manager) error {
			socket, namespace, err := m.discoverLimaContainerd(ctx, "synthetic")
			if err == nil && (socket != "/run/synthetic.sock" || namespace != "default") {
				return fmt.Errorf("actual socket or namespace was lost")
			}
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				if !command.StdoutOnly {
					return []byte("mux_client_request_session: synthetic diagnostic\n" + test.output), nil
				}
				return []byte(test.output), nil
			}}
			if err := test.read(New(runner)); err != nil {
				t.Fatalf("structured query consumed diagnostic stderr: %v", err)
			}
		})
	}
}

func TestDockerContainerSummaryReportsCanonicalNetworkIdentityForCompose(t *testing.T) {
	networkID := strings.Repeat("a", 64)
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		arguments := strings.Join(command.Args, " ")
		switch {
		case strings.HasPrefix(arguments, "ps "):
			return []byte("{\"ID\":\"synthetic-container\",\"Names\":\"synthetic-app\",\"State\":\"running\",\"Networks\":\"synthetic-stack\"}\n"), nil
		case arguments == "network inspect synthetic-stack":
			return []byte(`[{"Name":"synthetic-stack","Id":"` + networkID + `"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected synthetic network read: %s", strings.Join(command.Args, " "))
		}
	}}
	response := httptest.NewRecorder()
	NewAPI(New(runner), "/synthetic/socket").ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/v1.47/containers/json?all=true", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("container summary failed: %d: %s", response.Code, response.Body.String())
	}
	var summaries []struct {
		NetworkSettings struct {
			Networks map[string]struct{ NetworkID string }
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &summaries); err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].NetworkSettings.Networks["synthetic-stack"].NetworkID != networkID {
		t.Fatalf("Compose would recreate an unchanged service because its actual network ID is missing or abbreviated: %s", response.Body.String())
	}
}

func TestDockerNetworkListUsesTheSameCanonicalIDAsNetworkInspection(t *testing.T) {
	id := strings.Repeat("c", 64)
	runner := &fakeRunner{outputs: map[string][]byte{
		"nerdctl network ls --format {{json .}}":  []byte(`{"Name":"synthetic-stack","ID":"` + id[:12] + `","Driver":"bridge","Labels":"com.docker.compose.project=synthetic"}` + "\n"),
		"nerdctl network inspect synthetic-stack": []byte(`[{"Name":"synthetic-stack","Id":"` + id + `"}]`),
	}}
	response := httptest.NewRecorder()
	NewAPI(New(runner), "/synthetic/socket").ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1.47/networks", nil))
	var networks []struct{ ID, Name string }
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &networks) != nil || len(networks) != 1 || networks[0].ID != id {
		t.Fatalf("Compose would compare a display-shortened network ID with a canonical container endpoint ID: %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerNetworkListFiltersBeforeInspectingCanonicalIdentities(t *testing.T) {
	id := strings.Repeat("d", 64)
	for _, filter := range []string{
		`{"label":{"com.docker.compose.network=backend":true}}`,
		`{"name":{"synthetic-backend":true}}`,
		`{"driver":{"bridge":true}}`,
	} {
		t.Run(filter, func(t *testing.T) {
			runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				switch strings.Join(command.Args, " ") {
				case "network ls --format {{json .}}":
					return []byte(`{"Name":"synthetic-removed","ID":"old","Driver":"macvlan","Labels":"com.docker.compose.network=default"}` + "\n" +
						`{"Name":"synthetic-backend","ID":"` + id[:12] + `","Driver":"bridge","Labels":"com.docker.compose.network=backend"}` + "\n"), nil
				case "network inspect synthetic-backend":
					return []byte(`[{"Name":"synthetic-backend","Id":"` + id + `"}]`), nil
				default:
					return nil, fmt.Errorf("no network found matching: synthetic-removed")
				}
			}}
			response := httptest.NewRecorder()
			NewAPI(New(runner), "/synthetic/socket").ServeHTTP(response,
				httptest.NewRequest(http.MethodGet, "/v1.47/networks?filters="+url.QueryEscape(filter), nil))
			var networks []struct{ ID, Name string }
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &networks) != nil ||
				len(networks) != 1 || networks[0].ID != id || networks[0].Name != "synthetic-backend" {
				t.Fatalf("an unrelated concurrently removed network broke the filtered list: %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDockerNetworkListRefreshesAnObservedDeletionWithoutHidingOtherFailures(t *testing.T) {
	id := strings.Repeat("e", 64)
	for _, scenario := range []string{"deleted during inspection", "still missing", "permission denied", "malformed JSON"} {
		t.Run(scenario, func(t *testing.T) {
			lists, inspections := 0, 0
			runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				switch {
				case strings.Join(command.Args, " ") == "network ls --format {{json .}}":
					lists++
					output := `{"Name":"synthetic-backend","ID":"` + id[:12] + `","Driver":"bridge"}` + "\n"
					if lists == 1 || scenario == "still missing" {
						output += `{"Name":"synthetic-removed","ID":"old","Driver":"bridge"}` + "\n"
					}
					return []byte(output), nil
				case len(command.Args) >= 3 && command.Args[0] == "network" && command.Args[1] == "inspect":
					inspections++
					if scenario == "permission denied" {
						return nil, fmt.Errorf("synthetic backend permission denied")
					}
					if scenario == "malformed JSON" {
						return []byte("invalid synthetic JSON"), nil
					}
					if inspections == 1 || scenario == "still missing" {
						return nil, fmt.Errorf("no network found matching: synthetic-removed")
					}
					return []byte(`[{"Name":"synthetic-backend","Id":"` + id + `"}]`), nil
				default:
					return nil, fmt.Errorf("unexpected synthetic network read")
				}
			}}
			response := httptest.NewRecorder()
			NewAPI(New(runner), "/synthetic/socket").ServeHTTP(response,
				httptest.NewRequest(http.MethodGet, "/v1.47/networks", nil))
			if scenario == "deleted during inspection" {
				var networks []struct{ ID, Name string }
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &networks) != nil ||
					len(networks) != 1 || networks[0].ID != id {
					t.Fatalf("network deletion did not refresh the authoritative list: %d: %s", response.Code, response.Body.String())
				}
			} else if response.Code == http.StatusOK {
				t.Fatalf("network inspection failure became a successful partial/empty list: %s", response.Body.String())
			}
			expected := 1
			if scenario == "deleted during inspection" || scenario == "still missing" {
				expected = 2
			}
			if lists != expected || inspections != expected {
				t.Fatalf("network refresh was unbounded or hid an unrelated failure: lists=%d inspections=%d want=%d", lists, inspections, expected)
			}
		})
	}
}

func TestDockerSummaryRetainsNamedVolumeIdentityAfterOCIBindLowering(t *testing.T) {
	for _, key := range []string{"nerdctl/mounts", "nerdctl/mounts.0"} {
		t.Run(key, func(t *testing.T) {
			raw := `{"Type":"volume","Name":"synthetic-data","Source":"/synthetic/volumes/data/_data","Destination":"/data","Driver":"local","RW":false}`
			if key == "nerdctl/mounts" {
				raw = "[" + raw + "]"
			}
			mounts, err := dockerContainerMounts(Container{
				Labels: map[string]string{key: raw},
				MountDetails: []ContainerMount{
					{Type: "bind", Source: "/synthetic/volumes/data/_data", Destination: "/data", Options: []string{"rbind", "ro"}},
					{Type: "bind", Source: "/synthetic/config", Destination: "/config", Options: []string{"rbind", "rw"}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(mounts) != 2 || mounts[0]["Type"] != "volume" || mounts[0]["Name"] != "synthetic-data" || mounts[0]["Source"] != "/synthetic/volumes/data/_data" || mounts[0]["RW"] != false || mounts[1]["Type"] != "bind" || mounts[1]["Name"] != "" {
				t.Fatalf("Compose would recreate an unchanged service because a named volume became an anonymous bind: %+v", mounts)
			}
		})
	}
}

func TestDockerSummaryRejectsIncompleteOrMismatchedVolumeMetadata(t *testing.T) {
	for _, labels := range []map[string]string{
		{"nerdctl/mounts": `invalid`},
		{"nerdctl/mounts.1": `{"Type":"volume","Name":"synthetic-data","Source":"/synthetic/data","Destination":"/data"}`},
		{"nerdctl/mounts.0": `invalid`},
		{"nerdctl/mounts.0": `{"Type":"volume","Name":"synthetic-data","Source":"/unrelated/data","Destination":"/data"}`},
	} {
		if _, err := dockerContainerMounts(Container{
			Labels:       labels,
			MountDetails: []ContainerMount{{Type: "bind", Source: "/synthetic/data", Destination: "/data"}},
		}); err == nil {
			t.Fatal("invalid volume metadata was accepted as an actual mounted volume")
		}
	}
}

func TestDockerContainerInspectPreservesStateAndCompletesActualNetworkIdentity(t *testing.T) {
	networkID := strings.Repeat("b", 64)
	runner := &fakeRunner{outputs: map[string][]byte{
		"nerdctl container inspect synthetic-container": []byte(`[{"Id":"synthetic-container","Config":{"Labels":{}},"State":{"Status":"running","Running":true,"Pid":42},"NetworkSettings":{"Networks":{"synthetic-stack":{"IPAddress":"10.88.0.2","Aliases":["app"]}}}}]`),
		"nerdctl network inspect synthetic-stack":       []byte(`[{"Name":"synthetic-stack","Id":"` + networkID + `"}]`),
	}}
	response := httptest.NewRecorder()
	NewAPI(New(runner), "/synthetic/socket").ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/v1.47/containers/synthetic-container/json", nil))
	var inspected struct {
		State struct {
			Running bool
			Pid     int
		}
		NetworkSettings struct {
			Networks map[string]struct {
				NetworkID, IPAddress string
				Aliases              []string
			}
		}
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &inspected) != nil {
		t.Fatalf("network-aware inspection failed: %d: %s", response.Code, response.Body.String())
	}
	endpoint := inspected.NetworkSettings.Networks["synthetic-stack"]
	if endpoint.NetworkID != networkID || endpoint.IPAddress != "10.88.0.2" || len(endpoint.Aliases) != 1 || endpoint.Aliases[0] != "app" || !inspected.State.Running || inspected.State.Pid != 42 {
		t.Fatalf("actual network completion changed unrelated container state: %s", response.Body.String())
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
