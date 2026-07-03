package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	return s, s.migrate()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS projects (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL,
 path TEXT NOT NULL UNIQUE,
 strategy TEXT NOT NULL,
 command TEXT NOT NULL,
 port INTEGER DEFAULT 0,
 pinned_port INTEGER DEFAULT 0,
 hostname TEXT DEFAULT '',
 pid INTEGER DEFAULT 0,
 status TEXT DEFAULT 'stopped',
 auto_start INTEGER DEFAULT 0,
 last_started TEXT DEFAULT '',
 updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS logs (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 project_id INTEGER NOT NULL,
 stream TEXT NOT NULL,
 line TEXT NOT NULL,
 created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_logs_project_created ON logs(project_id, created_at);
`)
	return err
}

func (s *Store) UpsertProject(ctx context.Context, p app.Project) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if p.Hostname == "" {
		p.Hostname = safeHost(p.Name)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO projects(name,path,strategy,command,hostname,updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(path) DO UPDATE SET name=excluded.name, strategy=excluded.strategy, command=excluded.command,
hostname=CASE WHEN projects.hostname='' THEN excluded.hostname ELSE projects.hostname END, updated_at=excluded.updated_at`, p.Name, p.Path, p.Strategy, p.Command, p.Hostname, now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	if id != 0 {
		return id, nil
	}
	var existing int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE path=?`, p.Path).Scan(&existing)
	return existing, err
}

func (s *Store) ListProjects(ctx context.Context) ([]app.Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,path,strategy,command,port,pinned_port,hostname,pid,status,auto_start,last_started,updated_at FROM projects ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProject(ctx context.Context, name string) (app.Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,path,strategy,command,port,pinned_port,hostname,pid,status,auto_start,last_started,updated_at FROM projects WHERE name=? OR id=CAST(? AS INTEGER)`, name, name)
	return scanProject(row)
}

func (s *Store) UsedPorts(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT port FROM projects WHERE port > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		m[p] = true
	}
	return m, rows.Err()
}

func (s *Store) SetRuntime(ctx context.Context, id int64, status string, pid, port int) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET status=?, pid=?, port=?, last_started=CASE WHEN ?='running' THEN ? ELSE last_started END, updated_at=? WHERE id=?`, status, pid, port, status, now, now, id)
	return err
}

func (s *Store) SetPinnedPort(ctx context.Context, name string, port int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET pinned_port=?, port=CASE WHEN status='running' THEN port ELSE ? END, updated_at=? WHERE name=? OR id=CAST(? AS INTEGER)`, port, port, time.Now().UTC().Format(time.RFC3339Nano), name, name)
	return err
}

func (s *Store) SetHostname(ctx context.Context, name, host string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET hostname=?, updated_at=? WHERE name=? OR id=CAST(? AS INTEGER)`, host, time.Now().UTC().Format(time.RFC3339Nano), name, name)
	return err
}

func (s *Store) AddLog(ctx context.Context, id int64, stream, line string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO logs(project_id, stream, line, created_at) VALUES(?,?,?,?)`, id, stream, line, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) Logs(ctx context.Context, id int64, limit int) ([]app.LogLine, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT project_id,stream,line,created_at FROM logs WHERE project_id=? ORDER BY created_at DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rev []app.LogLine
	for rows.Next() {
		var l app.LogLine
		var ts string
		if err := rows.Scan(&l.ProjectID, &l.Stream, &l.Line, &ts); err != nil {
			return nil, err
		}
		l.CreatedAt, _ = time.Parse(time.RFC3339Nano, ts)
		rev = append(rev, l)
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanProject(row scanner) (app.Project, error) {
	var p app.Project
	var auto int
	var last, updated string
	err := row.Scan(&p.ID, &p.Name, &p.Path, &p.Strategy, &p.Command, &p.Port, &p.PinnedPort, &p.Hostname, &p.PID, &p.Status, &auto, &last, &updated)
	if err != nil {
		return p, err
	}
	p.AutoStart = auto == 1
	p.LastStarted, _ = time.Parse(time.RFC3339Nano, last)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return p, nil
}

func safeHost(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return fmt.Sprintf("project-%d", time.Now().Unix())
	}
	return out
}
