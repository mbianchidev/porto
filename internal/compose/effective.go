package compose

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/localtools"
	"github.com/mbianchidev/porto/internal/operations"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func (i *Integration) EffectiveDefinition(ctx context.Context, project app.Project) ([]byte, error) {
	file, err := i.configFile(project)
	if err != nil {
		return nil, err
	}
	args := composeArguments(project, file)
	override, err := OverridePath(project)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(override); err == nil {
		args = append(args, "-f", override)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	args = append(args, "config", "--format", "json")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := (localtools.ExecRunner{}).Run(ctx, runtimes.Command{Name: "docker", Dir: project.Path, Args: slices.Clone(args), Env: i.dockerEnvironment("COMPOSE_PROJECT_NAME="+ProjectName(project), "COMPOSE_PROFILES=")})
	if err != nil || !json.Valid(output.Stdout) {
		return nil, operations.Failure("Unable to record effective Compose configuration and its actual port overrides.")
	}
	return output.Stdout, nil
}
