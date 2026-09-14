package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

type delayedProvisioningRunner struct {
	*fakeRunner
	delays map[string]time.Duration
}

func (r *delayedProvisioningRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	key := command.Name + " " + strings.Join(command.Args, " ")
	for fragment, duration := range r.delays {
		if !strings.Contains(key, fragment) {
			continue
		}
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return []byte("synthetic setup progress\n"), errors.New("signal: killed")
		}
	}
	return r.fakeRunner.Run(ctx, command)
}

func TestClusterAddonsGiveStorageAndGatewayIndependentBudgets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &delayedProvisioningRunner{
			fakeRunner: newFakeRunner(),
			delays: map[string]time.Duration{
				"deployment/local-path-provisioner": 4 * time.Minute,
				envoyGatewayManifestURL:             4 * time.Minute,
			},
		}
		provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
		started := time.Now()
		if err := provisioner.ensureClusterAddons(context.Background(), "/mock/kubeconfig", "porto-test"); err != nil {
			t.Fatalf("healthy sequential addon installation was interrupted: %v", err)
		}
		if elapsed := time.Since(started); elapsed < 8*time.Minute {
			t.Fatalf("addon stages did not complete: %s", elapsed)
		}
	})
}

func TestKindMetricsWaitsHaveIndependentBudgets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &delayedProvisioningRunner{
			fakeRunner: newFakeRunner(),
			delays: map[string]time.Duration{
				"wait --for=condition=Ready nodes":                          4 * time.Minute,
				"wait --for=condition=Available deployment/metrics-server":  4 * time.Minute,
				"wait --for=condition=Available apiservice/v1beta1.metrics": 4 * time.Minute,
			},
		}
		provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
		started := time.Now()
		if err := provisioner.ensureKindMetricsServer(context.Background(), "/mock/kubeconfig", "porto-test"); err != nil {
			t.Fatalf("healthy sequential metrics installation was interrupted: %v", err)
		}
		if elapsed := time.Since(started); elapsed < 12*time.Minute {
			t.Fatalf("metrics stages did not complete: %s", elapsed)
		}
	})
}

func TestKindColdCreationCanPrepareImagesBeforeReadinessWait(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		runner := &delayedProvisioningRunner{
			fakeRunner: newFakeRunner(),
			delays:     map[string]time.Duration{"kind create cluster": 11 * time.Minute},
		}
		provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
		provisioner.SetRegistryConfigProvider(func(context.Context) ([]byte, error) {
			return []byte(`{"auths":{}}`), nil
		})
		cluster, err := provisioner.Create(context.Background(), ClusterRequest{
			Name: "test-kind", Provider: "kind", Version: "v1.37.0",
			NodeGroups: []NodeGroupSpec{{Name: "workers", Count: 1}},
		})
		if err != nil {
			t.Fatalf("cold kind creation was interrupted: %v", err)
		}
		if cluster.State != "running" || len(cluster.Nodes) != 2 {
			t.Fatalf("cold kind creation did not finish: %+v", cluster)
		}
	})
}

func TestAddonFailurePreservesParentDeadlineAndProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &delayedProvisioningRunner{
			fakeRunner: newFakeRunner(),
			delays:     map[string]time.Duration{envoyGatewayManifestURL: 2 * time.Minute},
		}
		provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
		cause := fmt.Errorf("synthetic cluster deadline: %w", context.DeadlineExceeded)
		ctx, cancel := context.WithTimeoutCause(context.Background(), time.Minute, cause)
		defer cancel()
		err := provisioner.ensureClusterAddons(ctx, "/mock/kubeconfig", "porto-test")
		if !errors.Is(err, cause) || !strings.Contains(err.Error(), "synthetic setup progress") ||
			!strings.Contains(err.Error(), "install Envoy Gateway") {
			t.Fatalf("addon failure discarded its deadline or progress: %v", err)
		}
	})
}

func TestAddonCommandStillHasItsOwnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &delayedProvisioningRunner{
			fakeRunner: newFakeRunner(),
			delays:     map[string]time.Duration{envoyGatewayManifestURL: 7 * time.Minute},
		}
		provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
		err := provisioner.ensureClusterAddons(context.Background(), "/mock/kubeconfig", "porto-test")
		if !errors.Is(err, context.DeadlineExceeded) ||
			!strings.Contains(err.Error(), "install Envoy Gateway timed out after 6m0s") ||
			!strings.Contains(err.Error(), "synthetic setup progress") {
			t.Fatalf("addon command was not bounded with a useful error: %v", err)
		}
	})
}

func TestKindCreationStillHasItsOwnDeadline(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		runner := &delayedProvisioningRunner{
			fakeRunner: newFakeRunner(),
			delays:     map[string]time.Duration{"kind create cluster": 21 * time.Minute},
		}
		provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
		provisioner.SetRegistryConfigProvider(func(context.Context) ([]byte, error) {
			return []byte(`{"auths":{}}`), nil
		})
		_, err := provisioner.Create(context.Background(), ClusterRequest{Name: "test-kind", Provider: "kind"})
		if !errors.Is(err, context.DeadlineExceeded) ||
			!strings.Contains(err.Error(), "create kind cluster timed out after 20m0s") ||
			!strings.Contains(err.Error(), "synthetic setup progress") {
			t.Fatalf("kind creation was not bounded with a useful error: %v", err)
		}
		state, readErr := provisioner.readClusterMetadata("test-kind")
		if readErr != nil || state.Phase != "error" || !strings.Contains(state.Error, "timed out after 20m0s") {
			t.Fatalf("kind deadline was not persisted: state=%+v, error=%v", state, readErr)
		}
	})
}

func TestAddonCancellationAtGatewayReadinessIsNotSuccess(t *testing.T) {
	cause := errors.New("synthetic daemon shutdown")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Contains(strings.Join(command.Args, " "), "wait --for=condition=Programmed gateway/porto") {
			cancel(cause)
			return nil, errors.New("signal: killed")
		}
		return nil, nil
	}
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	if err := provisioner.ensureClusterAddons(ctx, "/mock/kubeconfig", "porto-test"); !errors.Is(err, cause) {
		t.Fatalf("canceled cluster setup was reported as successful: %v", err)
	}
}
