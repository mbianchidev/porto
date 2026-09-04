package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type KubernetesRoute struct {
	Context     string `json:"context"`
	Namespace   string `json:"namespace"`
	Service     string `json:"service"`
	ServicePort int32  `json:"servicePort"`
	Hostname    string `json:"hostname"`
}

var defaultProtectedBranches = []string{
	"main",
	"master",
	"develop",
	"development",
	"staging",
	"production",
	"release/*",
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.migrateProjectPaths(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.migrateGeneratedHostnames(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.ensureUniqueHostnames(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
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
 base_hostname TEXT NOT NULL DEFAULT '',
 source_path TEXT NOT NULL DEFAULT '',
 managed_instance INTEGER NOT NULL DEFAULT 0,
 instance_branch TEXT NOT NULL DEFAULT '',
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
CREATE INDEX IF NOT EXISTS idx_logs_project_stream_created ON logs(project_id, stream, created_at);
CREATE TABLE IF NOT EXISTS kubernetes_routes (
 context_name TEXT NOT NULL,
 namespace TEXT NOT NULL,
 service_name TEXT NOT NULL,
 service_port INTEGER NOT NULL,
 hostname TEXT NOT NULL UNIQUE,
 updated_at TEXT NOT NULL,
 PRIMARY KEY(context_name, namespace, service_name, service_port)
);
CREATE INDEX IF NOT EXISTS idx_kubernetes_routes_context ON kubernetes_routes(context_name);
CREATE TABLE IF NOT EXISTS settings (
 id INTEGER PRIMARY KEY CHECK (id = 1),
 cleanup_local_merged INTEGER NOT NULL DEFAULT 0,
 cleanup_remote_merged INTEGER NOT NULL DEFAULT 0,
 prune_remote_tracking INTEGER NOT NULL DEFAULT 1,
 protected_branches TEXT NOT NULL,
 sql_not_so_lite_enabled INTEGER NOT NULL DEFAULT 0,
 kill_switch_enabled INTEGER NOT NULL DEFAULT 0,
 sendbox_enabled INTEGER NOT NULL DEFAULT 0,
 docker_enabled INTEGER NOT NULL DEFAULT 1,
 kubernetes_enabled INTEGER NOT NULL DEFAULT 0,
 vms_enabled INTEGER NOT NULL DEFAULT 0
);
`)
	if err != nil {
		return err
	}
	if err := s.ensureSettingsColumn("sql_not_so_lite_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureSettingsColumn("kill_switch_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureSettingsColumn("sendbox_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureSettingsColumn("docker_enabled", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := s.ensureSettingsColumn("kubernetes_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.ensureSettingsColumn("vms_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	for name, definition := range map[string]string{
		"base_hostname":    "TEXT NOT NULL DEFAULT ''",
		"source_path":      "TEXT NOT NULL DEFAULT ''",
		"managed_instance": "INTEGER NOT NULL DEFAULT 0",
		"instance_branch":  "TEXT NOT NULL DEFAULT ''",
	} {
		if err := s.ensureProjectColumn(name, definition); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(`UPDATE projects SET source_path=path WHERE source_path='';
UPDATE projects SET base_hostname=hostname WHERE base_hostname=''`); err != nil {
		return err
	}
	protected, err := json.Marshal(defaultProtectedBranches)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO settings(id, protected_branches, docker_enabled) VALUES(1, ?, 1)`, string(protected))
	return err
}

func (s *Store) UpsertProject(ctx context.Context, p app.Project) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	p.Path = canonicalProjectPath(p.Path)
	if p.Hostname == "" {
		p.Hostname = safeHost(p.Name)
	}
	if p.BaseHostname == "" {
		p.BaseHostname = p.Hostname
	}
	if p.SourcePath == "" {
		p.SourcePath = p.Path
	}
	p.SourcePath = canonicalProjectPath(p.SourcePath)
	hostname, err := s.availableHostname(ctx, p.Path, p.Hostname)
	if err != nil {
		return 0, err
	}
	p.Hostname = hostname
	res, err := s.db.ExecContext(ctx, `INSERT INTO projects(name,path,strategy,command,hostname,base_hostname,source_path,managed_instance,instance_branch,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(path) DO UPDATE SET name=excluded.name, strategy=excluded.strategy, command=excluded.command,
hostname=CASE WHEN projects.hostname='' OR projects.hostname=? THEN excluded.hostname ELSE projects.hostname END, updated_at=excluded.updated_at`,
		p.Name, p.Path, p.Strategy, p.Command, p.Hostname, p.BaseHostname, p.SourcePath, boolInt(p.ManagedInstance), p.Branch, now, legacySafeHost(p.Name))
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
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,path,strategy,command,port,pinned_port,hostname,base_hostname,source_path,managed_instance,instance_branch,pid,status,auto_start,last_started,updated_at FROM projects ORDER BY name,managed_instance,instance_branch`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []app.Project{}
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
	row := s.db.QueryRowContext(ctx, `SELECT id,name,path,strategy,command,port,pinned_port,hostname,base_hostname,source_path,managed_instance,instance_branch,pid,status,auto_start,last_started,updated_at FROM projects WHERE name=? OR id=CAST(? AS INTEGER) ORDER BY CASE WHEN id=CAST(? AS INTEGER) THEN 0 ELSE 1 END LIMIT 1`, name, name, name)
	return scanProject(row)
}

func (s *Store) GetProjectByID(ctx context.Context, id int64) (app.Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,path,strategy,command,port,pinned_port,hostname,base_hostname,source_path,managed_instance,instance_branch,pid,status,auto_start,last_started,updated_at FROM projects WHERE id=?`, id)
	return scanProject(row)
}

func (s *Store) GetProjectByPath(ctx context.Context, path string) (app.Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,path,strategy,command,port,pinned_port,hostname,base_hostname,source_path,managed_instance,instance_branch,pid,status,auto_start,last_started,updated_at FROM projects WHERE path=?`, canonicalProjectPath(path))
	return scanProject(row)
}

func (s *Store) GetProjectByHostname(ctx context.Context, hostname string) (app.Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,path,strategy,command,port,pinned_port,hostname,base_hostname,source_path,managed_instance,instance_branch,pid,status,auto_start,last_started,updated_at FROM projects WHERE hostname=?`, hostname)
	return scanProject(row)
}

func (s *Store) UpsertKubernetesRoute(ctx context.Context, route KubernetesRoute) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kubernetes_routes(context_name,namespace,service_name,service_port,hostname,updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(context_name,namespace,service_name,service_port) DO UPDATE SET hostname=excluded.hostname,updated_at=excluded.updated_at`,
		route.Context,
		route.Namespace,
		route.Service,
		route.ServicePort,
		route.Hostname,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

func (s *Store) EnsureKubernetesRoute(ctx context.Context, route KubernetesRoute) (KubernetesRoute, error) {
	existing, err := s.GetKubernetesRoute(ctx, route.Context, route.Namespace, route.Service, route.ServicePort)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return KubernetesRoute{}, err
	}
	available, err := s.hostnameAvailable(ctx, route.Hostname)
	if err != nil {
		return KubernetesRoute{}, err
	}
	if !available {
		base := route.Hostname
		suffix := shortHash(route.Context + "/" + route.Namespace + "/" + route.Service + "/" + strconv.Itoa(int(route.ServicePort)))
		route.Hostname = base + "-" + suffix
		for attempt := 2; ; attempt++ {
			available, err = s.hostnameAvailable(ctx, route.Hostname)
			if err != nil {
				return KubernetesRoute{}, err
			}
			if available {
				break
			}
			route.Hostname = fmt.Sprintf("%s-%s-%d", base, suffix, attempt)
		}
	}
	if err := s.UpsertKubernetesRoute(ctx, route); err != nil {
		return KubernetesRoute{}, err
	}
	return route, nil
}

func (s *Store) GetKubernetesRoute(
	ctx context.Context,
	contextName string,
	namespace string,
	service string,
	servicePort int32,
) (KubernetesRoute, error) {
	var route KubernetesRoute
	err := s.db.QueryRowContext(ctx, `SELECT context_name,namespace,service_name,service_port,hostname
FROM kubernetes_routes WHERE context_name=? AND namespace=? AND service_name=? AND service_port=?`,
		contextName,
		namespace,
		service,
		servicePort,
	).Scan(
		&route.Context,
		&route.Namespace,
		&route.Service,
		&route.ServicePort,
		&route.Hostname,
	)
	return route, err
}

func (s *Store) GetKubernetesRouteByHostname(ctx context.Context, hostname string) (KubernetesRoute, error) {
	var route KubernetesRoute
	err := s.db.QueryRowContext(ctx, `SELECT context_name,namespace,service_name,service_port,hostname
FROM kubernetes_routes WHERE hostname=?`, hostname).Scan(
		&route.Context,
		&route.Namespace,
		&route.Service,
		&route.ServicePort,
		&route.Hostname,
	)
	return route, err
}

func (s *Store) ListKubernetesRoutes(ctx context.Context, contextName string) ([]KubernetesRoute, error) {
	query := `SELECT context_name,namespace,service_name,service_port,hostname FROM kubernetes_routes`
	args := []any{}
	if contextName != "" {
		query += ` WHERE context_name=?`
		args = append(args, contextName)
	}
	query += ` ORDER BY context_name,namespace,service_name,service_port`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	routes := make([]KubernetesRoute, 0)
	for rows.Next() {
		var route KubernetesRoute
		if err := rows.Scan(&route.Context, &route.Namespace, &route.Service, &route.ServicePort, &route.Hostname); err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	return routes, rows.Err()
}

func (s *Store) DeleteKubernetesRoute(
	ctx context.Context,
	contextName string,
	namespace string,
	service string,
	servicePort int32,
) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kubernetes_routes
WHERE context_name=? AND namespace=? AND service_name=? AND service_port=?`,
		contextName,
		namespace,
		service,
		servicePort,
	)
	return err
}

func (s *Store) DeleteKubernetesRoutesByContext(ctx context.Context, contextName string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kubernetes_routes WHERE context_name=?`, contextName)
	return err
}

func (s *Store) RenameKubernetesRoutesContext(ctx context.Context, oldContext, newContext string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE kubernetes_routes SET context_name=?,updated_at=? WHERE context_name=?`,
		newContext,
		time.Now().UTC().Format(time.RFC3339Nano),
		oldContext,
	)
	return err
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

func (s *Store) SetBranchIdentity(ctx context.Context, id int64, branch, hostname string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET instance_branch=?,hostname=?,updated_at=? WHERE id=?`,
		branch, hostname, time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *Store) HostnameExists(ctx context.Context, hostname string, exceptID int64) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT
	(SELECT COUNT(*) FROM projects WHERE hostname=? AND id<>?) +
	(SELECT COUNT(*) FROM kubernetes_routes WHERE hostname=?)`,
		hostname,
		exceptID,
		hostname,
	).Scan(&count)
	return count > 0, err
}

func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE project_id=?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) Settings(ctx context.Context) (app.Settings, error) {
	var settings app.Settings
	var cleanupLocal, cleanupRemote, prune, sqlNotSoLiteEnabled, killSwitchEnabled, sendboxEnabled int
	var dockerEnabled, kubernetesEnabled, vmsEnabled int
	var protected string
	err := s.db.QueryRowContext(ctx, `SELECT cleanup_local_merged,cleanup_remote_merged,prune_remote_tracking,protected_branches,sql_not_so_lite_enabled,kill_switch_enabled,sendbox_enabled,docker_enabled,kubernetes_enabled,vms_enabled FROM settings WHERE id=1`).
		Scan(&cleanupLocal, &cleanupRemote, &prune, &protected, &sqlNotSoLiteEnabled, &killSwitchEnabled, &sendboxEnabled, &dockerEnabled, &kubernetesEnabled, &vmsEnabled)
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal([]byte(protected), &settings.ProtectedBranches); err != nil {
		return settings, fmt.Errorf("decode protected branches: %w", err)
	}
	settings.CleanupLocalMerged = cleanupLocal == 1
	settings.CleanupRemoteMerged = cleanupRemote == 1
	settings.PruneRemoteTracking = prune == 1
	settings.SQLNotSoLiteEnabled = sqlNotSoLiteEnabled == 1
	settings.KillSwitchEnabled = killSwitchEnabled == 1
	settings.SendboxEnabled = sendboxEnabled == 1
	settings.DockerEnabled = dockerEnabled == 1
	settings.KubernetesEnabled = kubernetesEnabled == 1
	settings.VMsEnabled = vmsEnabled == 1
	return settings, nil
}

func (s *Store) SetSettings(ctx context.Context, settings app.Settings) error {
	protected, err := json.Marshal(settings.ProtectedBranches)
	if err != nil {
		return fmt.Errorf("encode protected branches: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE settings SET cleanup_local_merged=?,cleanup_remote_merged=?,prune_remote_tracking=?,protected_branches=?,sql_not_so_lite_enabled=?,kill_switch_enabled=?,sendbox_enabled=?,docker_enabled=?,kubernetes_enabled=?,vms_enabled=? WHERE id=1`,
		boolInt(settings.CleanupLocalMerged),
		boolInt(settings.CleanupRemoteMerged),
		boolInt(settings.PruneRemoteTracking),
		string(protected),
		boolInt(settings.SQLNotSoLiteEnabled),
		boolInt(settings.KillSwitchEnabled),
		boolInt(settings.SendboxEnabled),
		boolInt(settings.DockerEnabled),
		boolInt(settings.KubernetesEnabled),
		boolInt(settings.VMsEnabled),
	)
	return err
}

func (s *Store) ensureSettingsColumn(name, definition string) error {
	rows, err := s.db.Query(`PRAGMA table_info(settings)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if columnName == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf(`ALTER TABLE settings ADD COLUMN %s %s`, name, definition))
	return err
}

func (s *Store) ensureProjectColumn(name, definition string) error {
	rows, err := s.db.Query(`PRAGMA table_info(projects)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if columnName == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf(`ALTER TABLE projects ADD COLUMN %s %s`, name, definition))
	return err
}

func (s *Store) deduplicateHostnames() error {
	rows, err := s.db.Query(`SELECT id,path,hostname FROM projects ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := map[string]bool{}
	type update struct {
		id       int64
		hostname string
	}
	var updates []update
	for rows.Next() {
		var id int64
		var path, hostname string
		if err := rows.Scan(&id, &path, &hostname); err != nil {
			return err
		}
		if !seen[hostname] {
			seen[hostname] = true
			continue
		}
		candidate := hostname + "-" + shortHash(path)
		for seen[candidate] {
			candidate += "x"
		}
		seen[candidate] = true
		updates = append(updates, update{id: id, hostname: candidate})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range updates {
		if _, err := s.db.Exec(`UPDATE projects SET hostname=? WHERE id=?`, item.hostname, item.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ensureUniqueHostnames() error {
	if err := s.deduplicateHostnames(); err != nil {
		return err
	}
	_, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_hostname ON projects(hostname)`)
	return err
}

func (s *Store) availableHostname(ctx context.Context, path, hostname string) (string, error) {
	var existingPath string
	err := s.db.QueryRowContext(ctx, `SELECT path FROM projects WHERE hostname=?`, hostname).Scan(&existingPath)
	if errors.Is(err, sql.ErrNoRows) {
		available, availableErr := s.hostnameAvailable(ctx, hostname)
		if availableErr != nil {
			return "", availableErr
		}
		if available {
			return hostname, nil
		}
	} else if err != nil {
		return "", err
	} else if canonicalProjectPath(existingPath) == path {
		return hostname, nil
	}
	candidate := hostname + "-" + shortHash(path)
	for i := 0; ; i++ {
		available, err := s.hostnameAvailable(ctx, candidate)
		if err != nil {
			return "", err
		}
		if available {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%s-%d", hostname, shortHash(path), i+2)
	}
}

func (s *Store) hostnameAvailable(ctx context.Context, hostname string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT
	(SELECT COUNT(*) FROM projects WHERE hostname=?) +
	(SELECT COUNT(*) FROM kubernetes_routes WHERE hostname=?)`,
		hostname,
		hostname,
	).Scan(&count)
	return count == 0, err
}

func (s *Store) AddLog(ctx context.Context, id int64, stream, line string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO logs(project_id, stream, line, created_at) VALUES(?,?,?,?)`, id, stream, line, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) Logs(ctx context.Context, id int64, limit int) ([]app.LogLine, error) {
	return s.logs(ctx, id, "", limit)
}

func (s *Store) LogsByStream(ctx context.Context, id int64, stream string, limit int) ([]app.LogLine, error) {
	return s.logs(ctx, id, stream, limit)
}

func (s *Store) logs(ctx context.Context, id int64, stream string, limit int) ([]app.LogLine, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query := `SELECT project_id,stream,line,created_at FROM logs WHERE project_id=?`
	args := []any{id}
	if stream != "" {
		query += ` AND stream=?`
		args = append(args, stream)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rev := make([]app.LogLine, 0)
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

func (s *Store) ClearLogs(ctx context.Context, id int64, stream string) (int64, error) {
	query := `DELETE FROM logs WHERE project_id=?`
	args := []any{id}
	if stream != "" {
		query += ` AND stream=?`
		args = append(args, stream)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

type scanner interface{ Scan(dest ...any) error }

func scanProject(row scanner) (app.Project, error) {
	var p app.Project
	var auto, managed int
	var last, updated string
	err := row.Scan(&p.ID, &p.Name, &p.Path, &p.Strategy, &p.Command, &p.Port, &p.PinnedPort, &p.Hostname, &p.BaseHostname, &p.SourcePath, &managed, &p.Branch, &p.PID, &p.Status, &auto, &last, &updated)
	if err != nil {
		return p, err
	}
	p.AutoStart = auto == 1
	p.ManagedInstance = managed == 1
	p.LastStarted, _ = time.Parse(time.RFC3339Nano, last)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return p, nil
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:3])
}

func safeHost(name string) string {
	name = strings.ToLower(name)
	labels := make([]string, 0, strings.Count(name, ".")+1)
	for _, rawLabel := range strings.Split(name, ".") {
		var label strings.Builder
		lastDash := false
		for _, r := range rawLabel {
			valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if valid {
				if label.Len() < 63 {
					label.WriteRune(r)
				}
				lastDash = false
				continue
			}
			if !lastDash && label.Len() > 0 && label.Len() < 63 {
				label.WriteByte('-')
				lastDash = true
			}
		}
		cleaned := strings.Trim(label.String(), "-")
		if cleaned != "" {
			labels = append(labels, cleaned)
		}
	}
	out := strings.Join(labels, ".")
	if out == "" {
		return "project"
	}
	return out
}

func legacySafeHost(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func (s *Store) migrateGeneratedHostnames() error {
	rows, err := s.db.Query(`SELECT id,name,hostname FROM projects`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type update struct {
		id       int64
		previous string
		hostname string
	}
	var updates []update
	for rows.Next() {
		var id int64
		var name, hostname string
		if err := rows.Scan(&id, &name, &hostname); err != nil {
			return err
		}
		generated := safeHost(name)
		if generated != hostname && hostname == legacySafeHost(name) {
			updates = append(updates, update{id: id, previous: hostname, hostname: generated})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range updates {
		if _, err := s.db.Exec(`UPDATE projects
SET hostname=?, base_hostname=CASE WHEN base_hostname='' OR base_hostname=? THEN ? ELSE base_hostname END
WHERE id=?`, item.hostname, item.previous, item.hostname, item.id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateProjectPaths() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id,name,path,strategy,command,port,pinned_port,hostname,pid,status,auto_start,last_started,updated_at
FROM projects ORDER BY id`)
	if err != nil {
		return err
	}
	type storedProject struct {
		id          int64
		name        string
		path        string
		strategy    string
		command     string
		port        int
		pinnedPort  int
		hostname    string
		pid         int
		status      string
		autoStart   int
		lastStarted string
		updatedAt   string
	}
	groups := map[string][]storedProject{}
	for rows.Next() {
		var project storedProject
		if err := rows.Scan(
			&project.id,
			&project.name,
			&project.path,
			&project.strategy,
			&project.command,
			&project.port,
			&project.pinnedPort,
			&project.hostname,
			&project.pid,
			&project.status,
			&project.autoStart,
			&project.lastStarted,
			&project.updatedAt,
		); err != nil {
			_ = rows.Close()
			return err
		}
		canonical := canonicalProjectPath(project.path)
		groups[canonical] = append(groups[canonical], project)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for canonical, projects := range groups {
		keeperIndex := 0
		for i := 1; i < len(projects); i++ {
			if projects[i].updatedAt > projects[keeperIndex].updatedAt {
				keeperIndex = i
			}
		}
		keeper := projects[keeperIndex]
		for i, duplicate := range projects {
			if i == keeperIndex {
				continue
			}
			if keeper.hostname == "" || keeper.hostname == safeHost(keeper.name) {
				if duplicate.hostname != "" && duplicate.hostname != safeHost(duplicate.name) {
					keeper.hostname = duplicate.hostname
				}
			}
			if keeper.pinnedPort == 0 && duplicate.pinnedPort != 0 {
				keeper.pinnedPort = duplicate.pinnedPort
			}
			if duplicate.autoStart == 1 {
				keeper.autoStart = 1
			}
			if keeper.status != "running" && duplicate.status == "running" {
				keeper.port = duplicate.port
				keeper.pid = duplicate.pid
				keeper.status = duplicate.status
			}
			if duplicate.lastStarted > keeper.lastStarted {
				keeper.lastStarted = duplicate.lastStarted
			}
			if _, err := tx.Exec(`UPDATE logs SET project_id=? WHERE project_id=?`, keeper.id, duplicate.id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM projects WHERE id=?`, duplicate.id); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`UPDATE projects SET
name=?,path=?,strategy=?,command=?,port=?,pinned_port=?,hostname=?,pid=?,status=?,auto_start=?,last_started=?,updated_at=?
WHERE id=?`,
			keeper.name,
			canonical,
			keeper.strategy,
			keeper.command,
			keeper.port,
			keeper.pinnedPort,
			keeper.hostname,
			keeper.pid,
			keeper.status,
			keeper.autoStart,
			keeper.lastStarted,
			keeper.updatedAt,
			keeper.id,
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func canonicalProjectPath(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
