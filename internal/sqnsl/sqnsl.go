package sqnsl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/app"
)

const installTarget = "github.com/mbianchidev/sql-not-so-lite/cmd/sqnsl@ba6cc552d81b6c04f2c709b5a0902c77aa4fe06c"

var errSQLiteFound = errors.New("SQLite database found")

var sqliteExtensions = map[string]bool{
	".db":       true,
	".sqlite":   true,
	".sqlite3":  true,
	".sqlitedb": true,
}

var ignoredDirectories = map[string]bool{
	".git":         true,
	"dist":         true,
	"node_modules": true,
	"target":       true,
	"vendor":       true,
}

type Runner interface {
	LookPath(name string) (string, error)
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type Status struct {
	State     string    `json:"state"`
	Message   string    `json:"message"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Result struct {
	ProjectPaths []string
	Output       string
}

type Manager struct {
	runner Runner
	mu     sync.Mutex
	status Status
}

func NewManager(runner Runner) *Manager {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Manager{
		runner: runner,
		status: Status{State: "idle", Message: "Integration has not run yet.", UpdatedAt: time.Now().UTC()},
	}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Manager) Start(projects []app.Project, done func(Result, error)) bool {
	m.mu.Lock()
	if m.status.State == "running" {
		m.mu.Unlock()
		return false
	}
	m.status = Status{State: "running", Message: "Looking for SQLite databases.", UpdatedAt: time.Now().UTC()}
	m.mu.Unlock()

	go func() {
		result, err := m.Sync(context.Background(), projects)
		status := Status{State: "ready", Message: "sql-not-so-lite scan completed.", UpdatedAt: time.Now().UTC()}
		if err != nil {
			status.State = "error"
			status.Message = err.Error()
		} else if len(result.ProjectPaths) == 0 {
			status.State = "idle"
			status.Message = "No SQLite databases found in orchestrated projects."
		}
		m.mu.Lock()
		m.status = status
		m.mu.Unlock()
		if done != nil {
			done(result, err)
		}
	}()
	return true
}

func (m *Manager) Sync(ctx context.Context, projects []app.Project) (Result, error) {
	paths, err := ProjectPathsWithSQLite(projects)
	if err != nil {
		return Result{}, err
	}
	if len(paths) == 0 {
		return Result{}, nil
	}

	binary, err := m.runner.LookPath("sqnsl")
	if err != nil {
		binary, err = m.install(ctx)
		if err != nil {
			return Result{ProjectPaths: paths}, err
		}
	}

	args := append([]string{"scan"}, paths...)
	output, err := m.runner.Run(ctx, binary, args...)
	result := Result{ProjectPaths: paths, Output: strings.TrimSpace(string(output))}
	if err != nil {
		return result, commandError("sqnsl scan", output, err)
	}
	return result, nil
}

func (m *Manager) install(ctx context.Context) (string, error) {
	goBinary, err := m.runner.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("sqnsl is not installed and Go is unavailable; install sql-not-so-lite manually from https://github.com/mbianchidev/sql-not-so-lite")
	}
	output, err := m.runner.Run(ctx, goBinary, "install", installTarget)
	if err != nil {
		return "", commandError("install sqnsl", output, err)
	}
	return m.installedBinary(ctx, goBinary)
}

func (m *Manager) installedBinary(ctx context.Context, goBinary string) (string, error) {
	output, err := m.runner.Run(ctx, goBinary, "env", "GOBIN")
	if err != nil {
		return "", commandError("resolve GOBIN", output, err)
	}
	binDir := strings.TrimSpace(string(output))
	if binDir == "" {
		output, err = m.runner.Run(ctx, goBinary, "env", "GOPATH")
		if err != nil {
			return "", commandError("resolve GOPATH", output, err)
		}
		goPaths := filepath.SplitList(strings.TrimSpace(string(output)))
		if len(goPaths) == 0 || goPaths[0] == "" {
			return "", fmt.Errorf("go install succeeded but GOPATH is empty")
		}
		binDir = filepath.Join(goPaths[0], "bin")
	}
	name := "sqnsl"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(binDir, name), nil
}

func ProjectPathsWithSQLite(projects []app.Project) ([]string, error) {
	paths := make([]string, 0, len(projects))
	for _, project := range projects {
		found, err := HasSQLiteDatabase(project.Path)
		if err != nil {
			return nil, fmt.Errorf("scan %s for SQLite databases: %w", project.Name, err)
		}
		if found {
			paths = append(paths, project.Path)
		}
	}
	return paths, nil
}

func HasSQLiteDatabase(root string) (bool, error) {
	found := false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && ignoredDirectories[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !sqliteExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
			return nil
		}
		valid, err := hasSQLiteHeader(path)
		if err != nil {
			return err
		}
		if valid {
			found = true
			return errSQLiteFound
		}
		return nil
	})
	if err != nil && !errors.Is(err, errSQLiteFound) {
		return false, err
	}
	return found, nil
}

func hasSQLiteHeader(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	header := make([]byte, 16)
	if _, err := io.ReadFull(file, header); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return false, nil
		}
		return false, err
	}
	return bytes.Equal(header, []byte("SQLite format 3\x00")), nil
}

func commandError(action string, output []byte, err error) error {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("%s failed: %w", action, err)
	}
	return fmt.Errorf("%s failed: %w: %s", action, err, message)
}
