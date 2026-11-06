package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
)

func TestDataOperationMigrationIsAdditiveAndIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "synthetic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE synthetic_existing_state (value TEXT NOT NULL);
INSERT INTO synthetic_existing_state(value) VALUES('preserve synthetic state')`); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	for range 2 {
		if err := st.migrateDataOperations(); err != nil {
			t.Fatal(err)
		}
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM synthetic_existing_state`).Scan(&value); err != nil || value != "preserve synthetic state" {
		t.Fatalf("data initialization changed existing state: %q, %v", value, err)
	}
	var tables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN (
'data_operations','volume_backup_schedules','migration_volume_ledger','migration_source_temporaries','volume_transfer_temporaries'
)`).Scan(&tables); err != nil || tables != 5 {
		t.Fatalf("data tables were not initialized exactly once: %d, %v", tables, err)
	}
}

func TestDataOperationMigrationRollsBackPartialSchemaOnFailure(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "synthetic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE volume_backup_schedules (id INTEGER PRIMARY KEY, fixture TEXT NOT NULL);
INSERT INTO volume_backup_schedules(id,fixture) VALUES(7,'preserve synthetic schedule')`); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	if err := st.migrateDataOperations(); err == nil {
		t.Fatal("incompatible schedule schema did not fail data initialization")
	}
	var partial int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN (
'data_operations','idx_data_operation_active','idx_data_operation_history','idx_volume_backup_due',
'migration_volume_ledger','migration_source_temporaries','volume_transfer_temporaries'
)`).Scan(&partial); err != nil || partial != 0 {
		t.Fatalf("failed migration left a partial data schema: %d, %v", partial, err)
	}
	var value string
	if err := db.QueryRow(`SELECT fixture FROM volume_backup_schedules WHERE id=7`).Scan(&value); err != nil || value != "preserve synthetic schedule" {
		t.Fatalf("failed migration changed existing state: %q, %v", value, err)
	}
}

func TestDataOperationReadsLegacyJSONWithoutRewritingIt(t *testing.T) {
	database := filepath.Join(t.TempDir(), "synthetic.db")
	st, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if st != nil {
			if err := st.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	const request = `{"action":"migration","context":"synthetic-source","selections":[{"kind":"volume","name":"synthetic-data","id":"source-1"}]}`
	const result = `{"steps":[{"kind":"volume","source":"synthetic-data","destination":"synthetic-copy","status":"succeeded","id":"source-1"}]}`
	const started = "2026-01-01T00:00:00Z"
	const completed = "2026-01-01T00:01:00Z"
	if _, err := st.db.Exec(`INSERT INTO data_operations(id,resource_key,request,status,phase,bytes,started_at,completed_at,result,error)
VALUES(7,'runtime-data',?,'failed','failed',64,?,?,?,'synthetic later transfer failed')`, request, started, completed, result); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(database)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.DataOperation(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	want := dataops.Operation{
		ID: 7,
		Request: dataops.Request{
			Action: "migration", Context: "synthetic-source",
			Selections: []dataops.Selection{{Kind: "volume", Name: "synthetic-data", ID: "source-1"}},
		},
		Status: "failed", Phase: "failed", Bytes: 64, StartedAt: started, CompletedAt: completed,
		Result: dataops.Result{Steps: []dataops.Step{{
			Kind: "volume", Source: "synthetic-data", Destination: "synthetic-copy", Status: "succeeded", ID: "source-1",
		}}},
		Error: "synthetic later transfer failed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy data operation changed meaning:\ngot:  %+v\nwant: %+v", got, want)
	}
	if err := st.RecoverDataOperations(context.Background(), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	var savedRequest, savedResult string
	if err := st.db.QueryRow(`SELECT request,result FROM data_operations WHERE id=7`).Scan(&savedRequest, &savedResult); err != nil {
		t.Fatal(err)
	}
	if savedRequest != request || savedResult != result {
		t.Fatalf("reopening or recovery rewrote terminal history: request=%s result=%s", savedRequest, savedResult)
	}
}

func TestDataRecoveryRejectsCorruptJSONWithoutChangingHistory(t *testing.T) {
	for _, field := range []string{"request", "result"} {
		t.Run(field, func(t *testing.T) {
			st, err := Open(filepath.Join(t.TempDir(), "synthetic.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for _, id := range []string{"synthetic-first", "synthetic-second"} {
				if _, err := st.BeginDataOperation(context.Background(), dataops.Request{
					Action: "volume-export", Resource: datafiles.Resource{Kind: "volume", Name: id, ID: id},
				}, now); err != nil {
					t.Fatal(err)
				}
			}
			statement := `UPDATE data_operations SET request='{"action":1}' WHERE id=2`
			if field == "result" {
				statement = `UPDATE data_operations SET result='{"steps":"invalid"}' WHERE id=2`
			}
			if _, err := st.db.Exec(statement); err != nil {
				t.Fatal(err)
			}
			if err := st.RecoverDataOperations(context.Background(), now.Add(time.Minute)); err == nil ||
				!strings.Contains(err.Error(), "decode data operation "+field) {
				t.Fatalf("corrupt %s did not produce an explicit decode failure: %v", field, err)
			}
			var unchanged int
			if err := st.db.QueryRow(`SELECT COUNT(*) FROM data_operations WHERE status='running' AND phase='queued' AND completed_at=''`).Scan(&unchanged); err != nil || unchanged != 2 {
				t.Fatalf("failed recovery partially rewrote history: %d, %v", unchanged, err)
			}
		})
	}
}

func TestBackupScheduleSurvivesRestartWithoutCatchUpStorm(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "fixture.db")
	st, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	schedule, err := st.SaveBackupSchedule(ctx, dataops.Schedule{
		Resource: datafiles.Resource{Kind: "volume", Name: "fixture", ID: "synthetic-volume"},
		Enabled:  true, IntervalHours: 24, Retention: 2, Directory: t.TempDir(),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	later := now.Add(7 * 24 * time.Hour)
	run, err := st.ClaimBackup(ctx, later)
	if err != nil || run == nil || run.Request.ScheduleID != schedule.ID {
		t.Fatalf("missed schedule was not claimed: %+v %v", run, err)
	}
	again, err := st.ClaimBackup(ctx, later)
	if err != nil || again != nil {
		t.Fatalf("schedule overlapped or caught up repeatedly: %+v %v", again, err)
	}
	if err := st.RecoverDataOperations(ctx, later.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.DataOperation(ctx, run.ID)
	if err != nil || recovered.Status != "interrupted" {
		t.Fatalf("interrupted backup was hidden: %+v %v", recovered, err)
	}
}

func TestBackupClaimWaitsForOtherDataJobsWithoutAdvancingDeadline(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "synthetic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	schedule, err := st.SaveBackupSchedule(context.Background(), dataops.Schedule{
		Resource: datafiles.Resource{Kind: "volume", Name: "synthetic-backup", ID: "volume-1"},
		Enabled:  true, IntervalHours: 24, Retention: 2, Directory: t.TempDir(),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	manual, err := st.BeginDataOperation(context.Background(), dataops.Request{
		Action: "volume-clone", Resource: datafiles.Resource{Kind: "volume", Name: "synthetic-manual", ID: "volume-2"},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	due := now.Add(7 * 24 * time.Hour)
	run, err := st.ClaimBackup(context.Background(), due)
	if err != nil || run != nil {
		t.Fatalf("scheduled backup overlapped another data job: %+v, %v", run, err)
	}
	schedules, err := st.BackupSchedules(context.Background())
	if err != nil || len(schedules) != 1 || schedules[0].NextRunAt != schedule.NextRunAt {
		t.Fatalf("busy backup silently lost its deadline: %+v, %v", schedules, err)
	}
	if err := st.FinishDataOperation(context.Background(), manual.ID, "succeeded", dataops.Result{}, "", due); err != nil {
		t.Fatal(err)
	}
	run, err = st.ClaimBackup(context.Background(), due)
	if err != nil || run == nil || run.Request.ScheduleID != schedule.ID {
		t.Fatalf("missed backup did not run once after the data worker became idle: %+v, %v", run, err)
	}
}

func TestVerifiedBackupRecoveryRecordSurvivesInterruptedDaemon(t *testing.T) {
	database := filepath.Join(t.TempDir(), "synthetic.db")
	st, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resource := datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	schedule, err := st.SaveBackupSchedule(ctx, dataops.Schedule{
		Resource: resource, Enabled: true, IntervalHours: 24, Retention: 2, Directory: t.TempDir(),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.ClaimBackup(ctx, now.Add(24*time.Hour))
	if err != nil || run == nil {
		t.Fatalf("claim backup: %+v, %v", run, err)
	}
	archive := dataops.Archive{
		Path: filepath.Join(schedule.Directory, "synthetic.tar"), Resource: resource,
		SHA256: strings.Repeat("a", 64), Manifest: strings.Repeat("b", 64), Bytes: 1024, Consistency: "crash-consistent",
	}
	if err := st.RecordDataArchive(ctx, run.ID, archive); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.RecoverDataOperations(ctx, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	recovery, err := st.VerifiedBackupOperations(ctx, schedule.ID)
	if err != nil || len(recovery) != 1 || recovery[0].Status != "interrupted" ||
		recovery[0].Result.Archive == nil || recovery[0].Result.Archive.SHA256 != archive.SHA256 {
		t.Fatalf("the last durable recovery point was lost on restart: %+v, %v", recovery, err)
	}
}

func TestVolumeTransferReservationsSurviveRestartUntilExactCleanup(t *testing.T) {
	database := filepath.Join(t.TempDir(), "synthetic.db")
	st, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	temporary := dataops.VolumeTemporary{Owner: strings.Repeat("a", 32), Destination: "synthetic-transfer"}
	if err := st.ReserveVolumeTemporary(ctx, temporary); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ClearVolumeTemporary(ctx, dataops.VolumeTemporary{Owner: "foreign", Destination: temporary.Destination}); err != nil {
		t.Fatal(err)
	}
	pending, err := st.VolumeTemporaries(ctx)
	if err != nil || len(pending) != 1 || pending[0] != temporary {
		t.Fatalf("restarted recovery lost or broadened ownership: %+v, %v", pending, err)
	}
	if err := st.ClearVolumeTemporary(ctx, temporary); err != nil {
		t.Fatal(err)
	}
	pending, err = st.VolumeTemporaries(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("proven cleanup did not clear its exact reservation: %+v, %v", pending, err)
	}
}

func TestMigrationCheckpointsRetainExactOutcomesAcrossDaemonInterruption(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "synthetic.db")
	st, err := Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	run, err := st.BeginDataOperation(ctx, dataops.Request{Action: "migration", Context: "synthetic-source"}, now)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := dataops.Result{Steps: []dataops.Step{
		{Kind: "image", Source: "synthetic:latest", Destination: "synthetic:latest", ID: "image-1", Status: "succeeded"},
		{Kind: "volume", Source: "synthetic-data", Destination: "synthetic-data", ID: "volume-1", Status: "running"},
		{Kind: "container", Source: "synthetic-app", Destination: "synthetic-app", ID: "container-1", Status: "pending"},
	}}
	if err := st.CheckpointDataOperation(ctx, run.ID, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecoverDataOperations(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.DataOperation(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	steps := recovered.Result.Steps
	if recovered.Status != "interrupted" || len(steps) != 3 ||
		steps[0].Status != "succeeded" || steps[0].ID != "image-1" ||
		steps[1].Status != "interrupted" || steps[1].ID != "volume-1" ||
		steps[2].Status != "pending" {
		t.Fatalf("interruption lost or misreported per-object outcomes: %+v", recovered)
	}
	if err := st.CheckpointDataOperation(ctx, run.ID, checkpoint); err == nil {
		t.Fatal("a stale checkpoint overwrote a completed operation")
	}
}
