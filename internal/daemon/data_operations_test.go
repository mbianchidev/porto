package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestDataOperationHistoryHTTPContract(t *testing.T) {
	server, _ := cleanupServerFixture(t)
	ctx := context.Background()
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	run, err := server.store.BeginDataOperation(ctx, dataops.Request{
		Action: "migration", Context: "synthetic-source", Confirm: true,
	}, started)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.FinishDataOperation(ctx, run.ID, "failed", dataops.Result{
		Steps: []dataops.Step{{
			Kind: "volume", Source: "synthetic-data", Destination: "synthetic-copy", Status: "succeeded", ID: "source-1",
		}},
	}, "synthetic later transfer failed", started.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	const operation = `{"id":1,"request":{"action":"migration","resource":{"kind":"","name":"","id":"","readOnly":false},"identity":"","context":"synthetic-source","confirm":true},"status":"failed","phase":"failed","bytes":0,"startedAt":"2026-01-01T00:00:00Z","completedAt":"2026-01-01T00:01:00Z","result":{"steps":[{"kind":"volume","source":"synthetic-data","destination":"synthetic-copy","status":"succeeded","id":"source-1"}]},"error":"synthetic later transfer failed"}`
	mux := http.NewServeMux()
	server.storageRoutes(mux)
	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/api/data/operations/1", body: operation},
		{path: "/api/data/operations", body: "[" + operation + "]"},
	} {
		t.Run(test.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("read data history: %d %s", response.Code, response.Body.String())
			}
			var got, want any
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(test.body), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("data API contract changed:\ngot:  %s\nwant: %s", response.Body.String(), test.body)
			}
		})
	}
}

func TestDataOperationRejectsIncompatibleJSON(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "unknown request field", body: `{"action":"migration","confirm":true,"preview":"synthetic","future":true}`},
		{name: "unknown resource field", body: `{"action":"volume-export","resource":{"name":"synthetic","future":true},"confirm":true,"preview":"synthetic"}`},
		{name: "wrong field type", body: `{"action":"migration","confirm":"true","preview":"synthetic"}`},
		{name: "multiple values", body: `{"action":"migration","confirm":true,"preview":"synthetic"} {}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, _ := cleanupServerFixture(t)
			response := httptest.NewRecorder()
			server.createDataOperation(response, httptest.NewRequest(http.MethodPost, "/api/data/operations", strings.NewReader(test.body)))
			if response.Code != http.StatusBadRequest || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/plain") {
				t.Fatalf("incompatible JSON did not return a plain-text 400: %d %s", response.Code, response.Body.String())
			}
			runs, err := server.store.DataOperations(context.Background())
			if err != nil || len(runs) != 0 {
				t.Fatalf("incompatible request reached the data worker: %+v, %v", runs, err)
			}
		})
	}
}

func TestManualDataRequestsRejectSchedulerFields(t *testing.T) {
	for _, field := range []string{"scheduleId", "retention", "directory", "trigger"} {
		t.Run(field, func(t *testing.T) {
			server, _ := cleanupServerFixture(t)
			server.dataExecutor = func(context.Context, dataops.Operation) (dataops.Result, error) {
				return dataops.Result{}, nil
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := server.stopDataOperations(ctx); err != nil {
					t.Error(err)
				}
			})
			request := map[string]any{
				"action": "volume-empty", "confirm": true, "preview": "synthetic-preview",
				"resource": datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"},
			}
			switch field {
			case "scheduleId":
				request[field] = 1
			case "retention":
				request[field] = 1
			case "directory":
				request[field] = "/synthetic/backups"
			case "trigger":
				request[field] = "scheduled"
			}
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.createDataOperation(response, httptest.NewRequest(http.MethodPost, "/api/data/operations", bytes.NewReader(body)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("manual request acquired internal scheduler privileges: %d %s", response.Code, response.Body.String())
			}
			runs, err := server.store.DataOperations(context.Background())
			if err != nil || len(runs) != 0 {
				t.Fatalf("invalid request reached the data worker: %+v, %v", runs, err)
			}
		})
	}
}

func backupArchiveFixture(t *testing.T, server *Server, resource datafiles.Resource, directory, name string) (dataops.Operation, dataops.Archive) {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "synthetic.txt"), []byte("synthetic backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(directory, name))
	if err != nil {
		t.Fatal(err)
	}
	_, exportErr := datafiles.Export(context.Background(), file, source, resource)
	if err := errors.Join(exportErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	archive, err := portodocker.InspectVolumeArchive(context.Background(), file.Name())
	if err != nil {
		t.Fatal(err)
	}
	run, err := server.store.BeginDataOperation(context.Background(), dataops.Request{
		Action: "volume-export", Resource: resource, Identity: resource.Fingerprint(),
		ScheduleID: 1, Retention: 1, Directory: directory, Trigger: "scheduled", Confirm: true,
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return *run, archive
}

func TestBackupRetentionRequiresDurableReplacement(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PORTO_HOME", home)
	directory := filepath.Join(home, "backups", "synthetic")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	server, _ := cleanupServerFixture(t)
	resource := datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"}
	old, oldArchive := backupArchiveFixture(t, server, resource, directory, "old.tar")
	if err := server.store.FinishDataOperation(context.Background(), old.ID, "succeeded", dataops.Result{Archive: &oldArchive}, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	newest, archive := backupArchiveFixture(t, server, resource, directory, "new.tar")
	err := server.applyBackupRetention(context.Background(), newest, archive)
	if err == nil {
		t.Fatal("retention ran before its replacement archive had a durable recovery record")
	}
	if _, err := os.Stat(oldArchive.Path); err != nil {
		t.Fatalf("last recorded recovery point was deleted: %v", err)
	}
}

func TestBackupScheduleCanBePausedWhenItsVolumeIsUnavailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PORTO_HOME", home)
	server, _ := cleanupServerFixture(t)
	schedule, err := server.store.SaveBackupSchedule(context.Background(), dataops.Schedule{
		Resource: datafiles.Resource{Kind: "volume", Name: "synthetic-missing", ID: "volume-1"},
		Enabled:  true, IntervalHours: 24, Retention: 7, Directory: filepath.Join(home, "backups", "synthetic"),
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	commands := 0
	server.docker = portodocker.New(runtimeRunnerFunc(func(context.Context, runtimes.Command) ([]byte, error) {
		commands++
		return nil, errors.New("synthetic volume is unavailable")
	}))
	schedule.Enabled = false
	body, err := json.Marshal(schedule)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.saveBackupSchedule(response, httptest.NewRequest(http.MethodPost, "/api/docker/backups", bytes.NewReader(body)))
	if response.Code != http.StatusOK || commands != 0 {
		t.Fatalf("an unavailable volume prevented pausing its failed schedule: %d %s, commands=%d", response.Code, response.Body.String(), commands)
	}
}

func TestBackupRetentionPreservesOldArchivesUntilDurableVerifiedReplacement(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified", true: "altered recovery point"}[corrupt], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("PORTO_HOME", home)
			directory := filepath.Join(home, "backups", "synthetic")
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			server, _ := cleanupServerFixture(t)
			resource := datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"}
			old, oldArchive := backupArchiveFixture(t, server, resource, directory, "old.tar")
			if err := server.store.FinishDataOperation(context.Background(), old.ID, "succeeded", dataops.Result{Archive: &oldArchive}, "", time.Now()); err != nil {
				t.Fatal(err)
			}
			if corrupt {
				if err := os.WriteFile(oldArchive.Path, []byte("altered synthetic archive"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			newest, archive := backupArchiveFixture(t, server, resource, directory, "new.tar")
			if err := server.store.RecordDataArchive(context.Background(), newest.ID, archive); err != nil {
				t.Fatal(err)
			}
			err := server.applyBackupRetention(context.Background(), newest, archive)
			if (err != nil) != corrupt {
				t.Fatalf("unexpected retention result: %v", err)
			}
			_, oldErr := os.Stat(oldArchive.Path)
			if corrupt && oldErr != nil || !corrupt && !errors.Is(oldErr, os.ErrNotExist) {
				t.Fatalf("retention violated archive ownership: %v", oldErr)
			}
			if _, err := portodocker.InspectVolumeArchive(context.Background(), archive.Path); err != nil {
				t.Fatalf("retention removed its last verified backup: %v", err)
			}
		})
	}
}
