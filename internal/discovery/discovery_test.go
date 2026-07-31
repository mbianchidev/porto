package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectAdditionalEcosystems(t *testing.T) {
	tests := []struct {
		name         string
		files        map[string]string
		wantStrategy string
		wantCommand  string
	}{
		{
			name:         "python",
			files:        map[string]string{"requirements.txt": "fastapi\n", "main.py": "print('ready')\n"},
			wantStrategy: "python",
			wantCommand:  ".venv",
		},
		{
			name:         "go",
			files:        map[string]string{"go.mod": "module example.com/app\n", "main.go": "package main\nfunc main() {}\n"},
			wantStrategy: "go",
			wantCommand:  "go run .",
		},
		{
			name:         "rust",
			files:        map[string]string{"Cargo.toml": "[package]\nname='app'\n", filepath.Join("src", "main.rs"): "fn main() {}\n"},
			wantStrategy: "rust",
			wantCommand:  "cargo run",
		},
		{
			name:         "pnpm",
			files:        map[string]string{"package.json": `{"scripts":{"dev":"vite"}}`, "pnpm-lock.yaml": ""},
			wantStrategy: "package",
			wantCommand:  "pnpm run dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), tt.name)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, contents := range tt.files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			project, ok := Detect(dir)
			if !ok {
				t.Fatal("project was not detected")
			}
			if project.Strategy != tt.wantStrategy {
				t.Fatalf("strategy = %q, want %q", project.Strategy, tt.wantStrategy)
			}
			if !strings.Contains(project.Command, tt.wantCommand) {
				t.Fatalf("command = %q, want it to contain %q", project.Command, tt.wantCommand)
			}
		})
	}
}
