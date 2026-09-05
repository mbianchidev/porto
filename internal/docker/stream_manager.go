package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type containerAttachDetails struct {
	ID    string
	TTY   bool
	State containerWaitState
}

type containerStartBaseline struct {
	eventSequence     uint64
	inventoryRevision uint64
	startedAt         string
	finishedAt        string
}

func (m *Manager) StartExec(ctx context.Context, request ExecRequest) (runtimes.Process, error) {
	if err := validateObjectID(request.ContainerID); err != nil {
		return nil, fmt.Errorf("container: %w", err)
	}
	if len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
		return nil, errors.New("exec command is required")
	}
	args := []string{"exec"}
	if request.Privileged {
		args = append(args, "--privileged")
	}
	for _, value := range request.Environment {
		if strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("invalid exec environment")
		}
		args = append(args, "--env", value)
	}
	args = appendStringFlag(args, "--workdir", request.WorkingDir)
	args = appendStringFlag(args, "--user", request.User)
	if request.AttachStdin {
		args = append(args, "--interactive")
	}
	if request.TTY {
		args = append(args, "--tty")
	}
	args = append(args, request.ContainerID)
	args = append(args, request.Command...)
	return m.startProcess(ctx, "start Porto container exec", args...)
}

func (m *Manager) StartAttach(ctx context.Context, id string, stdin bool) (runtimes.Process, error) {
	if err := validateObjectID(id); err != nil {
		return nil, err
	}
	args := []string{"attach"}
	if !stdin {
		args = append(args, "--no-stdin")
	}
	args = append(args, id)
	return m.startProcess(ctx, "attach Porto container", args...)
}

func (m *Manager) StartContainerAttached(ctx context.Context, id string, stdin bool) (runtimes.Process, error) {
	if err := validateObjectID(id); err != nil {
		return nil, err
	}
	args := []string{"start", "--attach"}
	if stdin {
		args = append(args, "--interactive")
	}
	args = append(args, id)
	process, err := m.startProcess(ctx, "start attached Porto container", args...)
	if err == nil {
		m.invalidateContainerInventory()
	}
	return process, err
}

func (m *Manager) containerAttachDetails(ctx context.Context, id string) (containerAttachDetails, error) {
	document, err := m.inspect(ctx, "container", id)
	if err != nil {
		return containerAttachDetails{}, err
	}
	var inspected struct {
		ID     string             `json:"Id"`
		Config struct{ TTY bool } `json:"Config"`
		State  containerWaitState `json:"State"`
	}
	if err := json.Unmarshal(document, &inspected); err != nil {
		return containerAttachDetails{}, fmt.Errorf("decode container attach settings: %w", err)
	}
	if inspected.ID == "" {
		return containerAttachDetails{}, errors.New("container runtime returned an empty container identifier")
	}
	return containerAttachDetails{
		ID:    inspected.ID,
		TTY:   inspected.Config.TTY,
		State: inspected.State,
	}, nil
}

func (m *Manager) containerStartBaseline(state containerWaitState) containerStartBaseline {
	baseline := containerStartBaseline{
		startedAt:  state.StartedAt,
		finishedAt: state.FinishedAt,
	}
	if inventory := m.activeContainerInventory(); inventory != nil {
		snapshot := inventory.snapshotValue()
		baseline.inventoryRevision = snapshot.Revision
		for _, event := range snapshot.Events {
			baseline.eventSequence = max(baseline.eventSequence, event.Sequence)
		}
	}
	return baseline
}

func (m *Manager) waitForContainerStart(
	ctx context.Context,
	id string,
	baseline containerStartBaseline,
) error {
	waitContext, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	if inventory := m.activeContainerInventory(); inventory != nil {
		results := make(chan error, 2)
		go func() {
			results <- inventory.waitForStart(waitContext, id, baseline)
		}()
		go func() {
			results <- m.pollForContainerStart(waitContext, id, baseline)
		}()
		var waitErr error
		for range 2 {
			err := <-results
			if err == nil {
				return nil
			}
			waitErr = errors.Join(waitErr, err)
		}
		return waitErr
	}
	return m.pollForContainerStart(waitContext, id, baseline)
}

func (m *Manager) pollForContainerStart(
	ctx context.Context,
	id string,
	baseline containerStartBaseline,
) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := m.containerWaitState(ctx, id)
		if err != nil {
			return err
		}
		if containerStartStateChanged(state, baseline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
		}
	}
}

func (m *Manager) containerStartObserved(
	ctx context.Context,
	id string,
	baseline containerStartBaseline,
) (bool, error) {
	state, err := m.containerWaitState(ctx, id)
	if err != nil {
		if containerRemovalComplete(err) {
			if inventory := m.activeContainerInventory(); inventory != nil {
				return inventory.startObserved(id, baseline), nil
			}
			return false, nil
		}
		return false, err
	}
	return containerStartStateChanged(state, baseline), nil
}

func containerStartStateChanged(state containerWaitState, baseline containerStartBaseline) bool {
	return state.active() ||
		(state.StartedAt != "" && state.StartedAt != baseline.startedAt) ||
		(state.FinishedAt != "" && state.FinishedAt != baseline.finishedAt)
}

func (m *Manager) startProcess(ctx context.Context, action string, args ...string) (runtimes.Process, error) {
	backend, err := m.backend(ctx)
	if err != nil {
		return nil, err
	}
	runner, ok := m.runner.(runtimes.ProcessRunner)
	if !ok {
		return nil, fmt.Errorf("%w: process streaming", ErrUnsupported)
	}
	process, err := runner.Start(ctx, runtimes.Command{
		Name: backend.name,
		Args: append(append([]string(nil), backend.prefix...), args...),
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return process, nil
}
