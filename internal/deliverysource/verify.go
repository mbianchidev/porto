package deliverysource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/localtools"
	"github.com/mbianchidev/porto/internal/operations"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func Verify(ctx context.Context, directory, commit string, runner localtools.Runner) error {
	if !commitID.MatchString(commit) {
		return errors.New("recorded Git commit identity is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	work, err := os.MkdirTemp("", "porto-git-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	hooks := filepath.Join(work, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		return err
	}
	globalConfig := filepath.Join(work, "gitconfig")
	if err := config.WriteFileAtomic(globalConfig, nil, 0o600); err != nil {
		return err
	}
	environment := localtools.IsolatedEnvironment(work, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+globalConfig, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=0")
	run := func(arguments ...string) ([]byte, error) {
		output, err := runner.Run(ctx, runtimes.Command{Name: "git", Dir: directory, Args: append([]string{"-c", "core.fsmonitor=false", "-c", "core.longpaths=true", "-c", "core.hooksPath=" + hooks}, arguments...), Env: environment})
		if err != nil {
			return nil, operations.Failure("Recorded Git source is unavailable or changed; no deployment was applied.")
		}
		return output.Stdout, nil
	}
	actual, err := run("rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(actual)) != commit {
		return operations.Failure("Recorded Git checkout no longer matches the previewed immutable commit. Preview the intended source again.")
	}
	status, err := run("status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(status)) != "" {
		return operations.Failure("Recorded Git checkout has local edits. They were not overwritten; resolve the drift or choose an explicitly detached source.")
	}
	return nil
}
