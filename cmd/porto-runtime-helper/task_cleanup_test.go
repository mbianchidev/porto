package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"
	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

func TestTaskCleanupRequestFencesBackendAndUnitIdentity(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, scenario := range []struct {
		name string
		args []string
		ok   bool
	}{
		{name: "valid", args: []string{"--address", "/synthetic/containerd.sock", "--namespace", "synthetic", "--id", id}, ok: true},
		{name: "relative socket", args: []string{"--address", "relative.sock", "--namespace", "synthetic", "--id", id}},
		{name: "missing namespace", args: []string{"--address", "/synthetic/containerd.sock", "--id", id}},
		{name: "not a systemd unit identity", args: []string{"--address", "/synthetic/containerd.sock", "--namespace", "synthetic", "--id", "unrelated.service"}},
		{name: "extra command", args: []string{"--address", "/synthetic/containerd.sock", "--namespace", "synthetic", "--id", id, "extra"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			request, err := parseTaskCleanupRequest(scenario.args)
			if (err == nil) != scenario.ok {
				t.Fatalf("cleanup request validation: %v", err)
			}
			if scenario.ok && (request.id != id || request.namespace != "synthetic") {
				t.Fatalf("cleanup selected another backend identity: %+v", request)
			}
		})
	}
}

type stoppedCleanupIO struct {
	cio.IO
	closed bool
	err    error
}

func (stream *stoppedCleanupIO) Close() error {
	stream.closed = true
	return stream.err
}

type stoppedCleanupTask struct {
	containerd.Task
	state  containerd.ProcessStatus
	stream *stoppedCleanupIO
}

func (task *stoppedCleanupTask) Status(context.Context) (containerd.Status, error) {
	return containerd.Status{Status: task.state}, nil
}

func (task *stoppedCleanupTask) IO() cio.IO { return task.stream }

type stoppedCleanupContainer struct {
	containerd.Container
	task     containerd.Task
	taskErr  error
	labels   map[string]string
	loadedIO bool
}

func (*stoppedCleanupContainer) ID() string { return "synthetic-container" }

func (container *stoppedCleanupContainer) Task(_ context.Context, attach cio.Attach) (containerd.Task, error) {
	container.loadedIO = attach != nil
	return container.task, container.taskErr
}

func (container *stoppedCleanupContainer) Labels(context.Context) (map[string]string, error) {
	return container.labels, nil
}

func TestStoppedTaskCleanupPreservesExitHookOwnership(t *testing.T) {
	healthFailure := errors.New("synthetic health unit cleanup failed")
	ioFailure := errors.New("synthetic IO close failed")
	for _, scenario := range []struct {
		name       string
		state      containerd.ProcessStatus
		taskErr    error
		health     string
		healthErr  error
		ioErr      error
		wantErr    error
		wantHealth bool
		wantClosed bool
	}{
		{name: "stopped", state: containerd.Stopped, health: `{"test":["CMD","/fixture"]}`, wantHealth: true, wantClosed: true},
		{name: "disabled health", state: containerd.Stopped, health: `{"test":["NONE"]}`, wantClosed: true},
		{name: "running task refused", state: containerd.Running, wantErr: errdefs.ErrFailedPrecondition},
		{name: "reaped task", taskErr: errdefs.ErrNotFound, health: `{"test":["CMD","/fixture"]}`, wantHealth: true},
		{name: "health failure still closes IO", state: containerd.Stopped, health: `{"test":["CMD","/fixture"]}`, healthErr: healthFailure, wantErr: healthFailure, wantHealth: true, wantClosed: true},
		{name: "IO failure explicit", state: containerd.Stopped, ioErr: ioFailure, wantErr: ioFailure, wantClosed: true},
		{name: "caller cancellation retained", state: containerd.Stopped, health: `{"test":["CMD","/fixture"]}`, healthErr: context.Canceled, wantErr: context.Canceled, wantHealth: true, wantClosed: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			stream := &stoppedCleanupIO{err: scenario.ioErr}
			container := &stoppedCleanupContainer{
				task:    &stoppedCleanupTask{state: scenario.state, stream: stream},
				taskErr: scenario.taskErr,
				labels:  map[string]string{"nerdctl/healthcheck": scenario.health},
			}
			healthCalled := false
			err := cleanupStoppedContainer(context.Background(), container, func(_ context.Context, id string) error {
				if id != container.ID() {
					t.Fatalf("health cleanup selected another identity: %q", id)
				}
				healthCalled = true
				return scenario.healthErr
			})
			if !errors.Is(err, scenario.wantErr) || stream.closed != scenario.wantClosed ||
				healthCalled != scenario.wantHealth || !container.loadedIO {
				t.Fatalf("auxiliary cleanup: error=%v closed=%t health=%t loadedIO=%t",
					err, stream.closed, healthCalled, container.loadedIO)
			}
		})
	}
}

type stoppedHealthUnits struct {
	units   []dbus.UnitStatus
	stopped []string
	err     error
	cancel  context.CancelFunc
}

func (units *stoppedHealthUnits) ListUnitsByNamesContext(context.Context, []string) ([]dbus.UnitStatus, error) {
	return units.units, nil
}

func (units *stoppedHealthUnits) StopUnitContext(_ context.Context, name, _ string, result chan<- string) (int, error) {
	units.stopped = append(units.stopped, name)
	if units.err != nil {
		return 0, units.err
	}
	if units.cancel != nil {
		units.cancel()
	} else {
		result <- "done"
	}
	return 1, nil
}

func TestStoppedHealthCleanupTargetsOnlyLiveOwnedUnits(t *testing.T) {
	failure := errors.New("synthetic systemd permission failure")
	for _, scenario := range []struct {
		name   string
		units  []dbus.UnitStatus
		err    error
		cancel bool
		want   []string
		result error
	}{
		{name: "absent units", units: []dbus.UnitStatus{{Name: "synthetic.timer", LoadState: "not-found"}}},
		{name: "inactive unit", units: []dbus.UnitStatus{{Name: "synthetic.timer", LoadState: "loaded", ActiveState: "inactive"}}},
		{name: "timer before service", units: []dbus.UnitStatus{
			{Name: "synthetic.service", LoadState: "loaded", ActiveState: "active"},
			{Name: "synthetic.timer", LoadState: "loaded", ActiveState: "active"},
			{Name: "unrelated.service", LoadState: "loaded", ActiveState: "active"},
		}, want: []string{"synthetic.timer", "synthetic.service"}},
		{name: "unit collected concurrently", units: []dbus.UnitStatus{{Name: "synthetic.timer", LoadState: "loaded", ActiveState: "active"}},
			err: godbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}, want: []string{"synthetic.timer"}},
		{name: "permission failure explicit", units: []dbus.UnitStatus{{Name: "synthetic.timer", LoadState: "loaded", ActiveState: "active"}},
			err: failure, want: []string{"synthetic.timer"}, result: failure},
		{name: "caller cancellation", units: []dbus.UnitStatus{{Name: "synthetic.timer", LoadState: "loaded", ActiveState: "active"}},
			cancel: true, want: []string{"synthetic.timer"}, result: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			units := &stoppedHealthUnits{units: scenario.units, err: scenario.err}
			if scenario.cancel {
				units.cancel = cancel
			}
			err := stopExistingHealthUnits(ctx, units, "synthetic")
			if !errors.Is(err, scenario.result) || !reflect.DeepEqual(units.stopped, scenario.want) {
				t.Fatalf("health unit cleanup: error=%v stopped=%v, want error=%v stopped=%v",
					err, units.stopped, scenario.result, scenario.want)
			}
		})
	}
}
