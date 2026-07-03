package discovery

import (
"context"
"encoding/json"
"os"
"path/filepath"
"sort"
"strings"

"github.com/mbianchidev/porto/internal/app"
)

type Options struct { Depth int; Ignore []string }

func Scan(ctx context.Context, roots []string, opts Options) ([]app.Project, error) {
if opts.Depth < 0 { opts.Depth = 0 }
ignore := map[string]bool{"node_modules": true}
for _, item := range opts.Ignore { if item != "" { ignore[item] = true } }
var projects []app.Project
seen := map[string]bool{}
for _, root := range roots {
abs, err := filepath.Abs(root); if err != nil { return nil, err }
baseDepth := strings.Count(abs, string(os.PathSeparator))
err = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
if err != nil { return nil }
select { case <-ctx.Done(): return ctx.Err(); default: }
if !d.IsDir() { return nil }
if ignore[d.Name()] && path != abs { return filepath.SkipDir }
if strings.Count(path, string(os.PathSeparator))-baseDepth > opts.Depth { return filepath.SkipDir }
p, ok := Detect(path)
if ok && !seen[path] { projects = append(projects, p); seen[path] = true; return filepath.SkipDir }
return nil
})
if err != nil { return nil, err }
}
sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
return projects, nil
}

func Detect(path string) (app.Project, bool) {
name := filepath.Base(path)
if has(path, "Makefile") || has(path, "makefile") {
target := makeTarget(path)
cmd := "make"
if target != "" { cmd += " " + target }
return app.Project{Name: name, Path: path, Strategy: "make", Command: cmd}, true
}
if has(path, "docker-compose.yml") || has(path, "docker-compose.yaml") || has(path, "compose.yml") || has(path, "compose.yaml") {
file := first(path, []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"})
return app.Project{Name: name, Path: path, Strategy: "compose", Command: "docker compose -f " + file + " up"}, true
}
if has(path, "package.json") {
if script := packageScript(path); script != "" {
return app.Project{Name: name, Path: path, Strategy: "package", Command: "npm run " + script}, true
}
}
return app.Project{}, false
}

func has(dir, name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
func first(dir string, names []string) string { for _, n := range names { if has(dir,n) { return n } }; return names[0] }

func makeTarget(dir string) string {
b, err := os.ReadFile(filepath.Join(dir, "Makefile")); if err != nil { b, _ = os.ReadFile(filepath.Join(dir, "makefile")) }
text := string(b)
for _, t := range []string{"dev", "run", "start"} { if strings.Contains(text, "\n"+t+":") || strings.HasPrefix(text, t+":") { return t } }
return ""
}

func packageScript(dir string) string {
b, err := os.ReadFile(filepath.Join(dir, "package.json")); if err != nil { return "" }
var pkg struct{ Scripts map[string]string `json:"scripts"`; Name string `json:"name"` }
if json.Unmarshal(b, &pkg) != nil { return "" }
if pkg.Scripts["start"] != "" { return "start" }
if pkg.Scripts["dev"] != "" { return "dev" }
return ""
}
