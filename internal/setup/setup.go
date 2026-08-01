package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/compose"
	"github.com/mbianchidev/porto/internal/process"
)

type Command struct {
	Name string
	Args []string
}

type Result struct {
	Commands []string `json:"commands"`
}

type Runner interface {
	Run(ctx context.Context, project app.Project, emit func(stream, line string) error) (Result, error)
}

type ExecRunner struct{}

var ErrUnsupported = errors.New("no supported dependency setup")

func (ExecRunner) Run(ctx context.Context, project app.Project, emit func(stream, line string) error) (Result, error) {
	commands, err := Plan(project)
	if err != nil {
		return Result{}, err
	}
	result := Result{Commands: make([]string, 0, len(commands))}
	for _, command := range commands {
		display := command.String()
		result.Commands = append(result.Commands, display)
		if err := emit("system", "$ "+display); err != nil {
			return result, err
		}
		if err := runCommand(ctx, project.Path, command, emit); err != nil {
			return result, fmt.Errorf("%s failed: %w", display, err)
		}
	}
	return result, nil
}

func Plan(project app.Project) ([]Command, error) {
	switch project.Strategy {
	case "compose":
		file, ok := compose.FindFile(project.Path)
		if !ok {
			return nil, fmt.Errorf("no Docker Compose file found for %s", project.Name)
		}
		return []Command{{Name: "docker", Args: []string{"compose", "-f", file, "build", "--no-cache"}}}, nil
	case "make":
		if target := makeSetupTarget(project.Path); target != "" {
			return []Command{{Name: "make", Args: []string{target}}}, nil
		}
	}

	if commands := nodeCommands(project.Path, usesStartScript(project.Command)); len(commands) > 0 {
		return commands, nil
	}
	if commands := pythonCommands(project.Path); len(commands) > 0 {
		return commands, nil
	}
	if has(project.Path, "go.mod") {
		return []Command{{Name: "go", Args: []string{"mod", "download"}}}, nil
	}
	if has(project.Path, "Cargo.toml") {
		return []Command{{Name: "cargo", Args: []string{"fetch"}}}, nil
	}
	return nil, fmt.Errorf("%w found for %s", ErrUnsupported, project.Name)
}

func NodeRunCommand(dir, script string) Command {
	switch {
	case has(dir, "pnpm-lock.yaml"):
		return Command{Name: "pnpm", Args: []string{"run", script}}
	case has(dir, "yarn.lock"):
		return Command{Name: "yarn", Args: []string{script}}
	case has(dir, "bun.lock"), has(dir, "bun.lockb"):
		return Command{Name: "bun", Args: []string{"run", script}}
	default:
		return Command{Name: "npm", Args: []string{"run", script}}
	}
}

func RuntimeCommand(project app.Project, port int) string {
	if project.Strategy != "package" || port <= 0 {
		return project.Command
	}
	manager, script, ok := nodeScriptCommand(project.Command)
	if !ok {
		return project.Command
	}
	scriptCommand := packageScript(project.Path, script)
	fields := strings.Fields(scriptCommand)
	if len(fields) == 0 || fields[0] != "vite" || hasArgument(fields, "--port") {
		return project.Command
	}
	args := make([]string, 0, 5)
	if !hasArgument(fields, "--host") {
		args = append(args, "--host", "127.0.0.1")
	}
	args = append(args, "--port", strconv.Itoa(port))
	separator := " -- "
	if manager == "yarn" {
		separator = " "
	}
	return project.Command + separator + strings.Join(args, " ")
}

func PythonRunCommand(dir, entry string) Command {
	args := []string{entry}
	if entry == "manage.py" {
		args = append(args, "runserver", "0.0.0.0:"+portVariable())
	}
	return Command{Name: venvPython(dir), Args: args}
}

func (c Command) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

func (c Command) ShellString() string {
	parts := append([]string{c.Name}, c.Args...)
	for i, part := range parts {
		if strings.ContainsAny(part, " \t\"") {
			parts[i] = `"` + strings.ReplaceAll(part, `"`, `\"`) + `"`
		}
	}
	return strings.Join(parts, " ")
}

func nodeCommands(dir string, includeBuild bool) []Command {
	if !has(dir, "package.json") {
		return nil
	}
	var commands []Command
	switch {
	case has(dir, "pnpm-lock.yaml"):
		commands = []Command{{Name: "pnpm", Args: []string{"install", "--frozen-lockfile"}}}
	case has(dir, "yarn.lock"):
		commands = []Command{{Name: "yarn", Args: []string{"install", "--frozen-lockfile"}}}
	case has(dir, "bun.lock"), has(dir, "bun.lockb"):
		commands = []Command{{Name: "bun", Args: []string{"install", "--frozen-lockfile"}}}
	case has(dir, "package-lock.json"), has(dir, "npm-shrinkwrap.json"):
		commands = []Command{{Name: "npm", Args: []string{"ci"}}}
	default:
		commands = []Command{{Name: "npm", Args: []string{"install"}}}
	}
	if includeBuild && packageHasScript(dir, "build") {
		commands = append(commands, NodeRunCommand(dir, "build"))
	}
	return commands
}

func pythonCommands(dir string) []Command {
	switch {
	case has(dir, "uv.lock"):
		return []Command{{Name: "uv", Args: []string{"sync", "--frozen"}}}
	case has(dir, "poetry.lock"):
		return []Command{{Name: "poetry", Args: []string{"install", "--no-interaction"}}}
	case has(dir, "Pipfile.lock"):
		return []Command{{Name: "pipenv", Args: []string{"sync"}}}
	case has(dir, "requirements.txt"):
		return []Command{
			{Name: "python3", Args: []string{"-m", "venv", ".venv"}},
			{Name: venvPython(dir), Args: []string{"-m", "pip", "install", "-r", "requirements.txt"}},
		}
	case has(dir, "pyproject.toml"):
		return []Command{
			{Name: "python3", Args: []string{"-m", "venv", ".venv"}},
			{Name: venvPython(dir), Args: []string{"-m", "pip", "install", "-e", "."}},
		}
	default:
		return nil
	}
}

func venvPython(dir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, ".venv", "Scripts", "python.exe")
	}
	return filepath.Join(dir, ".venv", "bin", "python")
}

func portVariable() string {
	if runtime.GOOS == "windows" {
		return "%PORT%"
	}
	return "$PORT"
}

func makeSetupTarget(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "Makefile"))
	if err != nil {
		data, _ = os.ReadFile(filepath.Join(dir, "makefile"))
	}
	text := string(data)
	for _, target := range []string{"install", "setup", "bootstrap", "deps", "dependencies"} {
		if strings.Contains(text, "\n"+target+":") || strings.HasPrefix(text, target+":") {
			return target
		}
	}
	return ""
}

func packageHasScript(dir, script string) bool {
	return packageScript(dir, script) != ""
}

func packageScript(dir, script string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Scripts[script])
}

func nodeScriptCommand(command string) (string, string, bool) {
	fields := strings.Fields(command)
	switch {
	case len(fields) == 3 && fields[0] == "npm" && fields[1] == "run":
		return fields[0], fields[2], true
	case len(fields) == 3 && fields[0] == "pnpm" && fields[1] == "run":
		return fields[0], fields[2], true
	case len(fields) == 3 && fields[0] == "bun" && fields[1] == "run":
		return fields[0], fields[2], true
	case len(fields) == 2 && fields[0] == "yarn":
		return fields[0], fields[1], true
	case len(fields) == 3 && fields[0] == "yarn" && fields[1] == "run":
		return fields[0], fields[2], true
	default:
		return "", "", false
	}
}

func hasArgument(fields []string, name string) bool {
	for _, field := range fields {
		if field == name || strings.HasPrefix(field, name+"=") {
			return true
		}
	}
	return false
}

func usesStartScript(command string) bool {
	fields := strings.Fields(command)
	return len(fields) > 0 && fields[len(fields)-1] == "start"
}

func runCommand(ctx context.Context, dir string, command Command, emit func(stream, line string) error) error {
	if _, err := exec.LookPath(command.Name); err != nil && !filepath.IsAbs(command.Name) {
		return fmt.Errorf("%s is not available in the Porto daemon PATH: %w", command.Name, err)
	}
	cmd, stdout, stderr, err := process.Command(ctx, dir, command.Name, command.Args...)
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return err
	}

	var wait sync.WaitGroup
	var outputErr error
	var outputMu sync.Mutex
	wait.Add(2)
	go captureReader("stdout", stdout, emit, &wait, &outputMu, &outputErr)
	go captureReader("stderr", stderr, emit, &wait, &outputMu, &outputErr)
	wait.Wait()
	commandErr := cmd.Wait()
	return errors.Join(commandErr, outputErr)
}

func captureReader(
	stream string,
	reader interface {
		Read([]byte) (int, error)
		Close() error
	},
	emit func(stream, line string) error,
	wait *sync.WaitGroup,
	outputMu *sync.Mutex,
	outputErr *error,
) {
	defer wait.Done()
	defer reader.Close()
	if err := process.Stream(reader, func(line string) error { return emit(stream, line) }); err != nil {
		outputMu.Lock()
		*outputErr = errors.Join(*outputErr, err)
		outputMu.Unlock()
	}
}

func has(dir, name string) bool {
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !info.IsDir()
}
