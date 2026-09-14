package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

// ClusterCreationTimeout includes cold runtime creation and sequential add-ons.
const ClusterCreationTimeout = time.Hour

const (
	kindCreationTimeout        = 20 * time.Minute
	addonCommandTimeout        = 6 * time.Minute
	clusterAPIReadinessTimeout = 6 * time.Minute
)

func provisioningContext(ctx context.Context, action string, timeout time.Duration) (context.Context, context.CancelFunc) {
	cause := fmt.Errorf("%s timed out after %s: %w", action, timeout, context.DeadlineExceeded)
	return context.WithTimeoutCause(ctx, timeout, cause)
}

func provisioningCommandError(ctx context.Context, action string, output []byte, err error) error {
	return runtimes.CommandError(action, output, errors.Join(err, context.Cause(ctx)))
}

func (p *ClusterProvisioner) addonCommandRunner(ctx context.Context, kubeconfigPath, contextName string) func(string, []byte, ...string) ([]byte, error) {
	baseArgs := []string{"--kubeconfig", kubeconfigPath, "--context", contextName}
	return func(action string, stdin []byte, args ...string) ([]byte, error) {
		commandContext, cancel := provisioningContext(ctx, action, addonCommandTimeout)
		defer cancel()
		if cause := context.Cause(commandContext); cause != nil {
			return nil, fmt.Errorf("%s: %w", action, cause)
		}
		commandArgs := append(append([]string(nil), baseArgs...), args...)
		output, err := p.runner.Run(commandContext, runtimes.Command{Name: "kubectl", Args: commandArgs, Stdin: stdin})
		if err != nil || commandContext.Err() != nil {
			return output, provisioningCommandError(commandContext, action, output, err)
		}
		return output, nil
	}
}

func (p *ClusterProvisioner) waitForClusterAPI(ctx context.Context, kubeconfigPath, contextName string) error {
	readinessContext, cancel := provisioningContext(ctx, "Kubernetes API readiness", clusterAPIReadinessTimeout)
	defer cancel()
	run := p.addonCommandRunner(readinessContext, kubeconfigPath, contextName)
	for {
		_, err := run("wait for Kubernetes API", nil, "version", "--request-timeout=10s", "-o", "json")
		if err == nil {
			return nil
		}
		select {
		case <-readinessContext.Done():
			return errors.Join(err, context.Cause(readinessContext))
		case <-time.After(time.Second):
		}
	}
}
