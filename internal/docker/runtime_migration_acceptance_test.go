package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/store"
)

func (fixture *runtimeDataFixture) sourceResponse(response *http.Response) error {
	if fixture.sourceFault.Load() != 1 || !strings.HasSuffix(response.Request.URL.Path, "/archive") {
		return nil
	}
	if err := response.Body.Close(); err != nil {
		return err
	}
	payload := []byte("synthetic corrupt archive header")
	response.Body = io.NopCloser(bytes.NewReader(payload))
	response.ContentLength = int64(len(payload))
	response.Header.Set("Content-Length", fmt.Sprint(len(payload)))
	return nil
}

func (fixture *runtimeDataFixture) migrationRun(t *testing.T, selections []dataops.Selection, helpers bool, progress func(string, int64) error) (dataops.Result, error) {
	t.Helper()
	request := dataops.Request{Action: "migration", Context: fixture.source.context.Name, Selections: selections, AllowSourceHelper: helpers}
	preview, err := fixture.manager.PreviewMigration(fixture.ctx, request)
	if err != nil {
		return dataops.Result{}, err
	}
	if len(preview.Conflicts) != 0 {
		return dataops.Result{}, fmt.Errorf("migration dry run conflicts: %v", preview.Conflicts)
	}
	preview.Request.Confirm = true
	run, err := fixture.store.BeginDataOperation(fixture.ctx, preview.Request, time.Now())
	if err != nil {
		return dataops.Result{}, err
	}
	if progress == nil {
		progress = func(string, int64) error { return nil }
	}
	result, operationErr := fixture.manager.Migrate(fixture.ctx, preview.Request, filepath.Join(os.Getenv("PORTO_HOME"), "transfers"), progress, func(result dataops.Result) error {
		return fixture.store.CheckpointDataOperation(fixture.ctx, run.ID, result)
	})
	status, message := "succeeded", ""
	if operationErr != nil {
		status, message = "failed", operationErr.Error()
		if errors.Is(operationErr, context.Canceled) {
			status = "cancelled"
		}
	}
	return result, errors.Join(operationErr, fixture.store.FinishDataOperation(context.Background(), run.ID, status, result, message, time.Now()))
}

func (fixture *runtimeDataFixture) migrationFailures(t *testing.T) {
	volume := fixture.selection[slices.IndexFunc(fixture.selection, func(selection dataops.Selection) bool { return selection.Kind == "volume" })]
	t.Run("deterministic preflight boundaries", fixture.migrationPreflight)
	t.Run("corrupt source archive never creates a destination", func(t *testing.T) {
		selected := volume
		selected.Destination = fixture.prefix + "-corrupt-volume"
		fixture.extraSelections = append(fixture.extraSelections, selected)
		fixture.sourceFault.Store(1)
		result, err := fixture.migrationRun(t, []dataops.Selection{selected}, false, nil)
		fixture.sourceFault.Store(0)
		if err == nil || len(result.Steps) != 1 || result.Steps[0].Status != "failed" {
			t.Fatalf("corrupt archive was accepted or its outcome was lost: %+v, %v", result, err)
		}
		fixture.requireMissingVolume(t, selected.Destination)
	})
	t.Run("cancelled copy cleans its exact temporary resources", func(t *testing.T) {
		selected := volume
		selected.Destination = fixture.prefix + "-cancelled-volume"
		fixture.extraSelections = append(fixture.extraSelections, selected)
		result, err := fixture.migrationRun(t, []dataops.Selection{selected}, false, func(phase string, _ int64) error {
			if strings.HasPrefix(phase, "Copying read-only source volume") {
				return context.Canceled
			}
			return nil
		})
		if !errors.Is(err, context.Canceled) || len(result.Steps) != 1 || result.Steps[0].Status != "cancelled" {
			t.Fatalf("cancelled migration was not reported: %+v, %v", result, err)
		}
		fixture.requireMissingVolume(t, selected.Destination)
	})
	t.Run("actual low-space destination retains no partial volume", fixture.lowSpaceMigration)
	t.Run("daemon interruption retains successes and recovers never-started helpers", fixture.interruptedMigration)
}

func (fixture *runtimeDataFixture) requireMissingVolume(t *testing.T, name string) {
	t.Helper()
	volumes, err := fixture.manager.Volumes(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(volumes, func(volume Volume) bool { return volume.Name == name }) {
		t.Fatalf("failed migration left a destination volume %s", name)
	}
	pending, err := fixture.store.VolumeTemporaries(fixture.ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("failed migration left owned temporary reservations: %+v, %v", pending, err)
	}
}

type runtimeMigrationWorker struct {
	StateDir, Ledger, Directory, Helper, Prefix string
	Run                                         dataops.Operation
}

type runtimeMigrationWorkerSignal struct {
	Phase string `json:"phase,omitempty"`
	Error string `json:"error,omitempty"`
}

func TestRuntimeMigrationWorkerReportsSetupFailureAsJSON(t *testing.T) {
	worker := runtimeMigrationWorker{Prefix: "synthetic-worker", Ledger: t.TempDir()}
	document, err := json.Marshal(worker)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, workerErr := (runtimes.ExecRunner{}).Run(ctx, runtimes.Command{
		Name: os.Args[0], Args: []string{"-test.run=^TestRuntimeMigrationWorkerProcess$"},
		Env: []string{"PORTO_RUNTIME_MIGRATION_WORKER=" + path},
	})
	var signal runtimeMigrationWorkerSignal
	if err := json.NewDecoder(bytes.NewReader(output)).Decode(&signal); err != nil {
		t.Fatalf("failed worker did not report a structured error before test output: %v: %s", err, output)
	}
	if workerErr == nil || signal.Error == "" || signal.Phase != "" {
		t.Fatalf("failed worker advertised transfer readiness: signal=%+v error=%v", signal, workerErr)
	}
}

func TestRuntimeMigrationWorkerProcess(t *testing.T) {
	path := os.Getenv("PORTO_RUNTIME_MIGRATION_WORKER")
	if path == "" {
		t.Skip("isolated migration worker subprocess")
	}
	if err := runRuntimeMigrationWorker(path); err != nil {
		reportErr := json.NewEncoder(os.Stdout).Encode(runtimeMigrationWorkerSignal{Error: err.Error()})
		if reportErr != nil {
			fmt.Fprintf(os.Stderr, "report migration worker failure: %v\n", reportErr)
		}
		t.Fatal(errors.Join(err, reportErr))
	}
}

func runRuntimeMigrationWorker(path string) (err error) {
	document, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var worker runtimeMigrationWorker
	if err := json.Unmarshal(document, &worker); err != nil {
		return err
	}
	for _, selection := range worker.Run.Request.Selections {
		if !strings.HasPrefix(selection.Destination, worker.Prefix) {
			return errors.New("refusing a migration worker outside its fixture destinations")
		}
	}
	ledger, err := store.Open(worker.Ledger)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, ledger.Close()) }()
	manager := NewWithStateDir(nil, worker.StateDir)
	if worker.Helper != "" {
		manager.runner = storageIntegrationRunner{helper: worker.Helper}
	}
	manager.SetMigrationVolumeLedger(ledger.RecordMigratedVolume, ledger.KnownMigratedVolume)
	manager.SetMigrationTemporaryLedger(ledger.ReserveMigrationTemporary, ledger.ClearMigrationTemporary, ledger.MigrationTemporaries)
	manager.SetVolumeTransferLedger(ledger.ReserveVolumeTemporary, ledger.ClearVolumeTemporary, ledger.VolumeTemporaries)
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		if err := manager.StartContainerInventory(context.Background()); err != nil {
			return err
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			err = errors.Join(err, manager.StopContainerInventory(ctx))
		}()
		deadline := time.NewTimer(time.Minute)
		defer deadline.Stop()
		for !manager.ContainerSnapshot().Available {
			select {
			case <-deadline.C:
				return fmt.Errorf("migration worker inventory unavailable: %s", manager.ContainerSnapshot().Message)
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	_, err = manager.Migrate(context.Background(), worker.Run.Request, worker.Directory, func(phase string, _ int64) error {
		if strings.HasPrefix(phase, "Copying read-only source volume") {
			if err := json.NewEncoder(os.Stdout).Encode(runtimeMigrationWorkerSignal{Phase: phase}); err != nil {
				return err
			}
			var control [1]byte
			_, err := os.Stdin.Read(control[:])
			return err
		}
		return nil
	}, func(result dataops.Result) error {
		return ledger.CheckpointDataOperation(context.Background(), worker.Run.ID, result)
	})
	if err != nil {
		return err
	}
	return errors.New("the fixture migration worker was not interrupted at its transfer checkpoint")
}

func (fixture *runtimeDataFixture) interruptedMigration(t *testing.T) {
	name := fixture.prefix + "-unattached"
	fixture.sourceJSON(http.MethodPost, "/volumes/create", nil, map[string]any{
		"Name": name, "Labels": map[string]string{"porto.test.suite": fixture.prefix},
	}, nil)
	inventory, err := fixture.manager.MigrationInventory(fixture.ctx, fixture.source.context.Name)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(inventory.Objects, func(object MigrationObject) bool { return object.Kind == "volume" && object.Name == name })
	if index < 0 {
		t.Fatal("new unattached fixture volume is missing")
	}
	object := inventory.Objects[index]
	selected := dataops.Selection{Kind: "volume", Name: name, ID: object.ID, Destination: fixture.prefix + "-resumed-volume"}
	fixture.extraSelections = append(fixture.extraSelections, selected)
	image := fixture.selection[slices.IndexFunc(fixture.selection, func(selection dataops.Selection) bool { return selection.Kind == "image" })]
	request := dataops.Request{Action: "migration", Context: fixture.source.context.Name, Selections: []dataops.Selection{image, selected}}
	unconsented, err := fixture.manager.PreviewMigration(fixture.ctx, request)
	if err != nil || len(unconsented.Conflicts) == 0 {
		t.Fatalf("unattached volume did not require separate helper consent: %+v, %v", unconsented, err)
	}
	request.AllowSourceHelper = true
	preview, err := fixture.manager.PreviewMigration(fixture.ctx, request)
	if err != nil || len(preview.Conflicts) != 0 {
		t.Fatalf("consented unattached dry run: %+v, %v", preview.Conflicts, err)
	}
	preview.Request.Confirm = true
	run, err := fixture.store.BeginDataOperation(fixture.ctx, preview.Request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	worker := runtimeMigrationWorker{
		StateDir: fixture.manager.stateDir, Ledger: filepath.Join(fixture.root, "ledger.db"),
		Directory: filepath.Join(os.Getenv("PORTO_HOME"), "transfers"), Helper: fixture.destinationHelper, Prefix: fixture.prefix, Run: *run,
	}
	document, err := json.Marshal(worker)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.root, "worker.json")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatal(err)
	}
	child, err := (runtimes.ExecRunner{}).Start(fixture.ctx, runtimes.Command{
		Name: os.Args[0], Args: []string{"-test.run=^TestRuntimeMigrationWorkerProcess$"}, Env: []string{"PORTO_RUNTIME_MIGRATION_WORKER=" + path},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	var signal runtimeMigrationWorkerSignal
	decoder := json.NewDecoder(child.Stdout())
	if err := decoder.Decode(&signal); err != nil {
		killErr := child.Kill()
		buffered, outputErr := io.ReadAll(io.LimitReader(decoder.Buffered(), 64*1024))
		diagnostics, diagnosticErr := io.ReadAll(io.LimitReader(child.Stderr(), 64*1024))
		t.Fatalf("migration worker did not reach actual helper-backed transfer: %v: stdout=%q stderr=%q", errors.Join(err, killErr, outputErr, diagnosticErr), buffered, diagnostics)
	}
	if signal.Error != "" || !strings.HasPrefix(signal.Phase, "Copying read-only source volume") {
		t.Fatalf("migration worker failed before actual helper-backed transfer: %+v", signal)
	}
	if err := child.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil {
		t.Fatal("migration worker did not actually terminate")
	}
	if err := fixture.store.RecoverDataOperations(fixture.ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	recovered, err := fixture.store.DataOperation(fixture.ctx, run.ID)
	if err != nil || recovered.Status != "interrupted" || len(recovered.Result.Steps) != 2 ||
		recovered.Result.Steps[0].Status != "succeeded" || recovered.Result.Steps[1].Status != "interrupted" {
		t.Fatalf("real worker interruption lost exact object outcomes: %+v, %v", recovered, err)
	}
	temporaries, err := fixture.store.MigrationTemporaries(fixture.ctx)
	if err != nil || len(temporaries) != 2 {
		t.Fatalf("interrupted helper ownership was not durable: %+v, %v", temporaries, err)
	}
	for _, temporary := range temporaries {
		if temporary.Kind != "container" {
			continue
		}
		var container struct {
			State struct {
				Running bool
				Status  string
			}
			HostConfig struct{ ReadonlyRootfs bool }
			Mounts     []struct{ RW bool }
		}
		fixture.sourceJSON(http.MethodGet, "/containers/"+url.PathEscape(temporary.ID)+"/json", nil, nil, &container)
		if container.State.Running || container.State.Status != "created" || !container.HostConfig.ReadonlyRootfs || len(container.Mounts) != 1 || container.Mounts[0].RW {
			t.Fatalf("source access helper was not stopped and read-only: %+v", container)
		}
	}
	if err := fixture.manager.RecoverMigrationHelpers(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	temporaries, err = fixture.store.MigrationTemporaries(fixture.ctx)
	if err != nil || len(temporaries) != 0 {
		t.Fatalf("owned helper recovery did not finish: %+v, %v", temporaries, err)
	}
	result, err := fixture.migrationRun(t, request.Selections, true, nil)
	if err != nil || len(result.Steps) != 2 || slices.ContainsFunc(result.Steps, func(step dataops.Step) bool { return step.Status != "succeeded" }) {
		t.Fatalf("interrupted real migration could not resume: %+v, %v", result, err)
	}
	var original struct{ Name string }
	fixture.sourceJSON(http.MethodGet, "/volumes/"+url.PathEscape(name), nil, nil, &original)
	if original.Name != name {
		t.Fatal("source helper recovery removed the original source volume")
	}
}
