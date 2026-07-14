package sendbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/process"
)

const (
	ConfigFile = ".sendbox.yaml"
	installURL = "https://github.com/mbianchidev/sendbox"
)

type Resolver interface {
	LookPath(name string) (string, error)
}

type ExecResolver struct{}

func (ExecResolver) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

type Status struct {
	State     string    `json:"state"`
	Message   string    `json:"message"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Launch struct {
	Binary     string
	Args       []string
	Dir        string
	ConfigPath string
}

type Integration struct {
	resolver Resolver
	goos     string
	goarch   string
	now      func() time.Time
}

func New(resolver Resolver) *Integration {
	return newIntegration(resolver, runtime.GOOS, runtime.GOARCH, time.Now)
}

func newIntegration(resolver Resolver, goos, goarch string, now func() time.Time) *Integration {
	if resolver == nil {
		resolver = ExecResolver{}
	}
	return &Integration{resolver: resolver, goos: goos, goarch: goarch, now: now}
}

func (i *Integration) Status(projects []app.Project) Status {
	configured := 0
	for _, project := range projects {
		path, err := ConfigPath(project.Path)
		if err != nil {
			return i.status("error", fmt.Sprintf("Inspect %s Sendbox config: %v", project.Name, err))
		}
		if path != "" {
			configured++
		}
	}
	if configured == 0 {
		return i.status("idle", "No managed project contains .sendbox.yaml.")
	}
	if err := i.supported(); err != nil {
		return i.status("error", err.Error())
	}
	if _, err := i.resolver.LookPath("sendbox"); err != nil {
		return i.status("error", "Sendbox is not installed; install it from "+installURL+".")
	}
	return i.status("ready", fmt.Sprintf("Sendbox is ready for %d configured project(s).", configured))
}

func (i *Integration) Launch(project app.Project) (Launch, error) {
	configPath, err := ConfigPath(project.Path)
	if err != nil {
		return Launch{}, fmt.Errorf("inspect Sendbox config: %w", err)
	}
	if configPath == "" {
		return Launch{}, fmt.Errorf("%s does not contain %s", project.Name, ConfigFile)
	}
	if err := i.supported(); err != nil {
		return Launch{}, err
	}
	binary, err := i.resolver.LookPath("sendbox")
	if err != nil {
		return Launch{}, fmt.Errorf("sendbox is not installed; install it from %s", installURL)
	}
	return Launch{
		Binary:     binary,
		Args:       []string{"run", "--config", configPath, "--project", project.Path},
		Dir:        project.Path,
		ConfigPath: configPath,
	}, nil
}

func (i *Integration) Command(ctx context.Context, project app.Project) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	launch, err := i.Launch(project)
	if err != nil {
		return nil, nil, nil, err
	}
	return process.Command(ctx, launch.Dir, launch.Binary, launch.Args...)
}

func ConfigPath(root string) (string, error) {
	path := filepath.Join(root, ConfigFile)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	return path, nil
}

func (i *Integration) supported() error {
	if i.goos != "darwin" || i.goarch != "arm64" {
		return fmt.Errorf("sendbox requires macOS 26 on Apple Silicon")
	}
	return nil
}

func (i *Integration) status(state, message string) Status {
	return Status{State: state, Message: message, UpdatedAt: i.now().UTC()}
}
