package store

import (
	"context"
	"path/filepath"
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
