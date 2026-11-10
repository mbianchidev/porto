package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	"github.com/mbianchidev/porto/internal/runtimes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestContainerIdentityUsesLiveMatchingInventoryMetadataWithoutCLIForks(t *testing.T) {
	client := &fakeContainersClient{container: &containersapi.Container{
		ID: "synthetic-container", Labels: map[string]string{nerdctlNameLabel: "current-live-name"},
	}}
	manager := NewWithStateDir(&fakeRunner{handler: func(runtimes.Command) ([]byte, error) {
		t.Fatal("live identity inspection forked a CLI instead of using the owned inventory connection")
		return nil, nil
	}}, t.TempDir())
	if err := manager.writeEngineState(engineState{Mode: "lima", Instance: engineInstanceName, OwnerID: "synthetic-owner"}); err != nil {
		t.Fatal(err)
	}
	runtimeClient := &grpcContainerRuntime{
		containers: client, namespace: "default",
		commandSource: commandBackend{name: "limactl", limaInstance: engineInstanceName, ownerID: "synthetic-owner"},
	}
	manager.inventoryCancel = func() {}
	manager.inventory = &containerInventory{
		snapshot:      ContainerSnapshot{Available: true, Containers: []Container{{ID: "synthetic-container", Name: "stale-snapshot-name"}}},
		runtimeClient: runtimeClient,
	}
	for _, identifier := range []string{"", "-all", "synthetic\ncontainer"} {
		if _, _, err := manager.resolveContainerIdentity(context.Background(), identifier); err == nil {
			t.Fatalf("invalid identity %q was accepted", identifier)
		}
	}
	if len(client.calls) != 0 {
		t.Fatal("invalid identity reached containerd")
	}
	for _, identifier := range []string{"synthetic-container", "synthetic-cont", "/current-live-name"} {
		id, name, err := manager.resolveContainerIdentity(context.Background(), identifier)
		if err != nil || id != "synthetic-container" || name != "current-live-name" {
			t.Fatalf("live identity %q = %q, %q, %v", identifier, id, name, err)
		}
	}
	client.getErr = status.Error(codes.PermissionDenied, "synthetic access denied")
	if _, _, err := manager.resolveContainerIdentity(context.Background(), "synthetic-container"); err == nil {
		t.Fatal("live metadata failure fell back to cached identity")
	}
	if err := manager.writeEngineState(engineState{Mode: "lima", Instance: engineInstanceName, OwnerID: "replaced-owner"}); err != nil {
		t.Fatal(err)
	}
	if _, _, matched := manager.inventoryLimaRuntime(); matched {
		t.Fatal("identity reused a connection belonging to a replaced engine owner")
	}
}

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

func TestContainerNetworkIdentitiesUseAdvertisedOwnedHelper(t *testing.T) {
	firstID, secondID := strings.Repeat("a", 64), strings.Repeat("b", 64)
	document := fmt.Sprintf(`[{"Name":"synthetic-first","ID":%q},{"Name":"synthetic-second","ID":%q}]`, firstID, secondID)
	denied := errors.New("synthetic helper permission denied")
	for _, test := range []struct {
		name, output, message string
		legacy, cancel        bool
		configured, caller    time.Duration
		failure, wantErr      error
	}{
		{name: "advertised helper"},
		{name: "legacy helper", legacy: true},
		{name: "helper failure remains explicit", failure: denied, wantErr: denied},
		{name: "invalid JSON remains explicit", output: "{", message: "decode canonical container network identities"},
		{name: "missing identity remains explicit", output: "[]", wantErr: ErrUnavailable},
		{name: "conflicting identities remain explicit", output: `[{"Name":"synthetic-first","ID":"one"},{"Name":"synthetic-first","ID":"two"}]`, wantErr: ErrConflict},
		{name: "shorter caller deadline", caller: 2 * time.Second},
		{name: "longer configured deadline", configured: 2 * time.Minute},
		{name: "caller cancellation", cancel: true, wantErr: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			caller := 3 * time.Minute
			if test.caller > 0 {
				caller = test.caller
			}
			configured := defaultTimeout
			if test.configured > 0 {
				configured = test.configured
			}
			ctx, cancel := context.WithTimeout(context.Background(), caller)
			defer cancel()
			queries := 0
			runner := lifecycleDeadlineRunner(func(commandContext context.Context, command runtimes.Command) ([]byte, error) {
				if command.Name != "limactl" || !command.StdoutOnly ||
					!containsArgumentSequence(command.Args, []string{
						"sh", "-c", limaOwnedRuntimeCommand, "porto-runtime-command", "synthetic-owner",
					}) {
					return nil, fmt.Errorf("network metadata omitted owned structured execution: %+v", command)
				}
				if command.Args[len(command.Args)-1] == "probe" {
					return json.Marshal(runtimeHelperProbe{CNI: true, NetworkIdentities: !test.legacy})
				}
				queries++
				want := []string{"porto-runtime-helper", "synthetic-namespace", "cni-identities", "synthetic-first", "synthetic-second"}
				if test.legacy {
					want = []string{"nerdctl", "network", "inspect", "synthetic-first", "synthetic-second"}
				}
				if !containsArgumentSequence(command.Args, want) {
					return nil, fmt.Errorf("canonical lookup used the wrong helper/namespace or inspected containers: %+v", command)
				}
				deadline, ok := commandContext.Deadline()
				budget := min(max(configured, objectInspectionTimeout), caller)
				if remaining := time.Until(deadline); !ok || remaining <= budget-time.Second || remaining > budget {
					return nil, fmt.Errorf("metadata lookup changed its bounded caller/probe budget: %s", remaining)
				}
				if test.cancel {
					cancel()
					return nil, context.Cause(commandContext)
				}
				if test.failure != nil {
					return nil, test.failure
				}
				if test.output != "" {
					return []byte(test.output), nil
				}
				return []byte(document), nil
			})
			manager := NewWithStateDir(runner, t.TempDir())
			manager.timeout = configured
			if err := manager.writeEngineState(engineState{Mode: "lima", Instance: engineInstanceName, OwnerID: "synthetic-owner"}); err != nil {
				t.Fatal(err)
			}
			runtime := &grpcContainerRuntime{
				runner: runner, lima: engineInstanceName, namespace: "synthetic-namespace",
				commandSource: commandBackend{name: "limactl", limaInstance: engineInstanceName, ownerID: "synthetic-owner"},
			}
			runtime.Capabilities(context.Background())
			manager.inventoryCancel = func() {}
			manager.inventory = &containerInventory{snapshot: ContainerSnapshot{Available: true}, runtimeClient: runtime}
			identities, err := manager.containerNetworkIdentities(ctx, []string{"synthetic-second", "none", "synthetic-first", "synthetic-first"})
			switch {
			case test.wantErr != nil:
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("network identity error = %v, want %v", err, test.wantErr)
				}
			case test.message != "":
				if err == nil || !strings.Contains(err.Error(), test.message) {
					t.Fatalf("network identity error = %v, want %q", err, test.message)
				}
			case err != nil || len(identities) != 2 || identities["synthetic-first"] != firstID || identities["synthetic-second"] != secondID:
				t.Fatalf("actual selected identities were not preserved: %+v, %v", identities, err)
			}
			if queries != 1 {
				t.Fatalf("identity lookup retried or hid a helper failure: %d queries", queries)
			}
		})
	}
}

func TestContainerNetworkIdentityReadsAllowSlowBackendWithoutExtendingCallers(t *testing.T) {
	for _, callerDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "slow metadata read", true: "shorter caller deadline"}[callerDeadline], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				id := strings.Repeat("a", 64)
				queries := 0
				runner := lifecycleDeadlineRunner(func(ctx context.Context, command runtimes.Command) ([]byte, error) {
					if command.Name != "nerdctl" || strings.Join(command.Args, " ") != "network inspect synthetic-stack" {
						return nil, fmt.Errorf("unexpected network identity command: %+v", command)
					}
					queries++
					timer := time.NewTimer(25 * time.Second)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						return nil, context.Cause(ctx)
					case <-timer.C:
						return []byte(`[{"Name":"synthetic-stack","ID":"` + id + `"}]`), nil
					}
				})
				manager := New(runner)
				ctx := context.Background()
				if callerDeadline {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
				}
				started := time.Now()
				identities, err := manager.containerNetworkIdentities(ctx, []string{"synthetic-stack"})
				wantElapsed := 25 * time.Second
				if callerDeadline {
					wantElapsed = 10 * time.Second
					if !errors.Is(err, context.DeadlineExceeded) || len(identities) != 0 {
						t.Fatalf("shorter caller deadline was extended or returned partial identities: %+v, %v", identities, err)
					}
				} else if err != nil || len(identities) != 1 || identities["synthetic-stack"] != id {
					t.Fatalf("a slow authoritative read lost its actual canonical identity: %+v, %v", identities, err)
				}
				if queries != 1 || time.Since(started) != wantElapsed {
					t.Fatalf("metadata read retried or exceeded its bounded allowance: queries=%d elapsed=%s", queries, time.Since(started))
				}
			})
		})
	}
}

func TestDockerObjectInspectionAllowsSlowReadsWithoutExtendingCallers(t *testing.T) {
	for _, kind := range []string{"container", "network", "volume"} {
		for _, callerDeadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/caller-deadline=%t", kind, callerDeadline), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					queries := 0
					runner := lifecycleDeadlineRunner(func(ctx context.Context, command runtimes.Command) ([]byte, error) {
						if strings.Join(command.Args, " ") != kind+" inspect synthetic-object" {
							return nil, fmt.Errorf("unexpected object inspection: %+v", command)
						}
						queries++
						timer := time.NewTimer(25 * time.Second)
						defer timer.Stop()
						select {
						case <-ctx.Done():
							return nil, context.Cause(ctx)
						case <-timer.C:
							return []byte(`[{"Id":"synthetic-object"}]`), nil
						}
					})
					ctx := context.Background()
					if callerDeadline {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
						defer cancel()
					}
					started := time.Now()
					document, err := New(runner).inspect(ctx, kind, "synthetic-object")
					wantElapsed := 25 * time.Second
					if callerDeadline {
						wantElapsed = 10 * time.Second
						if !errors.Is(err, context.DeadlineExceeded) || len(document) != 0 {
							t.Fatalf("shorter caller deadline was lost: %s, %v", document, err)
						}
					} else if err != nil || string(document) != `{"Id":"synthetic-object"}` {
						t.Fatalf("slow authoritative object inspection failed: %s, %v", document, err)
					}
					if queries != 1 || time.Since(started) != wantElapsed {
						t.Fatalf("object inspection retried or exceeded its allowance: queries=%d elapsed=%s", queries, time.Since(started))
					}
				})
			})
		}
	}
}

func TestDockerContainerInspectionSharesBudgetWithNetworkIdentityLookup(t *testing.T) {
	for _, scenario := range []struct {
		name                               string
		objectDelay, identityDelay, caller time.Duration
		wantElapsed                        time.Duration
		wantSuccess                        bool
		unresolved                         bool
	}{
		{"complete object and network", 25 * time.Second, 25 * time.Second, 0, 50 * time.Second, true, false},
		{"one shared allowance", 40 * time.Second, 40 * time.Second, 0, time.Minute, false, false},
		{"shorter caller", 25 * time.Second, 25 * time.Second, 10 * time.Second, 10 * time.Second, false, false},
		{"unresolved live attachment", 25 * time.Second, 25 * time.Second, 0, 50 * time.Second, true, true},
		{"unresolved shared allowance", 40 * time.Second, 40 * time.Second, 0, time.Minute, false, true},
		{"unresolved shorter caller", 25 * time.Second, 25 * time.Second, 10 * time.Second, 10 * time.Second, false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				id := strings.Repeat("a", 64)
				runner := lifecycleDeadlineRunner(func(ctx context.Context, command runtimes.Command) ([]byte, error) {
					var delay time.Duration
					var document string
					switch strings.Join(command.Args, " ") {
					case "container inspect synthetic-object":
						delay = scenario.objectDelay
						document = `[{"Id":"synthetic-object","Config":{"Labels":{}},"State":{"Status":"running","Running":true,"Pid":42},"NetworkSettings":{"Networks":{"synthetic-stack":{"IPAddress":"10.88.0.2"}}}}]`
						if scenario.unresolved {
							document = `[{"Id":"synthetic-object","Config":{"Labels":{"nerdctl/networks":"[\"synthetic-stack\"]"}},"State":{"Status":"running","Running":true,"Pid":42},"NetworkSettings":{"Networks":{"unknown-eth0":{"IPAddress":"10.88.0.2"}}}}]`
						}
					case "network inspect synthetic-stack":
						delay = scenario.identityDelay
						document = `[{"Name":"synthetic-stack","ID":"` + id + `","Containers":{"synthetic-object":{"IPv4Address":"10.88.0.2/24"}}}]`
					default:
						return nil, fmt.Errorf("unexpected combined inspection: %+v", command)
					}
					timer := time.NewTimer(delay)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						return nil, context.Cause(ctx)
					case <-timer.C:
						return []byte(document), nil
					}
				})
				ctx := context.Background()
				if scenario.caller > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, scenario.caller)
					defer cancel()
				}
				request := httptest.NewRequest(http.MethodGet, "/v1.47/containers/synthetic-object/json", nil).WithContext(ctx)
				response := httptest.NewRecorder()
				started := time.Now()
				NewAPI(New(runner), "/synthetic/socket").ServeHTTP(response, request)
				if (response.Code == http.StatusOK) != scenario.wantSuccess || time.Since(started) != scenario.wantElapsed {
					t.Fatalf("combined inspection reset or exceeded its budget: status=%d elapsed=%s body=%s", response.Code, time.Since(started), response.Body.String())
				}
				if scenario.wantSuccess && (!strings.Contains(response.Body.String(), id) ||
					!strings.Contains(response.Body.String(), `"Pid":42`) || !strings.Contains(response.Body.String(), "10.88.0.2")) {
					t.Fatalf("combined inspection lost authoritative object/network state: %s", response.Body.String())
				}
			})
		})
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

func TestDockerContainerInspectReconcilesUnresolvedComposeNetwork(t *testing.T) {
	networkID := strings.Repeat("c", 64)
	document := `[{
		"Id":"synthetic-container",
		"Config":{"Labels":{"nerdctl/networks":"[\"synthetic-compose-network\"]"}},
		"State":{"Status":"running","Running":true,"Pid":42,"Health":{"Status":"healthy"}},
		"Mounts":[{"Type":"volume","Name":"synthetic-data","Destination":"/data"}],
		"NetworkSettings":{
			"Ports":{"8080/tcp":[{"HostIp":"127.0.0.1","HostPort":"18080"}]},
			"Networks":{"unknown-eth0":{
				"NetworkID":"","IPAddress":"10.88.0.2","IPPrefixLen":24,
				"MacAddress":"02:00:00:00:00:02","Aliases":["synthetic-app"]
			}}
		}
	}]`
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		switch command.Name + " " + strings.Join(command.Args, " ") {
		case "nerdctl container inspect synthetic-container":
			return []byte(document), nil
		case "nerdctl network inspect synthetic-compose-network":
			return []byte(`[{
				"Name":"synthetic-compose-network","Id":"` + networkID + `",
				"Containers":{"synthetic-container":{
					"IPv4Address":"10.88.0.2/24","MacAddress":"02:00:00:00:00:02"
				}}
			}]`), nil
		default:
			return nil, fmt.Errorf("inspection must not query placeholder networks or mutate runtime state: %+v", command)
		}
	}}
	response := httptest.NewRecorder()
	NewAPI(New(runner), "/synthetic/socket").ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/v1.47/containers/synthetic-container/json", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("Compose health inspection failed on an unresolved interface: %d: %s", response.Code, response.Body.String())
	}
	var inspected struct {
		State struct {
			Running bool
			Pid     int
			Health  struct{ Status string }
		}
		Mounts          []struct{ Type, Name, Destination string }
		NetworkSettings struct {
			Ports    map[string][]struct{ HostIP, HostPort string }
			Networks map[string]struct {
				NetworkID, IPAddress, MacAddress string
				IPPrefixLen                      int
				Aliases                          []string
			}
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	endpoint := inspected.NetworkSettings.Networks["synthetic-compose-network"]
	if len(inspected.NetworkSettings.Networks) != 1 || endpoint.NetworkID != networkID ||
		endpoint.IPAddress != "10.88.0.2" || endpoint.IPPrefixLen != 24 ||
		endpoint.MacAddress != "02:00:00:00:00:02" || strings.Join(endpoint.Aliases, ",") != "synthetic-app" {
		t.Fatalf("unresolved interface was not preserved under its actual Compose network: %s", response.Body.String())
	}
	ports := inspected.NetworkSettings.Ports["8080/tcp"]
	if !inspected.State.Running || inspected.State.Pid != 42 || inspected.State.Health.Status != "healthy" ||
		len(inspected.Mounts) != 1 || inspected.Mounts[0].Name != "synthetic-data" ||
		len(ports) != 1 || ports[0].HostIP != "127.0.0.1" || ports[0].HostPort != "18080" {
		t.Fatalf("network reconciliation changed health, process, volume or published-port state: %s", response.Body.String())
	}
}

func TestResolvedContainerNetworkDoesNotRequireRecordedMetadata(t *testing.T) {
	document := json.RawMessage(`{"Config":{"Labels":{"nerdctl/networks":"invalid"}},"NetworkSettings":{"Networks":{"unknown-eth0":{"NetworkID":"synthetic-resolved-id","IPAddress":"10.88.0.2"}}}}`)
	manager := New(&fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		t.Fatalf("already-resolved network required another runtime query: %+v", command)
		return nil, nil
	}})
	inspected, err := manager.completeInspectNetworkIdentities(context.Background(), document)
	if err != nil || !bytes.Equal(inspected, document) {
		t.Fatalf("resolved network was changed or rejected by unused recorded metadata: %s, %v", inspected, err)
	}
}

func TestUnresolvedContainerNetworksMatchLiveAttachmentsRatherThanLabelOrder(t *testing.T) {
	document := json.RawMessage(`{
		"Id":"synthetic-container",
		"Config":{"Labels":{"nerdctl/networks":"[\"synthetic-z\",\"synthetic-a\",\"synthetic-existing\",\"none\"]"}},
		"NetworkSettings":{"Networks":{
			"unknown-eth0":{"IPAddress":"10.90.0.2","IPPrefixLen":24},
			"unknown-porto10":{"GlobalIPv6Address":"2001:0db8:20:0:0:0:0:2","GlobalIPv6PrefixLen":64},
			"synthetic-existing":{"NetworkID":"synthetic-retained-id","IPAddress":"10.80.0.2"}
		}}
	}`)
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if command.Name != "nerdctl" || strings.Join(command.Args, " ") != "network inspect synthetic-a synthetic-existing synthetic-z" {
			return nil, fmt.Errorf("live reconciliation queried an unrecorded network or network mode: %+v", command)
		}
		return []byte(`[
			{"Name":"synthetic-z","Id":"synthetic-v4-id","Containers":{"synthetic-container":{"IPv4Address":"10.90.0.2/24"}}},
			{"Name":"synthetic-existing","Id":"synthetic-retained-id"},
			{"Name":"synthetic-a","Id":"synthetic-v6-id","Containers":{"synthetic-container":{"IPv6Address":"2001:db8:20::2/64"}}}
		]`), nil
	}}
	result, err := New(runner).completeInspectNetworkIdentities(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	var inspected struct {
		NetworkSettings struct {
			Networks map[string]struct {
				NetworkID, IPAddress, GlobalIPv6Address string
				IPPrefixLen, GlobalIPv6PrefixLen        int
			}
		}
	}
	if err := json.Unmarshal(result, &inspected); err != nil {
		t.Fatal(err)
	}
	networks := inspected.NetworkSettings.Networks
	if len(networks) != 3 || networks["synthetic-z"].NetworkID != "synthetic-v4-id" ||
		networks["synthetic-z"].IPAddress != "10.90.0.2" || networks["synthetic-z"].IPPrefixLen != 24 ||
		networks["synthetic-a"].NetworkID != "synthetic-v6-id" ||
		networks["synthetic-a"].GlobalIPv6Address != "2001:0db8:20:0:0:0:0:2" ||
		networks["synthetic-a"].GlobalIPv6PrefixLen != 64 ||
		networks["synthetic-existing"].NetworkID != "synthetic-retained-id" ||
		networks["synthetic-existing"].IPAddress != "10.80.0.2" {
		t.Fatalf("network reconciliation guessed from interface/label order or changed an existing endpoint: %s", result)
	}
}

func TestRecordedUnknownPrefixNetworkIsNotRewritten(t *testing.T) {
	document := json.RawMessage(`{"Id":"synthetic-container","Config":{"Labels":{"nerdctl/networks":"[\"unknown-eth0\"]"}},"NetworkSettings":{"Networks":{"unknown-eth0":{"IPAddress":"10.88.0.2"}}}}`)
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if command.Name != "nerdctl" || strings.Join(command.Args, " ") != "network inspect unknown-eth0" {
			return nil, fmt.Errorf("recorded network name was treated as a placeholder: %+v", command)
		}
		return []byte(`[{"Name":"unknown-eth0","ID":"synthetic-real-network-id"}]`), nil
	}}
	result, err := New(runner).completeInspectNetworkIdentities(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	var inspected struct {
		NetworkSettings struct {
			Networks map[string]struct{ NetworkID, IPAddress string }
		}
	}
	if err := json.Unmarshal(result, &inspected); err != nil {
		t.Fatal(err)
	}
	endpoint := inspected.NetworkSettings.Networks["unknown-eth0"]
	if len(inspected.NetworkSettings.Networks) != 1 || endpoint.NetworkID != "synthetic-real-network-id" || endpoint.IPAddress != "10.88.0.2" {
		t.Fatalf("a real recorded unknown-prefix network was changed or rejected: %s, %v", result, err)
	}
}

func TestUnresolvedContainerNetworkRejectsUnverifiableAttachments(t *testing.T) {
	denied := errors.New("synthetic live attachment access denied")
	for _, test := range []struct {
		name, recorded, endpoints, output, command, message string
		failure, wantErr                                    error
		noQuery                                             bool
	}{
		{name: "missing recorded candidates", recorded: "[]", wantErr: ErrUnavailable, noQuery: true},
		{name: "malformed recorded candidates", recorded: "{", message: "decode recorded container networks", noQuery: true},
		{name: "another container", output: `[{"Name":"synthetic-stack","ID":"synthetic-id","Containers":{"synthetic-other":{"IPv4Address":"10.88.0.2/24","MacAddress":"02:00:00:00:00:02"}}}]`, wantErr: ErrUnavailable},
		{name: "stale address", output: `[{"Name":"synthetic-stack","ID":"synthetic-id","Containers":{"synthetic-container":{"IPv4Address":"10.88.0.3/24","MacAddress":"02:00:00:00:00:02"}}}]`, wantErr: ErrUnavailable},
		{name: "stale MAC", output: `[{"Name":"synthetic-stack","ID":"synthetic-id","Containers":{"synthetic-container":{"IPv4Address":"10.88.0.2/24","MacAddress":"02:00:00:00:00:03"}}}]`, wantErr: ErrUnavailable},
		{name: "no live observations", endpoints: `{"unknown-eth0":{}}`, wantErr: ErrUnavailable},
		{name: "ambiguous networks", recorded: `["synthetic-stack","synthetic-other"]`, command: "network inspect synthetic-other synthetic-stack",
			output: `[{"Name":"synthetic-stack","ID":"synthetic-id","Containers":{"synthetic-container":{"IPv4Address":"10.88.0.2/24"}}},{"Name":"synthetic-other","ID":"synthetic-other-id","Containers":{"synthetic-container":{"IPv4Address":"10.88.0.2/24"}}}]`, wantErr: ErrConflict},
		{name: "existing endpoint", endpoints: `{"unknown-eth0":{"IPAddress":"10.88.0.2"},"synthetic-stack":{"NetworkID":"synthetic-existing-id"}}`, wantErr: ErrConflict},
		{name: "malformed live attachments", output: "{", message: "decode live container network attachments"},
		{name: "backend failure", failure: denied, wantErr: denied},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorded := firstNonEmpty(test.recorded, `["synthetic-stack"]`)
			endpoints := firstNonEmpty(test.endpoints, `{"unknown-eth0":{"IPAddress":"10.88.0.2","MacAddress":"02:00:00:00:00:02"}}`)
			document, err := json.Marshal(map[string]any{
				"Id": "synthetic-container", "Config": map[string]any{"Labels": map[string]string{nerdctlNetworksLabel: recorded}},
				"NetworkSettings": map[string]any{"Networks": json.RawMessage(endpoints)},
			})
			if err != nil {
				t.Fatal(err)
			}
			queries := 0
			runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				queries++
				want := firstNonEmpty(test.command, "network inspect synthetic-stack")
				if test.noQuery || command.Name != "nerdctl" || strings.Join(command.Args, " ") != want {
					return nil, fmt.Errorf("unverifiable endpoint queried a placeholder, unrecorded network or mutation: %+v", command)
				}
				return []byte(firstNonEmpty(test.output,
					`[{"Name":"synthetic-stack","ID":"synthetic-id","Containers":{"synthetic-container":{"IPv4Address":"10.88.0.2/24","MacAddress":"02:00:00:00:00:02"}}}]`)), test.failure
			}}
			result, err := New(runner).completeInspectNetworkIdentities(context.Background(), document)
			if result != nil || err == nil || (test.wantErr != nil && !errors.Is(err, test.wantErr)) ||
				(test.message != "" && !strings.Contains(err.Error(), test.message)) {
				t.Fatalf("unverifiable attachment returned partial/successful inspection or hid its failure: %s, %v", result, err)
			}
			if (test.noQuery && queries != 0) || (!test.noQuery && queries != 1) {
				t.Fatalf("unverifiable attachment retried or queried despite invalid metadata: %d queries", queries)
			}
		})
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
