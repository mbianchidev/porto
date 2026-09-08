package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const (
	defaultHealthInterval      = 30 * time.Second
	defaultHealthTimeout       = 30 * time.Second
	defaultHealthStartInterval = 5 * time.Second
	defaultHealthRetries       = 3
	maxHealthLogEntries        = 5
	maxHealthOutputBytes       = 4096
)

type containerHealthLog struct {
	Start    time.Time `json:"Start"`
	End      time.Time `json:"End"`
	ExitCode int       `json:"ExitCode"`
	Output   string    `json:"Output"`
}

type containerHealthState struct {
	Status        string               `json:"Status"`
	FailingStreak int                  `json:"FailingStreak"`
	Log           []containerHealthLog `json:"Log,omitempty"`
}

type healthSchedule struct {
	startedAt time.Time
	nextRun   time.Time
	running   bool
}

type healthSchedulerState struct {
	mu             sync.Mutex
	schedules      map[string]*healthSchedule
	networkNext    map[string]time.Time
	networkRunning map[string]bool
}

func (m *Manager) runHealthScheduler(ctx context.Context) {
	state := &healthSchedulerState{
		schedules:      make(map[string]*healthSchedule),
		networkNext:    make(map[string]time.Time),
		networkRunning: make(map[string]bool),
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.scheduleHealthChecks(ctx, state, now)
		}
	}
}

func (m *Manager) scheduleHealthChecks(
	ctx context.Context,
	state *healthSchedulerState,
	now time.Time,
) {
	snapshot := m.ContainerSnapshot()
	m.scheduleNetworkReconciliation(ctx, state, snapshot, now)
	active := make(map[string]struct{})
	for _, container := range snapshot.Containers {
		if container.Labels[portoManagedLabel] != portoRuntimeVersion ||
			container.State != "running" ||
			container.Healthcheck == nil ||
			!healthcheckEnabled(container.Healthcheck) {
			continue
		}
		active[container.ID] = struct{}{}
		state.mu.Lock()
		schedule := state.schedules[container.ID]
		if schedule == nil {
			starting := container.Healthcheck.StartPeriod > 0
			schedule = &healthSchedule{
				startedAt: now,
				nextRun:   now.Add(healthInterval(container.Healthcheck, starting)),
			}
			state.schedules[container.ID] = schedule
		}
		if schedule.running || now.Before(schedule.nextRun) {
			state.mu.Unlock()
			continue
		}
		schedule.running = true
		healthcheck := cloneHealthcheck(container.Healthcheck)
		currentHealth := container.Health
		startedAt := schedule.startedAt
		state.mu.Unlock()
		go m.executeScheduledHealthCheck(
			ctx,
			state,
			container.ID,
			healthcheck,
			currentHealth,
			startedAt,
		)
	}
	state.mu.Lock()
	for id := range state.schedules {
		if _, ok := active[id]; !ok {
			delete(state.schedules, id)
		}
	}
	state.mu.Unlock()
}

func (m *Manager) scheduleNetworkReconciliation(
	ctx context.Context,
	state *healthSchedulerState,
	snapshot ContainerSnapshot,
	now time.Time,
) {
	active := make(map[string]struct{})
	for _, container := range snapshot.Containers {
		if container.Labels[portoManagedLabel] != portoRuntimeVersion ||
			container.Labels[portoNetworkStateLabel] == "" {
			continue
		}
		active[container.ID] = struct{}{}
		state.mu.Lock()
		next := state.networkNext[container.ID]
		persistedPID := uint32(parsePositiveInt(container.Labels[portoNetworkPIDLabel]))
		pidChanged := persistedPID != container.PID
		if state.networkRunning[container.ID] ||
			(!pidChanged && !next.IsZero() && now.Before(next)) {
			state.mu.Unlock()
			continue
		}
		state.networkNext[container.ID] = now.Add(defaultInventoryReconcileInterval)
		state.networkRunning[container.ID] = true
		state.mu.Unlock()
		go func(id string, force bool) {
			defer func() {
				state.mu.Lock()
				state.networkRunning[id] = false
				state.mu.Unlock()
			}()
			if err := m.reconcileContainerNetworks(ctx, id, force); err != nil && ctx.Err() == nil {
				log.Printf("reconcile networks for container %s: %v", id, err)
			}
		}(container.ID, pidChanged)
	}
	state.mu.Lock()
	for id := range state.networkNext {
		if _, ok := active[id]; !ok {
			delete(state.networkNext, id)
			delete(state.networkRunning, id)
		}
	}
	state.mu.Unlock()
}

type containerNetworkReconciler interface {
	ReconcileNetworks(context.Context, string, bool) error
}

func (m *Manager) reconcileContainerNetworks(ctx context.Context, id string, force bool) error {
	if m.runtimeConnector == nil {
		return fmt.Errorf("%w: container runtime connector is unavailable", ErrUnavailable)
	}
	runtimeClient, err := m.runtimeConnector(ctx)
	if err != nil {
		return err
	}
	defer runtimeClient.Close()
	reconciler, ok := runtimeClient.(containerNetworkReconciler)
	if !ok {
		return fmt.Errorf("%w: direct network reconciliation", ErrUnsupported)
	}
	return reconciler.ReconcileNetworks(ctx, id, force)
}

func (m *Manager) executeScheduledHealthCheck(
	ctx context.Context,
	state *healthSchedulerState,
	id string,
	healthcheck *ContainerHealthcheck,
	current ContainerHealth,
	startedAt time.Time,
) {
	result := m.runHealthCheck(ctx, id, healthcheck)
	now := time.Now()
	withinStartPeriod := healthcheck.StartPeriod > 0 && now.Sub(startedAt) < healthcheck.StartPeriod
	nextDelay := healthInterval(healthcheck, withinStartPeriod)
	state.mu.Lock()
	if schedule := state.schedules[id]; schedule != nil {
		schedule.running = false
		schedule.nextRun = now.Add(nextDelay)
	}
	state.mu.Unlock()
	if result == nil {
		return
	}
	healthState := containerHealthState{
		Status:        current.Status,
		FailingStreak: current.FailingStreak,
		Log:           []containerHealthLog{*result},
	}
	if encoded := m.currentHealthState(id); encoded != "" {
		var persisted containerHealthState
		if err := json.Unmarshal([]byte(encoded), &persisted); err == nil {
			healthState = persisted
			healthState.Log = append(healthState.Log, *result)
		}
	}
	if len(healthState.Log) > maxHealthLogEntries {
		healthState.Log = append([]containerHealthLog(nil), healthState.Log[len(healthState.Log)-maxHealthLogEntries:]...)
	}
	if result.ExitCode == 0 {
		healthState.Status = "healthy"
		healthState.FailingStreak = 0
	} else if !withinStartPeriod {
		healthState.FailingStreak++
		if healthState.FailingStreak >= healthRetries(healthcheck) {
			healthState.Status = "unhealthy"
		} else {
			healthState.Status = "starting"
		}
	}
	encoded, err := json.Marshal(healthState)
	if err != nil {
		log.Printf("encode health state for container %s: %v", id, err)
		return
	}
	handled, updateErr := m.withContainerOperations(ctx, func(operations containerOperations) error {
		return operations.UpdateLabels(ctx, id, map[string]string{
			nerdctlHealthStateLabel: string(encoded),
		})
	})
	if !handled {
		updateErr = fmt.Errorf("%w: direct health metadata update", ErrUnavailable)
	}
	if updateErr != nil && ctx.Err() == nil {
		log.Printf("update health state for container %s: %v", id, updateErr)
		return
	}
	m.invalidateContainerInventory()
}

func (m *Manager) currentHealthState(id string) string {
	snapshot := m.ContainerSnapshot()
	for _, container := range snapshot.Containers {
		if container.ID == id {
			return container.Labels[nerdctlHealthStateLabel]
		}
	}
	return ""
}

func (m *Manager) runHealthCheck(
	ctx context.Context,
	id string,
	healthcheck *ContainerHealthcheck,
) *containerHealthLog {
	command := healthCommand(healthcheck)
	if len(command) == 0 {
		return nil
	}
	timeout := healthcheck.Timeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	probeContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now().UTC()
	process, err := m.StartExec(probeContext, ExecRequest{
		ContainerID:  id,
		Command:      command,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return &containerHealthLog{
			Start:    start,
			End:      time.Now().UTC(),
			ExitCode: -1,
			Output:   truncateHealthOutput(err.Error()),
		}
	}
	_ = process.Stdin().Close()
	output := &healthOutput{}
	var copies sync.WaitGroup
	copies.Add(2)
	go func() {
		defer copies.Done()
		_, _ = io.Copy(output, process.Stdout())
	}()
	go func() {
		defer copies.Done()
		_, _ = io.Copy(output, process.Stderr())
	}()
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- process.Wait()
	}()
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-probeContext.Done():
		killErr := process.Kill()
		waitErr = errors.Join(<-waitDone, killErr)
		_, _ = output.Write([]byte("healthcheck timed out"))
	}
	copies.Wait()
	exitCode := 0
	if waitErr != nil {
		exitCode = -1
		var exited interface{ ExitCode() int }
		if errors.As(waitErr, &exited) {
			exitCode = exited.ExitCode()
		} else if errors.Is(probeContext.Err(), context.DeadlineExceeded) {
			exitCode = -1
		} else {
			_, _ = output.Write([]byte(waitErr.Error()))
		}
	}
	return &containerHealthLog{
		Start:    start,
		End:      time.Now().UTC(),
		ExitCode: exitCode,
		Output:   output.String(),
	}
}

type healthOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (o *healthOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	remaining := maxHealthOutputBytes - o.buffer.Len()
	if remaining > 0 {
		o.buffer.Write(data[:min(len(data), remaining)])
	}
	return len(data), nil
}

func (o *healthOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.TrimSpace(o.buffer.String())
}

func healthcheckEnabled(healthcheck *ContainerHealthcheck) bool {
	return healthcheck != nil &&
		len(healthcheck.Test) > 0 &&
		healthcheck.Test[0] != "" &&
		healthcheck.Test[0] != "NONE"
}

func healthCommand(healthcheck *ContainerHealthcheck) []string {
	if !healthcheckEnabled(healthcheck) {
		return nil
	}
	switch healthcheck.Test[0] {
	case "CMD":
		return append([]string(nil), healthcheck.Test[1:]...)
	case "CMD-SHELL":
		return []string{"sh", "-c", strings.Join(healthcheck.Test[1:], " ")}
	default:
		return nil
	}
}

func healthInterval(healthcheck *ContainerHealthcheck, starting bool) time.Duration {
	if starting && healthcheck.StartInterval > 0 {
		return healthcheck.StartInterval
	}
	if starting && healthcheck.StartInterval == 0 {
		return defaultHealthStartInterval
	}
	if healthcheck.Interval > 0 {
		return healthcheck.Interval
	}
	return defaultHealthInterval
}

func healthRetries(healthcheck *ContainerHealthcheck) int {
	if healthcheck.Retries > 0 {
		return healthcheck.Retries
	}
	return defaultHealthRetries
}

func cloneHealthcheck(healthcheck *ContainerHealthcheck) *ContainerHealthcheck {
	if healthcheck == nil {
		return nil
	}
	cloned := *healthcheck
	cloned.Test = append([]string(nil), healthcheck.Test...)
	return &cloned
}

func truncateHealthOutput(value string) string {
	if len(value) <= maxHealthOutputBytes {
		return value
	}
	return value[:maxHealthOutputBytes]
}

var _ runtimes.Process = (*directContainerProcess)(nil)
