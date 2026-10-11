package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/runtimes"
)

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
