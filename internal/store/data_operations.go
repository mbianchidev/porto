package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mbianchidev/porto/internal/dataops"
)

func (s *Store) migrateDataOperations() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
CREATE TABLE IF NOT EXISTS data_operations (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 resource_key TEXT NOT NULL,
 request TEXT NOT NULL,
 status TEXT NOT NULL,
 phase TEXT NOT NULL DEFAULT 'queued',
 bytes INTEGER NOT NULL DEFAULT 0,
 started_at TEXT NOT NULL,
 completed_at TEXT NOT NULL DEFAULT '',
 result TEXT NOT NULL DEFAULT '{}',
 error TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_data_operation_active ON data_operations(resource_key) WHERE status='running';
CREATE INDEX IF NOT EXISTS idx_data_operation_history ON data_operations(started_at);
CREATE TABLE IF NOT EXISTS volume_backup_schedules (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 resource_key TEXT NOT NULL UNIQUE,
 resource TEXT NOT NULL,
 enabled INTEGER NOT NULL,
 interval_hours INTEGER NOT NULL CHECK(interval_hours BETWEEN 1 AND 8760),
 retention INTEGER NOT NULL CHECK(retention BETWEEN 1 AND 365),
 directory TEXT NOT NULL,
 next_run_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_volume_backup_due ON volume_backup_schedules(enabled,next_run_at);
`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) BeginDataOperation(ctx context.Context, request dataops.Request, now time.Time) (*dataops.Operation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	run, err := beginDataOperation(ctx, tx, request, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}

func beginDataOperation(ctx context.Context, tx *sql.Tx, request dataops.Request, now time.Time) (*dataops.Operation, error) {
	document, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(document) > 1024*1024 {
		return nil, errors.New("data operation request exceeds its persistence limit")
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM data_operations WHERE resource_key=? AND status='running'`, request.ResourceKey()).Scan(&active); err != nil {
		return nil, err
	}
	if active != 0 {
		return nil, dataops.ErrBusy
	}
	run := &dataops.Operation{Request: request, Status: "running", Phase: "queued", StartedAt: now.UTC().Format(time.RFC3339Nano)}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO data_operations(resource_key,request,status,started_at) VALUES(?,?,'running',?)`,
		request.ResourceKey(), string(document), run.StartedAt)
	if err != nil {
		return nil, err
	}
	run.ID, err = inserted.LastInsertId()
	return run, err
}

const dataOperationColumns = `id,request,status,phase,bytes,started_at,completed_at,result,error`

func scanDataOperation(row scanner) (dataops.Operation, error) {
	var run dataops.Operation
	var request, result string
	if err := row.Scan(&run.ID, &request, &run.Status, &run.Phase, &run.Bytes, &run.StartedAt, &run.CompletedAt, &result, &run.Error); err != nil {
		return run, err
	}
	if err := json.Unmarshal([]byte(request), &run.Request); err != nil {
		return run, fmt.Errorf("decode data operation request %d: %w", run.ID, err)
	}
	if err := json.Unmarshal([]byte(result), &run.Result); err != nil {
		return run, fmt.Errorf("decode data operation result %d: %w", run.ID, err)
	}
	return run, nil
}

func (s *Store) DataOperation(ctx context.Context, id int64) (dataops.Operation, error) {
	return scanDataOperation(s.db.QueryRowContext(ctx, `SELECT `+dataOperationColumns+` FROM data_operations WHERE id=?`, id))
}

func (s *Store) DataOperations(ctx context.Context) ([]dataops.Operation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+dataOperationColumns+` FROM data_operations ORDER BY id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := make([]dataops.Operation, 0)
	for rows.Next() {
		run, err := scanDataOperation(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) DataProgress(ctx context.Context, id int64, phase string, bytes int64) error {
	if bytes < 0 || len(phase) > 512 {
		return errors.New("invalid data operation progress")
	}
	updated, err := s.db.ExecContext(ctx, `UPDATE data_operations SET phase=?,bytes=? WHERE id=? AND status='running'`, phase, bytes, id)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("data operation is no longer running")
	}
	return nil
}

func (s *Store) FinishDataOperation(ctx context.Context, id int64, status string, result dataops.Result, message string, now time.Time) error {
	switch status {
	case "succeeded", "failed", "cancelled", "interrupted", "skipped":
	default:
		return errors.New("invalid completed data operation status")
	}
	document, err := json.Marshal(result)
	if err != nil {
		return err
	}
	updated, err := s.db.ExecContext(ctx, `UPDATE data_operations SET status=?,phase=?,completed_at=?,result=?,error=? WHERE id=? AND status='running'`,
		status, status, now.UTC().Format(time.RFC3339Nano), string(document), message, id)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("refusing to overwrite a completed data operation")
	}
	return nil
}

func (s *Store) RecoverDataOperations(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE data_operations SET status='interrupted',phase='interrupted',completed_at=?,error=?
WHERE status='running'`, now.UTC().Format(time.RFC3339Nano),
		"Porto stopped before this operation finished. Some verified transfers or confirmed cleanup changes may already be committed; refresh inventory before retrying.")
	return err
}

func (s *Store) SaveBackupSchedule(ctx context.Context, schedule dataops.Schedule, now time.Time) (dataops.Schedule, error) {
	if err := schedule.Validate(); err != nil {
		return schedule, err
	}
	document, err := json.Marshal(schedule.Resource)
	if err != nil {
		return schedule, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return schedule, err
	}
	defer tx.Rollback()
	if schedule.ID != 0 {
		var existing string
		if err := tx.QueryRowContext(ctx, `SELECT resource_key FROM volume_backup_schedules WHERE id=?`, schedule.ID).Scan(&existing); err != nil {
			return schedule, err
		}
		if existing != schedule.Resource.Fingerprint() {
			return schedule, errors.New("refusing to retarget a schedule to a different volume identity")
		}
	}
	schedule.NextRunAt = schedule.Deadline(now)
	inserted, err := tx.ExecContext(ctx, `INSERT INTO volume_backup_schedules(resource_key,resource,enabled,interval_hours,retention,directory,next_run_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(resource_key) DO UPDATE SET enabled=excluded.enabled,interval_hours=excluded.interval_hours,retention=excluded.retention,
directory=excluded.directory,next_run_at=excluded.next_run_at`, schedule.Resource.Fingerprint(), string(document), boolInt(schedule.Enabled),
		schedule.IntervalHours, schedule.Retention, schedule.Directory, schedule.NextRunAt)
	if err != nil {
		return schedule, err
	}
	if schedule.ID, err = inserted.LastInsertId(); err != nil {
		return schedule, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM volume_backup_schedules WHERE resource_key=?`, schedule.Resource.Fingerprint()).Scan(&schedule.ID); err != nil {
		return schedule, err
	}
	return schedule, tx.Commit()
}

func scanBackupSchedule(row scanner) (dataops.Schedule, error) {
	var schedule dataops.Schedule
	var resource string
	var enabled int
	if err := row.Scan(&schedule.ID, &resource, &enabled, &schedule.IntervalHours, &schedule.Retention, &schedule.Directory, &schedule.NextRunAt); err != nil {
		return schedule, err
	}
	schedule.Enabled = enabled == 1
	if err := json.Unmarshal([]byte(resource), &schedule.Resource); err != nil {
		return schedule, err
	}
	return schedule, schedule.Validate()
}

func (s *Store) BackupSchedules(ctx context.Context) ([]dataops.Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,resource,enabled,interval_hours,retention,directory,next_run_at FROM volume_backup_schedules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	schedules := make([]dataops.Schedule, 0)
	for rows.Next() {
		schedule, err := scanBackupSchedule(rows)
		if err != nil {
			return nil, err
		}
		schedules = append(schedules, schedule)
	}
	return schedules, rows.Err()
}

func (s *Store) DeleteBackupSchedule(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM volume_backup_schedules WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return sql.ErrNoRows
	}
	return err
}

func (s *Store) ClaimBackup(ctx context.Context, now time.Time) (*dataops.Operation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	schedule, err := scanBackupSchedule(tx.QueryRowContext(ctx, `SELECT b.id,b.resource,b.enabled,b.interval_hours,b.retention,b.directory,b.next_run_at
FROM volume_backup_schedules b JOIN settings s ON s.id=1 WHERE b.enabled=1 AND s.docker_enabled=1 AND julianday(b.next_run_at)<=julianday(?)
ORDER BY julianday(b.next_run_at),b.id LIMIT 1`, now.UTC().Format(time.RFC3339Nano)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Move missed deadlines forward from now, not from their old deadline.
	// A restart cannot replay days of missed jobs in a catch-up storm.
	if _, err := tx.ExecContext(ctx, `UPDATE volume_backup_schedules SET next_run_at=? WHERE id=?`, schedule.Deadline(now), schedule.ID); err != nil {
		return nil, err
	}
	request := dataops.Request{
		Action: "volume-export", Resource: schedule.Resource, Identity: schedule.Resource.Fingerprint(),
		Directory: schedule.Directory, Confirm: true, ScheduleID: schedule.ID, Retention: schedule.Retention, Trigger: "scheduled",
	}
	run, err := beginDataOperation(ctx, tx, request, now)
	if errors.Is(err, dataops.ErrBusy) {
		return nil, tx.Commit()
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}

func (s *Store) VerifiedBackupOperations(ctx context.Context, scheduleID int64) ([]dataops.Operation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+dataOperationColumns+` FROM data_operations
WHERE status='succeeded' AND json_extract(request,'$.scheduleId')=? ORDER BY id DESC`, scheduleID)
	if err != nil {
		return nil, err
	}

	defer rows.Close()
	runs := make([]dataops.Operation, 0)
	for rows.Next() {
		run, err := scanDataOperation(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func (s *Store) ExpireBackupArchive(ctx context.Context, id int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE data_operations SET result=json_set(result,'$.archive.path',''),
phase='archive-expired' WHERE id=? AND status='succeeded'`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return errors.New("backup result changed; retention outcome could not be recorded")
	}
	return err
}
