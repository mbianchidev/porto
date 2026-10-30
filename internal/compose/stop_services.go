package compose

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/operations"
)

func (i *Integration) StopServices(ctx context.Context, project app.Project) error {
	file, err := i.configFile(project)
	if err != nil {
		return err
	}
	args := composeArguments(project, file)
	override, err := OverridePath(project)
	if err != nil {
		return err
	}
	if _, err := os.Stat(override); err == nil {
		args = append(args, "-f", override)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	args = append(args, "stop", "--timeout", "30")
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := i.runner.Run(ctx, Command{Dir: project.Path, Name: "docker", Args: args, Env: i.dockerEnvironment("COMPOSE_PROJECT_NAME=" + ProjectName(project))}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return operations.Failure("Unable to stop the selected Compose services gracefully. No volumes were removed.")
	}
	return nil
}
