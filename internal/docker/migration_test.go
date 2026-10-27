package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	apitypes "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestMigrationRequestRejectsNonLocalResourcePaths(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	source := fixtureMigrationClient(server)
	for _, resource := range []string{
		"",
		"volumes/synthetic",
		"@synthetic.invalid/metadata",
		":2375/metadata",
		"http://synthetic.invalid/metadata",
		"//synthetic.invalid/metadata",
		"/volumes/synthetic?force=true",
		"/volumes/synthetic#fragment",
		"/volumes/%zz",
	} {
		t.Run(resource, func(t *testing.T) {
			before := requests.Load()
			response, err := source.request(context.Background(), http.MethodGet, resource, nil, nil)
			if response != nil {
				response.Body.Close()
			}
			if !errors.Is(err, datafiles.ErrInvalid) || requests.Load() != before {
				t.Fatalf("invalid source path reached the transport: response=%v, error=%v, requests=%d", response, err, requests.Load()-before)
			}
		})
	}
}

func TestMigrationRequestRejectsSourceRedirects(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, location := range []string{"/redirected", "http://synthetic.invalid/redirected", "//synthetic.invalid/redirected"} {
			t.Run(fmt.Sprintf("%d-%s", status, location), func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.URL.Path == "/containers/create" {
						w.Header().Set("Location", location)
						w.WriteHeader(status)
						fmt.Fprint(w, `{"message":"synthetic redirect"}`)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				defer server.Close()
				response, err := fixtureMigrationClient(server).request(context.Background(), http.MethodPost, "/containers/create", nil, strings.NewReader(`{"name":"synthetic"}`))
				if response != nil {
					response.Body.Close()
				}
				var sourceError *sourceAPIError
				if !errors.As(err, &sourceError) || sourceError.status != status || requests.Load() != 1 {
					t.Fatalf("source redirect was followed: response=%v, error=%v, requests=%d", response, err, requests.Load())
				}
			})
		}
	}
}

func TestMigrationRequestPreservesEscapedPathsQueriesAndBodies(t *testing.T) {
	for _, test := range []struct {
		name, method, resource, path, body, contentType string
		query                                           url.Values
	}{
		{
			name: "escaped image name", method: http.MethodGet,
			resource: "/images/" + url.PathEscape("synthetic.invalid/team/synthetic:latest?#%") + "/json",
			path:     "/images/synthetic.invalid/team/synthetic:latest?#%/json",
			query:    url.Values{"names": {"http://synthetic.invalid/metadata?force=true#fragment", "synthetic&quiet=false"}},
		},
		{
			name: "container configuration", method: http.MethodPost, resource: "/containers/create", path: "/containers/create",
			query: url.Values{"name": {"synthetic?name#fragment%/@host:2375"}},
			body:  `{"Image":"synthetic:latest"}`, contentType: "application/json",
		},
		{
			name: "image archive", method: http.MethodPost, resource: "/images/load", path: "/images/load",
			query: url.Values{"quiet": {"true"}}, body: "synthetic archive stream", contentType: "application/x-tar",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				expectedURI := test.resource + "?" + test.query.Encode()
				if r.Host != "docker" || r.Method != test.method || r.RequestURI != expectedURI || r.URL.Path != test.path {
					t.Errorf("source request target changed: host=%q method=%q URI=%q path=%q", r.Host, r.Method, r.RequestURI, r.URL.Path)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != test.body || r.Header.Get("Content-Type") != test.contentType {
					t.Errorf("source request body changed: body=%q content-type=%q error=%v", body, r.Header.Get("Content-Type"), err)
				}
				fmt.Fprint(w, "synthetic response")
			}))
			defer server.Close()
			source := fixtureMigrationClient(server)
			var body io.Reader
			if test.body != "" {
				body = strings.NewReader(test.body)
			}
			response, err := source.request(context.Background(), test.method, test.resource, test.query, body)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			result, err := io.ReadAll(response.Body)
			if err != nil || string(result) != "synthetic response" {
				t.Fatalf("source response changed: %q, %v", result, err)
			}
			if source.client.CheckRedirect != nil {
				t.Fatal("migration request changed the shared client's redirect policy")
			}
		})
	}
}

func TestMigrationRejectsRunningAndUnsupportedContainersBeforeCreation(t *testing.T) {
	for _, document := range []string{
		`{"Id":"fixture-id","Name":"/fixture","State":{"Running":true},"Config":{"Image":"fixture:latest"},"HostConfig":{}}`,
		`{"Id":"fixture-id","Name":"/fixture","State":{"Running":false},"Config":{"Image":"fixture:latest"},"HostConfig":{"PidMode":"host"}}`,
		`{"Id":"fixture-id","Name":"/fixture","State":{"Running":false},"Config":{"Image":"fixture:latest"},"HostConfig":{"Binds":["/unrelated:/data"]}}`,
	} {
		if _, _, err := migrationContainerRequest([]byte(document), false); err == nil {
			t.Fatalf("unsupported source configuration accepted: %s", document)
		}
	}
}

func TestMigrationRequiresExplicitSensitiveEnvironmentConsent(t *testing.T) {
	document := []byte(`{"Id":"fixture-id","Name":"/fixture","State":{"Running":false},"Config":{"Image":"fixture:latest","Env":["FIXTURE_TOKEN=synthetic-value","MODE=test"]},"HostConfig":{"RestartPolicy":{"Name":"no"}}}`)
	if _, _, err := migrationContainerRequest(document, false); err == nil {
		t.Fatal("sensitive environment was copied without explicit selection")
	}
	request, _, err := migrationContainerRequest(document, true)
	if err != nil || len(request.Environment) != 2 || request.Name != "fixture" {
		t.Fatalf("selected local environment transfer was lost: %+v %v", request, err)
	}
}

func TestMigrationPreservesExposedPortsAndIPv6Bindings(t *testing.T) {
	document := []byte(`{
		"Id":"synthetic-container","Name":"/synthetic-app","State":{"Running":false},
		"Config":{"Image":"synthetic:latest","ExposedPorts":{"80/tcp":{},"53/udp":{}}},
		"HostConfig":{"PortBindings":{"80/tcp":[{"HostIP":"::1","HostPort":"8080"}]}},
		"NetworkSettings":{"Networks":{"synthetic-network":{"Aliases":["synthetic-alias"],"IPAddress":"172.28.0.2","IPPrefixLen":16,"Gateway":"172.28.0.1"}}}
	}`)
	request, _, err := migrationContainerRequest(document, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(request.Expose, []string{"53/udp", "80/tcp"}) ||
		!reflect.DeepEqual(request.Publish, []string{"[::1]:8080:80/tcp"}) ||
		!reflect.DeepEqual(request.Networks, []ContainerNetwork{{Name: "synthetic-network", Aliases: []string{"synthetic-alias"}}}) {
		t.Fatalf("source network configuration changed during migration: %+v", request)
	}
}

func TestMigrationDryRunRejectsEveryArchiveTagConflictWithoutSourceMutation(t *testing.T) {
	mutated := false
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutated = true
			http.Error(w, "source mutation forbidden", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/images/json":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "sha256:fixture", "RepoTags": []string{"fixture:latest", "unselected:latest"}}})
		case "/images/sha256:fixture/json":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:fixture", "RepoTags": []string{"fixture:latest", "unselected:latest"}})
		case "/volumes":
			_ = json.NewEncoder(w).Encode(map[string]any{"Volumes": []any{}})
		default:
			_ = json.NewEncoder(w).Encode([]any{})
		}
	}))
	defer source.Close()
	address := source.Listener.Addr().String()
	manager := New(&fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		if strings.Contains(args, "image inspect") {
			if strings.Contains(args, "unselected:latest") {
				return []byte(`[{"Id":"sha256:existing-destination"}]`), nil
			}
			return nil, errors.New("no such image")
		}
		return nil, nil
	}})
	manager.migrationSourceFactory = func(context.Context, string) (*migrationClient, error) {
		return &migrationClient{
			context: MigrationContext{Name: "fixture-source", Supported: true},
			client: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", address)
			}}},
		}, nil
	}
	preview, err := manager.PreviewMigration(context.Background(), dataops.Request{
		Context: "fixture-source", Selections: []dataops.Selection{{Kind: "image", Name: "fixture:latest", ID: "sha256:fixture"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutated || len(preview.Conflicts) != 1 || !strings.Contains(preview.Conflicts[0], "unselected:latest") {
		t.Fatalf("source mutated=%v dry-run conflicts=%v", mutated, preview.Conflicts)
	}
	if len(preview.Objects) != 1 || preview.Objects[0].PlanDigest == "" {
		t.Fatal("source metadata changes do not invalidate the migration plan")
	}
}

func migrationFixtureManager(t *testing.T, documents map[string]any, runner *fakeRunner) *Manager {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("migration mutated an original source resource: %s %s", r.Method, r.URL.Path)
			http.Error(w, "source mutation forbidden", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		value, found := documents[r.URL.Path]
		if !found {
			switch r.URL.Path {
			case "/volumes":
				value = map[string]any{"Volumes": []any{}}
			case "/images/json", "/containers/json", "/networks":
				value = []any{}
			default:
				t.Errorf("unexpected source request: %s", r.URL.Path)
				http.NotFound(w, r)
				return
			}
		}
		if dynamic, ok := value.(func() any); ok {
			value = dynamic()
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	if runner == nil {
		runner = &fakeRunner{}
	}
	manager := New(runner)
	manager.migrationSourceFactory = func(context.Context, string) (*migrationClient, error) {
		return fixtureMigrationClient(server), nil
	}
	return manager
}

func TestMigrationDryRunRejectsDuplicateDestinations(t *testing.T) {
	documents := map[string]any{
		"/networks": []map[string]any{
			{"Id": "network-1", "Name": "synthetic-first", "Driver": "bridge"},
			{"Id": "network-2", "Name": "synthetic-second", "Driver": "bridge"},
		},
		"/networks/network-1": map[string]any{"Id": "network-1", "Name": "synthetic-first", "Driver": "bridge"},
		"/networks/network-2": map[string]any{"Id": "network-2", "Name": "synthetic-second", "Driver": "bridge"},
	}
	manager := migrationFixtureManager(t, documents, nil)
	preview, err := manager.PreviewMigration(context.Background(), dataops.Request{
		Context: "fixture-source",
		Selections: []dataops.Selection{
			{Kind: "network", Name: "synthetic-first", ID: "network-1", Destination: "synthetic-shared"},
			{Kind: "network", Name: "synthetic-second", ID: "network-2", Destination: "synthetic-shared"},
		},
	})
	if err != nil || len(preview.Conflicts) == 0 {
		t.Fatalf("two source objects could silently target the same destination: %+v, %v", preview, err)
	}
}

func TestMigrationPartialFailureAndCancellationCheckpointEverySelectedObject(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled-%t", cancelled), func(t *testing.T) {
			documents := map[string]any{"/networks": []map[string]any{}}
			selections := make([]dataops.Selection, 3)
			for index := range selections {
				id, name := fmt.Sprintf("network-%d", index+1), fmt.Sprintf("synthetic-network-%d", index+1)
				selections[index] = dataops.Selection{Kind: "network", Name: name, ID: id}
				network := map[string]any{"Id": id, "Name": name, "Driver": "bridge"}
				documents["/networks"] = append(documents["/networks"].([]map[string]any), network)
				documents["/networks/"+id] = network
			}
			failure := errors.New("synthetic network creation failure")
			if cancelled {
				failure = context.Canceled
			}
			created := 0
			runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				if slices.Contains(command.Args, "create") {
					created++
					if created == 2 {
						return nil, failure
					}
					return []byte("synthetic-destination-id\n"), nil
				}
				return nil, nil
			}}
			manager := migrationFixtureManager(t, documents, runner)
			preview, err := manager.PreviewMigration(context.Background(), dataops.Request{Context: "fixture-source", Selections: selections})
			if err != nil || len(preview.Conflicts) != 0 {
				t.Fatalf("prepare migration: %+v, %v", preview, err)
			}
			preview.Request.Confirm = true
			var checkpoints []dataops.Result
			result, err := manager.Migrate(context.Background(), preview.Request, t.TempDir(),
				func(string, int64) error { return nil },
				func(result dataops.Result) error {
					snapshot := result
					snapshot.Steps = slices.Clone(result.Steps)
					checkpoints = append(checkpoints, snapshot)
					return nil
				})
			failedStatus := "failed"
			if cancelled {
				failedStatus = "cancelled"
			}
			if !errors.Is(err, failure) || created != 2 || len(result.Steps) != 3 ||
				result.Steps[0].Status != "succeeded" || result.Steps[1].Status != failedStatus ||
				result.Steps[2].Status != "pending" || result.Steps[1].ID != selections[1].ID {
				t.Fatalf("partial migration outcomes were lost: %+v, %v, created=%d", result, err, created)
			}
			if len(checkpoints) != 5 || checkpoints[0].Steps[0].Status != "pending" ||
				checkpoints[1].Steps[0].Status != "running" ||
				!reflect.DeepEqual(checkpoints[len(checkpoints)-1].Steps, result.Steps) {
				t.Fatalf("object mutations did not have before/after persistence: %+v", checkpoints)
			}
		})
	}
}

func TestMigrationRejectsUnsupportedContainerSettingsInsteadOfDroppingThem(t *testing.T) {
	for _, host := range []map[string]any{
		{"CpuShares": 512},
		{"CpusetCpus": "0-1"},
		{"CpuQuota": 50000, "CpuPeriod": 100000},
		{"PidsLimit": 32},
		{"ExtraHosts": []string{"synthetic:127.0.0.1"}},
		{"GroupAdd": []string{"1234"}},
		{"LogConfig": map[string]any{"Type": "syslog"}},
		{"MemorySwap": -1},
		{"Mounts": []map[string]any{{"Type": "bind", "Source": "/synthetic-source", "Target": "/data", "BindOptions": map[string]any{"NonRecursive": true}}}},
		{"Mounts": []map[string]any{{"Type": "bind", "Source": "/synthetic-source", "Target": "/data", "ReadOnly": true, "BindOptions": map[string]any{"ReadOnlyNonRecursive": true}}}},
		{"Mounts": []map[string]any{{"Type": "bind", "Source": "/synthetic-source", "Target": "/data", "ReadOnly": true, "BindOptions": map[string]any{"ReadOnlyForceRecursive": true}}}},
	} {
		document, err := json.Marshal(map[string]any{
			"Id": "synthetic-container", "Name": "/synthetic-app",
			"Config": map[string]any{"Image": "synthetic:latest"}, "HostConfig": host,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := migrationContainerRequest(document, false); !errors.Is(err, ErrUnsupported) {
			t.Errorf("source settings would be silently dropped: %s, %v", document, err)
		}
	}
}

func TestMigrationDryRunRequiresTheContainersExactImage(t *testing.T) {
	container := map[string]any{
		"Id": "synthetic-container", "Name": "/synthetic-app", "Image": "sha256:original-image",
		"Config": map[string]any{"Image": "synthetic:latest"},
	}
	manager := migrationFixtureManager(t, map[string]any{
		"/containers/json":                     []map[string]any{{"Id": "synthetic-container", "Names": []string{"/synthetic-app"}, "State": "exited"}},
		"/containers/synthetic-container/json": container,
	}, nil)
	preview, err := manager.PreviewMigration(context.Background(), dataops.Request{
		Context: "fixture-source", Selections: []dataops.Selection{{Kind: "container", Name: "synthetic-app", ID: "synthetic-container"}},
	})
	if err != nil || len(preview.Conflicts) == 0 || !strings.Contains(strings.Join(preview.Conflicts, "\n"), "image") {
		t.Fatalf("a container could implicitly pull a replacement for its original image: %+v, %v", preview, err)
	}
}

func TestMigrationRechecksConfirmedSourceMetadataBeforeEachObject(t *testing.T) {
	var changed atomic.Bool
	created := false
	manager := migrationFixtureManager(t, map[string]any{
		"/networks": []map[string]any{{"Id": "network-1", "Name": "synthetic-network", "Driver": "bridge"}},
		"/networks/network-1": func() any {
			return map[string]any{
				"Id": "network-1", "Name": "synthetic-network", "Driver": "bridge",
				"Internal": changed.Load(),
			}
		},
	}, &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if slices.Contains(command.Args, "create") {
			created = true
			return []byte("synthetic-destination\n"), nil
		}
		return nil, nil
	}})
	preview, err := manager.PreviewMigration(context.Background(), dataops.Request{
		Context: "fixture-source", Selections: []dataops.Selection{{Kind: "network", Name: "synthetic-network", ID: "network-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	preview.Request.Confirm = true
	result, err := manager.Migrate(context.Background(), preview.Request, t.TempDir(),
		func(string, int64) error { return nil },
		func(result dataops.Result) error {
			if result.Steps[0].Status == "running" {
				changed.Store(true)
			}
			return nil
		})
	if !errors.Is(err, datafiles.ErrConflict) || created || len(result.Steps) != 1 || result.Steps[0].Status != "failed" {
		t.Fatalf("confirmed source configuration changed without a new dry run: %+v, %v, created=%v", result, err, created)
	}
}

func TestMigrationDoesNotReuseAnEarlierMigrationWithChangedSourceConfiguration(t *testing.T) {
	document := map[string]any{"Id": "network-1", "Name": "synthetic-network", "Driver": "bridge", "Internal": true}
	manager := migrationFixtureManager(t, map[string]any{
		"/networks": []map[string]any{document}, "/networks/network-1": document,
	}, &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if strings.HasPrefix(strings.Join(command.Args, " "), "network ls ") {
			return json.Marshal(map[string]string{
				"ID": "destination-network", "Name": "synthetic-network",
				"Labels": migrationSourceLabel + "=" + migrationIdentity("fixture-source", "network", "network-1"),
			})
		}
		return nil, nil
	}})
	preview, err := manager.PreviewMigration(context.Background(), dataops.Request{
		Context: "fixture-source", Selections: []dataops.Selection{{Kind: "network", Name: "synthetic-network", ID: "network-1"}},
	})
	if err != nil || len(preview.Conflicts) == 0 {
		t.Fatalf("source-ID labels alone falsely proved configuration fidelity: %+v, %v", preview, err)
	}
}

func TestMigrationReplayPlanBindsRenamedDependenciesButNotSelectionOrder(t *testing.T) {
	metadata := json.RawMessage(`{"Id":"synthetic-container","Config":{"Image":"synthetic:latest"}}`)
	request := dataops.Request{Selections: []dataops.Selection{
		{Kind: "image", Name: "synthetic:latest", ID: "image-1"},
		{Kind: "volume", Name: "synthetic-data", ID: "volume-1", Destination: "restored-data"},
	}}
	first, err := migrationReplayPlan(metadata, request, "container")
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(request.Selections)
	request.Selections[1].Destination = request.Selections[1].Name
	second, err := migrationReplayPlan(metadata, request, "container")
	if err != nil || first != second {
		t.Fatalf("equivalent retry selections changed the plan: %s %s, %v", first, second, err)
	}
	request.Selections[0].Destination = "different-data"
	third, err := migrationReplayPlan(metadata, request, "container")
	if err != nil || first == third {
		t.Fatalf("a retry could silently reuse different mounted data: %s %s, %v", first, third, err)
	}
}

func TestMigrationDoesNotSkipIncompleteContainerResourceUpdates(t *testing.T) {
	document := json.RawMessage(`{
		"Id":"synthetic-source-container","Name":"/synthetic-app","Image":"sha256:source-image",
		"Config":{"Image":"synthetic:latest"},"HostConfig":{"Memory":536870912}
	}`)
	selection := dataops.Selection{Kind: "container", Name: "synthetic-app", ID: "synthetic-source-container"}
	request := dataops.Request{Context: "fixture-source", Selections: []dataops.Selection{selection}}
	plan, err := migrationReplayPlan(document, request, "container")
	if err != nil {
		t.Fatal(err)
	}
	mutations := 0
	manager := New(&fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		switch {
		case len(command.Args) > 0 && command.Args[0] == "ps":
			return json.Marshal(map[string]string{
				"ID": "synthetic-destination", "Names": "synthetic-app",
				"Labels": migrationSourceLabel + "=" + migrationIdentity(request.Context, selection.Kind, selection.ID) + "," + migrationPlanLabel + "=" + plan,
			})
		case strings.Join(command.Args, " ") == "container inspect synthetic-destination":
			return []byte(`[{"Id":"synthetic-destination","HostConfig":{"Memory":0}}]`), nil
		default:
			mutations++
			return nil, nil
		}
	}})
	source := &migrationClient{context: MigrationContext{Name: request.Context}}
	err = manager.migrateContainer(context.Background(), source, request, selection, selection.Name, document)
	if !errors.Is(err, datafiles.ErrConflict) || mutations != 0 {
		t.Fatalf("an interrupted resource update was reported as successful or overwritten: %v, mutations=%d", err, mutations)
	}
}

func TestMigrationOCIImageIdentityAllowsInspectorDefaultsButRejectsChangedContent(t *testing.T) {
	source := json.RawMessage(`{"Id":"sha256:source-manifest","Descriptor":{"digest":"sha256:source-manifest"},"Architecture":"arm64","Os":"linux","Config":{"Env":["SYNTHETIC_MODE=original"]},"RootFS":{"Type":"layers","Layers":["sha256:synthetic-layer"]}}`)
	for _, changed := range []bool{false, true} {
		environment := "original"
		if changed {
			environment = "changed"
		}

		destination := `[{"Id":"sha256:destination-manifest","Architecture":"arm64","Os":"linux","Config":{"Env":["SYNTHETIC_MODE=` + environment + `"],"AttachStdin":false,"Image":"sha256:legacy-parent-hint"},"RootFS":{"Type":"layers","Layers":["sha256:synthetic-layer"]}}]`
		manager := New(&fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
			if slices.Contains(command.Args, "inspect") {
				return []byte(destination), nil
			}
			return nil, nil
		}})
		matches, err := manager.migrationImageMatches(context.Background(), "synthetic:latest", source)
		if err != nil || matches == changed {
			t.Fatalf("inspector compatibility changed image identity: matches=%v changed=%v err=%v", matches, changed, err)
		}
	}
}

func TestMigrationOCIImageIdentityVerifiesPinnedConfigDespiteInspectorLayerDifferences(t *testing.T) {
	for _, scenario := range []string{"unchanged", "changed configuration", "changed layers", "missing source manifest", "missing destination image", "missing legacy destination image"} {
		t.Run(scenario, func(t *testing.T) {
			service := &snapshotterTestServer{snapshotter: "overlayfs"}
			backend := newSnapshotterTestRuntime(t, "synthetic-engine", "", service)
			fixture := seedSnapshotterTestImage(t, backend, service)
			sourceManifest := fixture.manifest
			if scenario == "changed configuration" || scenario == "changed layers" {
				write := func(mediaType string, value any) ocispec.Descriptor {
					t.Helper()
					document, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					descriptor := ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(document), Size: int64(len(document))}
					if err := content.WriteBlob(context.Background(), fixture.store, descriptor.Digest.String(), bytes.NewReader(document), descriptor); err != nil {
						t.Fatal(err)
					}
					return descriptor
				}
				image := ocispec.Image{
					Platform: ocispec.Platform{OS: "linux", Architecture: "arm64"},
					Config:   ocispec.ImageConfig{Cmd: []string{"true"}},
					RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{fixture.layer.Digest}},
				}
				if scenario == "changed configuration" {
					image.Config.Cmd = []string{"false"}
				} else {
					image.RootFS.DiffIDs = []digest.Digest{digest.FromString("synthetic-changed-layer")}
				}
				configuration := write(ocispec.MediaTypeImageConfig, image)
				manifest := write(ocispec.MediaTypeImageManifest, ocispec.Manifest{
					Versioned: imagespec.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest,
					Config: configuration, Layers: []ocispec.Descriptor{fixture.layer},
				})
				service.mu.Lock()
				service.image.Target = &apitypes.Descriptor{MediaType: manifest.MediaType, Digest: manifest.Digest.String(), Size: manifest.Size}
				service.mu.Unlock()
			} else if scenario == "missing source manifest" {
				sourceManifest.Digest = digest.FromString("synthetic-missing-source-manifest")
			} else if strings.HasPrefix(scenario, "missing") {
				service.mu.Lock()
				service.image = nil
				service.mu.Unlock()
				if scenario == "missing legacy destination image" {
					sourceManifest = ocispec.Descriptor{}
				}
			}
			sourceID := sourceManifest.Digest.String()
			if scenario == "missing legacy destination image" {
				sourceID = fixture.config.Digest.String()
			}
			source, err := json.Marshal(map[string]any{
				"Id": sourceID, "Descriptor": sourceManifest,
				"Architecture": "arm64", "Os": "linux", "Config": map[string]any{"Cmd": []string{"true"}},
				"RootFS": map[string]any{"Type": "layers", "Layers": []string{"sha256:source-inspector-layer"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			manager := New(&fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
				if slices.Contains(command.Args, "inspect") {
					return []byte(`[{"Architecture":"arm64","Os":"linux","Config":{"Cmd":["true"]},"RootFS":{"Type":"layers","Layers":["sha256:destination-inspector-layer"]}}]`), nil
				}
				return nil, nil
			}})
			manager.directCLI = false
			manager.runtimeConnector = func(context.Context) (containerRuntime, error) { return backend, nil }
			matches, err := manager.migrationImageMatches(context.Background(), "synthetic:latest", source)
			if scenario == "missing source manifest" {
				if err == nil || matches || missingDockerObject(err, "image") {
					t.Fatalf("missing pinned OCI metadata was accepted: matches=%v, %v", matches, err)
				}
			} else if strings.HasPrefix(scenario, "missing") {
				if matches || !missingDockerObject(err, "image") {
					t.Fatalf("a missing destination blocked initial import: matches=%v, %v", matches, err)
				}
			} else if err != nil || matches != (scenario == "unchanged") {
				t.Fatalf("canonical configuration identity was not enforced: matches=%v, %v", matches, err)
			}
		})
	}
}

func TestMigrationNeverTreatsAReportedGuestVolumePathAsLocalHostData(t *testing.T) {
	host := t.TempDir()
	if err := os.WriteFile(filepath.Join(host, "synthetic-unrelated.txt"), []byte("not source volume data"), 0o600); err != nil {
		t.Fatal(err)
	}
	volume := map[string]any{"Name": "synthetic-source", "Driver": "local", "CreatedAt": "2026-01-01T00:00:00Z", "Mountpoint": host}
	manager := migrationFixtureManager(t, map[string]any{
		"/volumes":                  map[string]any{"Volumes": []map[string]any{volume}},
		"/volumes/synthetic-source": volume,
	}, nil)
	inventory, err := manager.MigrationInventory(context.Background(), "fixture-source")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := manager.PreviewMigration(context.Background(), dataops.Request{
		Context: "fixture-source", Selections: []dataops.Selection{{Kind: "volume", Name: "synthetic-source", ID: inventory.Objects[0].ID}},
	})
	if err != nil || len(preview.Conflicts) == 0 || !slices.Contains(preview.SourceHelpers, "synthetic-source") {
		t.Fatalf("a socket-forwarded guest path was mistaken for source data on the host: %+v, %v", preview, err)
	}
}
