package setup

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/app"
)

func TestPlanSelectsSupportedEcosystems(t *testing.T) {
	tests := []struct {
		name     string
		strategy string
		files    map[string]string
		want     []Command
	}{
		{
			name:     "compose",
			strategy: "compose",
			files:    map[string]string{"compose.yaml": "services: {}"},
			want:     []Command{{Name: "docker", Args: []string{"compose", "-f", "compose.yaml", "build", "--no-cache"}}},
		},
		{
			name:     "make target",
			strategy: "make",
			files:    map[string]string{"Makefile": "setup:\n\t@echo setup\n"},
			want:     []Command{{Name: "make", Args: []string{"setup"}}},
		},
		{
			name:     "pnpm",
			strategy: "package",
			files:    map[string]string{"package.json": "{}", "pnpm-lock.yaml": ""},
			want:     []Command{{Name: "pnpm", Args: []string{"install", "--frozen-lockfile"}}},
		},
		{
			name:     "npm lock",
			strategy: "package",
			files:    map[string]string{"package.json": "{}", "package-lock.json": "{}"},
			want:     []Command{{Name: "npm", Args: []string{"ci"}}},
		},
		{
			name:     "production node build",
			strategy: "package",
			files: map[string]string{
				"package.json":      `{"scripts":{"start":"next start","build":"next build"}}`,
				"package-lock.json": "{}",
			},
			want: []Command{
				{Name: "npm", Args: []string{"ci"}},
				{Name: "npm", Args: []string{"run", "build"}},
			},
		},
		{
			name:     "python requirements",
			strategy: "make",
			files:    map[string]string{"Makefile": "run:\n\tpython3 app.py\n", "requirements.txt": "fastapi\n"},
			want: []Command{
				{Name: "python3", Args: []string{"-m", "venv", ".venv"}},
				{Name: "VENVPYTHON", Args: []string{"-m", "pip", "install", "-r", "requirements.txt"}},
			},
		},
		{
			name:     "go",
			strategy: "make",
			files:    map[string]string{"Makefile": "run:\n\tgo run .\n", "go.mod": "module example.com/app\n"},
			want:     []Command{{Name: "go", Args: []string{"mod", "download"}}},
		},
		{
			name:     "rust",
			strategy: "make",
			files:    map[string]string{"Makefile": "run:\n\tcargo run\n", "Cargo.toml": "[package]\nname='app'\n"},
			want:     []Command{{Name: "cargo", Args: []string{"fetch"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, contents := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			command := ""
			if tt.name == "production node build" {
				command = "npm run start"
			}
			got, err := Plan(app.Project{Name: "app", Path: dir, Strategy: tt.strategy, Command: command})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			for i := range got {
				binary := strings.TrimSuffix(filepath.Base(got[i].Name), ".exe")
				if filepath.IsAbs(got[i].Name) && binary == "python" {
					got[i].Name = "VENVPYTHON"
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("commands = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestNodeRunCommandUsesLockfileManager(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "yarn.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := NodeRunCommand(dir, "dev")
	want := Command{Name: "yarn", Args: []string{"dev"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %#v, want %#v", got, want)
	}
}

func TestRuntimeCommandConfiguresVite(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		script      string
		wantCommand string
	}{
		{
			name:        "npm",
			command:     "npm run dev",
			script:      "vite",
			wantCommand: "npm run dev -- --host 127.0.0.1 --port 41001",
		},
		{
			name:        "pnpm",
			command:     "pnpm run dev",
			script:      "vite --open",
			wantCommand: "pnpm run dev -- --host 127.0.0.1 --port 41001",
		},
		{
			name:        "yarn",
			command:     "yarn dev",
			script:      "vite",
			wantCommand: "yarn dev --host 127.0.0.1 --port 41001",
		},
		{
			name:        "bun",
			command:     "bun run dev",
			script:      "vite --host 0.0.0.0",
			wantCommand: "bun run dev -- --port 41001",
		},
		{
			name:        "existing port",
			command:     "npm run dev",
			script:      "vite --port 3000",
			wantCommand: "npm run dev",
		},
		{
			name:        "composed script",
			command:     "npm run dev",
			script:      "concurrently vite api",
			wantCommand: "npm run dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			packageJSON := `{"scripts":{"dev":` + strconv.Quote(tt.script) + `}}`
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			project := app.Project{Path: dir, Strategy: "package", Command: tt.command}
			if got := RuntimeCommand(project, 41001); got != tt.wantCommand {
				t.Fatalf("command = %q, want %q", got, tt.wantCommand)
			}
		})
	}
}
