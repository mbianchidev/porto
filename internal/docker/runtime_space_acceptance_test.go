package docker

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type limitedVolumeRunner struct {
	base runtimes.Runner
	root string
}

func (runner limitedVolumeRunner) command(command runtimes.Command) runtimes.Command {
	if command.Name == "nerdctl" {
		command.Args = append([]string{"--data-root", runner.root}, command.Args...)
		return command
	}
	index := slices.Index(command.Args, "nerdctl")
	if index >= 0 {
		arguments := append(slices.Clone(command.Args[:index+1]), "--data-root", runner.root)
		command.Args = append(arguments, command.Args[index+1:]...)
	}
	return command
}

func (runner limitedVolumeRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	return runner.base.Run(ctx, runner.command(command))
}

func (runner limitedVolumeRunner) RunStreaming(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
	streamer, ok := runner.base.(streamingRunner)
	if !ok {
		return nil, errors.New("the real limited-space backend cannot stream filesystem operations")
	}
	return streamer.RunStreaming(ctx, runner.command(command), emit)
}

func (fixture *runtimeDataFixture) lowSpaceMigration(t *testing.T) {
	root := fixture.runtimeLowSpaceRoot(t)
	base := fixture.manager.runner
	fixture.manager.runner = limitedVolumeRunner{base: base, root: root}
	defer func() { fixture.manager.runner = base }()
	volume := fixture.selection[slices.IndexFunc(fixture.selection, func(selection dataops.Selection) bool { return selection.Kind == "volume" })]
	volume.Destination = fixture.prefix + "-low-space-volume"
	result, err := fixture.migrationRun(t, []dataops.Selection{volume}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "insufficient free space") || len(result.Steps) != 1 || result.Steps[0].Status != "failed" {
		t.Fatalf("actual 8 MiB filesystem did not enforce staged restore space requirements: %+v, %v", result, err)
	}
	fixture.requireMissingVolume(t, volume.Destination)
}
