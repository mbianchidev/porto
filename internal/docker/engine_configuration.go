package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/mbianchidev/porto/internal/config"
	controlapi "github.com/moby/buildkit/api/services/control"
)

type EngineConfiguration struct {
	Directory string `json:"dir"`
	Status    string `json:"status"`
	VMType    string `json:"vmType"`
	Arch      string `json:"arch"`
	CPUs      int    `json:"cpus"`
	Memory    int64  `json:"memory"`
	Disk      int64  `json:"disk"`
}

func (m *Manager) CheckEngineIdle(ctx context.Context) (err error) {
	if m.nativeGuard != nil {
		for _, kind := range []string{"container", "volume"} {
			if err := m.nativeGuard(kind, "*"); err != nil {
				return fmt.Errorf("active native filesystem access prevents engine suspension: %w", err)
			}
		}
	}
	runtimeClient, err := m.runtimeConnector(ctx)
	if err != nil {
		return fmt.Errorf("fresh all-namespace engine inventory is unavailable: %w", err)
	}
	defer func() { err = errors.Join(err, runtimeClient.Close()) }()
	backend, ok := runtimeClient.(*grpcContainerRuntime)
	if !ok || backend.client == nil {
		return fmt.Errorf("%w: a direct all-namespace inventory is required before suspending or reconfiguring the engine", ErrUnsupported)
	}
	namespaces, err := backend.client.NamespaceService().List(ctx)
	if err != nil {
		return fmt.Errorf("inspect every containerd namespace: %w", err)
	}
	for _, namespace := range namespaces {
		response, err := backend.client.TaskService().List(withContainerdNamespace(ctx, namespace), &tasksapi.ListTasksRequest{})
		if err != nil {
			return fmt.Errorf("inspect active tasks in namespace %s: %w", namespace, err)
		}
		for _, task := range response.Tasks {
			if task.Status != tasktypes.Status_STOPPED && task.Status != tasktypes.Status_CREATED {
				return fmt.Errorf("%w: task %s in namespace %s is %s; running, manually paused, Kubernetes, and unmanaged tasks prevent engine suspension", ErrConflict, task.ID, namespace, task.Status)
			}
		}
	}
	connection, err := newBuildKitControlConnection(func(ctx context.Context) (net.Conn, error) {
		return m.DialBuildKit(ctx)
	})
	if err != nil {
		return fmt.Errorf("inspect active builds before engine lifecycle changes: %w", err)
	}
	defer func() { err = errors.Join(err, connection.Close()) }()
	return ensureBuildKitIdle(ctx, controlapi.NewControlClient(connection))
}

func (m *Manager) engineConfiguration(ctx context.Context, instance string) (EngineConfiguration, error) {
	output, err := m.runCommand(ctx, 20*time.Second, "inspect owned engine configuration", nil,
		"limactl", "list", instance, "--json")
	if err != nil {
		return EngineConfiguration{}, err
	}
	var configuration EngineConfiguration
	if err := json.Unmarshal(output, &configuration); err != nil {
		return configuration, fmt.Errorf("decode owned engine configuration: %w", err)
	}
	if configuration.Directory == "" || filepath.Base(configuration.Directory) != instance {
		return configuration, errors.New("Lima did not identify the owned engine's configuration directory")
	}
	return configuration, nil
}

func (m *Manager) editEngineConfiguration(
	ctx context.Context,
	restartConfirmed bool,
	plan func(EngineConfiguration, []byte) (string, error),
	beforeStop func(context.Context) error,
	afterStart func(context.Context) error,
) (err error) {
	if !restartConfirmed {
		return errors.New("restartEngine=true is required; engine configuration changes require a bounded restart")
	}
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return err
	}
	defer release()
	m.installMu.Lock()
	defer m.installMu.Unlock()
	lease, err := acquireEngineInstallLock(filepath.Join(m.stateDir, engineLockFile))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Close()) }()
	state, err := m.readEngineState()
	if err != nil {
		return err
	}
	if state.Mode != "lima" || state.Instance != engineInstanceName || state.OwnerID == "" {
		return fmt.Errorf("%w: resource and translation changes require Porto's owned Lima engine; external services are administrator-managed", ErrUnsupported)
	}
	configuration, err := m.engineConfiguration(ctx, state.Instance)
	if err != nil {
		return err
	}
	path := filepath.Join(configuration.Directory, "lima.yaml")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("owned engine configuration must be a regular file")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	expression, err := plan(configuration, original)
	if err != nil {
		return err
	}
	wasRunning := strings.EqualFold(configuration.Status, "running")
	changed, stopped := false, false
	defer func() {
		recoveryContext, cancel := context.WithTimeout(context.Background(), engineStartTimeout)
		defer cancel()
		if err != nil && changed {
			if !stopped {
				_, stopErr := m.runCommand(recoveryContext, 5*time.Minute, "stop failed configured engine before rollback", nil,
					"limactl", "stop", state.Instance)
				if stopErr != nil {
					err = errors.Join(err, stopErr)
					return
				}
				stopped = true
			}
			if restoreErr := config.WriteFileAtomic(path, original, info.Mode().Perm()); restoreErr != nil {
				err = errors.Join(err, restoreErr)
				return
			}
		}
		if err != nil && stopped && wasRunning {
			_, recoveryErr := m.startLimaRuntime(recoveryContext, "restore owned engine after configuration failure", state.Instance)
			err = errors.Join(err, recoveryErr)
		}
		if !wasRunning && !stopped {
			_, stopErr := m.runCommand(recoveryContext, 5*time.Minute, "restore stopped engine state", nil,
				"limactl", "stop", state.Instance)
			err = errors.Join(err, stopErr)
		}
		m.invalidateContainerInventory()
	}()
	if !wasRunning {
		if _, err := m.startLimaRuntime(ctx, "start owned engine for configuration preflight", state.Instance); err != nil {
			return err
		}
	}
	if err := m.verifyLimaOwnership(ctx, state.OwnerID); err != nil {
		return err
	}
	if err := m.CheckEngineIdle(ctx); err != nil {
		return err
	}
	if beforeStop != nil {
		if err := beforeStop(ctx); err != nil {
			return err
		}
	}
	if _, err := m.runCommand(ctx, 5*time.Minute, "stop idle engine before configuration changes", nil, "limactl", "stop", state.Instance); err != nil {
		return err
	}
	stopped = true
	changed = true
	if _, err := m.runCommand(ctx, 30*time.Second, "apply owned engine configuration", nil,
		"limactl", "edit", "--tty=false", "--set="+expression, state.Instance); err != nil {
		return err
	}
	if _, err := m.startLimaRuntime(ctx, "start configured owned engine", state.Instance); err != nil {
		return err
	}
	stopped = false
	if err := m.verifyLimaOwnership(ctx, state.OwnerID); err != nil {
		return err
	}
	if afterStart != nil {
		return afterStart(ctx)
	}
	return nil
}
