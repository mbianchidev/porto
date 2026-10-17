package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
)

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
