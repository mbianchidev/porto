package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mbianchidev/porto/internal/app"
)

var (
	ErrDockerCleanupRunning  = errors.New("Docker cleanup is already running")
	ErrDockerCleanupDisabled = errors.New("Docker runtime is disabled")
)

func (s *Store) DockerCleanupStatus(ctx context.Context) (app.DockerCleanupStatus, error) {
	status := app.DockerCleanupStatus{Runs: make([]app.DockerCleanupRun, 0)}
	var enabled, dockerEnabled int
	if err := s.db.QueryRowContext(ctx, `SELECT s.docker_auto_prune_enabled,s.docker_enabled,c.next_run_at
FROM settings s JOIN docker_cleanup_schedule c ON s.id=c.id WHERE s.id=1`).
		Scan(&enabled, &dockerEnabled, &status.NextRunAt); err != nil {
		return status, fmt.Errorf("read Docker cleanup schedule: %w", err)
	}
	status.Enabled = enabled == 1
	status.DockerEnabled = dockerEnabled == 1
	rows, err := s.db.QueryContext(ctx, `SELECT id,trigger,status,started_at,completed_at,result,error
FROM docker_cleanup_runs ORDER BY id DESC LIMIT 10`)
	if err != nil {
		return status, fmt.Errorf("read Docker cleanup results: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var run app.DockerCleanupRun
		var result string
		if err := rows.Scan(&run.ID, &run.Trigger, &run.Status, &run.StartedAt, &run.CompletedAt, &result, &run.Error); err != nil {
			return status, err
		}
		if err := json.Unmarshal([]byte(result), &run.Result); err != nil {
			return status, fmt.Errorf("decode Docker cleanup result %d: %w", run.ID, err)
		}
		status.Runs = append(status.Runs, run)
	}
	return status, rows.Err()
}

func (s *Store) BeginDockerCleanup(ctx context.Context, trigger app.DockerCleanupTrigger, now time.Time) (*app.DockerCleanupRun, error) {
	if trigger != app.CleanupManual && trigger != app.CleanupScheduled {
		return nil, fmt.Errorf("invalid Docker cleanup trigger %q", trigger)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var enabled, dockerEnabled int
	var nextRunAt string
	if err := tx.QueryRowContext(ctx, `SELECT s.docker_auto_prune_enabled,s.docker_enabled,c.next_run_at
FROM settings s JOIN docker_cleanup_schedule c ON s.id=c.id WHERE s.id=1`).
		Scan(&enabled, &dockerEnabled, &nextRunAt); err != nil {
		return nil, err
	}
	if trigger == app.CleanupScheduled {
		if enabled != 1 || dockerEnabled != 1 || nextRunAt == "" {
			return nil, nil
		}
		due, err := time.Parse(time.RFC3339Nano, nextRunAt)
		if err != nil {
			return nil, fmt.Errorf("parse Docker cleanup deadline: %w", err)
		}
		if now.Before(due) {
			return nil, nil
		}
	}
	if dockerEnabled != 1 {
		return nil, ErrDockerCleanupDisabled
	}
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM docker_cleanup_runs WHERE status='running'`).Scan(&running); err != nil {
		return nil, err
	}
	if running != 0 {
		return nil, ErrDockerCleanupRunning
	}
	run := &app.DockerCleanupRun{
		Trigger: trigger, Status: app.CleanupRunning,
		StartedAt: now.UTC().Format(time.RFC3339Nano), Result: app.NewDockerCleanupResult(),
	}
	result, err := json.Marshal(run.Result)
	if err != nil {
		return nil, err
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO docker_cleanup_runs(trigger,status,started_at,result) VALUES(?,?,?,?)`,
		run.Trigger, run.Status, run.StartedAt, string(result))
	if err != nil {
		return nil, err
	}
	if run.ID, err = inserted.LastInsertId(); err != nil {
		return nil, err
	}
	if err := scheduleDockerCleanup(ctx, tx, enabled == 1, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}

func (s *Store) FinishDockerCleanup(ctx context.Context, id int64, status app.CleanupStatus, result app.DockerCleanupResult, message string, now time.Time) error {
	switch status {
	case app.CleanupSucceeded, app.CleanupFailed, app.CleanupSkipped, app.CleanupInterrupted:
	default:
		return fmt.Errorf("invalid completed Docker cleanup status %q", status)
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode Docker cleanup result: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updated, err := tx.ExecContext(ctx, `UPDATE docker_cleanup_runs SET status=?,completed_at=?,result=?,error=? WHERE id=? AND status='running'`,
		status, now.UTC().Format(time.RFC3339Nano), string(data), message, id)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("Docker cleanup run %d is no longer active; refusing to overwrite its result", id)
	}
	var enabled int
	if err := tx.QueryRowContext(ctx, `SELECT docker_auto_prune_enabled FROM settings WHERE id=1`).Scan(&enabled); err != nil {
		return err
	}
	if err := scheduleDockerCleanup(ctx, tx, enabled == 1, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecoverDockerCleanup(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updated, err := tx.ExecContext(ctx, `UPDATE docker_cleanup_runs SET status='interrupted',completed_at=?,error=?
WHERE status='running'`, now.UTC().Format(time.RFC3339Nano),
		"Porto stopped before this cleanup finished. Some unused data may already have been removed.")
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 0 {
		var enabled int
		if err := tx.QueryRowContext(ctx, `SELECT docker_auto_prune_enabled FROM settings WHERE id=1`).Scan(&enabled); err != nil {
			return err
		}
		if err := scheduleDockerCleanup(ctx, tx, enabled == 1, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scheduleDockerCleanup(ctx context.Context, tx *sql.Tx, enabled bool, now time.Time) error {
	nextRunAt := ""
	if enabled {
		nextRunAt = now.UTC().Add(app.DockerCleanupInterval).Format(time.RFC3339Nano)
	}
	_, err := tx.ExecContext(ctx, `UPDATE docker_cleanup_schedule SET next_run_at=? WHERE id=1`, nextRunAt)
	return err
}
