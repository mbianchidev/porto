package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

func (m *Manager) StartContainerInventory(ctx context.Context) error {
	if ctx == nil {
		return errors.New("container inventory context is required")
	}
	m.inventoryMu.Lock()
	defer m.inventoryMu.Unlock()
	if m.inventoryCancel != nil {
		return nil
	}
	connector := m.runtimeConnector
	if connector == nil {
		connector = m.connectContainerRuntime
	}
	inventory := newContainerInventory(connector, defaultInventoryOptions())
	inventoryContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.inventory = inventory
	m.inventoryCancel = cancel
	m.inventoryDone = done
	healthDone := make(chan struct{})
	healthContext, healthCancel := context.WithCancel(inventoryContext)
	m.healthCancel = healthCancel
	m.healthDone = healthDone
	go func() {
		defer close(done)
		inventory.run(inventoryContext)
	}()
	go func() {
		defer close(healthDone)
		m.runHealthScheduler(healthContext)
	}()
	return nil
}

func (m *Manager) StopContainerInventory(ctx context.Context) error {
	m.inventoryMu.Lock()
	cancel := m.inventoryCancel
	done := m.inventoryDone
	inventory := m.inventory
	healthCancel := m.healthCancel
	healthDone := m.healthDone
	m.inventoryCancel = nil
	m.inventoryDone = nil
	m.healthCancel = nil
	m.healthDone = nil
	m.inventoryMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	if healthCancel != nil {
		healthCancel()
	}
	select {
	case <-done:
		if healthDone != nil {
			select {
			case <-healthDone:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		m.inventoryMu.Lock()
		if m.inventory == inventory {
			m.inventory = nil
		}
		m.inventoryMu.Unlock()
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

const inactiveInventoryInstanceID = "inactive"

func (m *Manager) ContainerSnapshot() ContainerSnapshot {
	m.inventoryMu.Lock()
	inventory := m.inventory
	m.inventoryMu.Unlock()
	if inventory == nil {
		return ContainerSnapshot{
			InstanceID:   inactiveInventoryInstanceID,
			Message:      "Container inventory is not running",
			Containers:   []Container{},
			Capabilities: containerCapabilities(),
		}
	}
	return inventory.snapshotValue()
}

// ContainerSnapshotVersion identifies the published inventory snapshot without
// copying it. Container data only changes together with a new revision, so
// periodic readers can reuse a snapshot while the version is unchanged.
func (m *Manager) ContainerSnapshotVersion() (instanceID string, revision uint64) {
	m.inventoryMu.Lock()
	inventory := m.inventory
	m.inventoryMu.Unlock()
	if inventory == nil {
		return inactiveInventoryInstanceID, 0
	}
	inventory.mu.RLock()
	defer inventory.mu.RUnlock()
	return inventory.snapshot.InstanceID, inventory.snapshot.Revision
}

// containerSnapshotCache reuses a cloned snapshot until the inventory publishes
// another version. It is owned by one goroutine and its snapshot is read-only.
type containerSnapshotCache struct {
	snapshot ContainerSnapshot
	loaded   bool
}

func (c *containerSnapshotCache) load(m *Manager) ContainerSnapshot {
	instanceID, revision := m.ContainerSnapshotVersion()
	if !c.loaded || c.snapshot.InstanceID != instanceID || c.snapshot.Revision != revision {
		c.snapshot = m.ContainerSnapshot()
		c.loaded = true
	}
	return c.snapshot
}

func (m *Manager) containerInventoryFresh() bool {
	m.inventoryMu.Lock()
	inventory := m.inventory
	m.inventoryMu.Unlock()
	if inventory == nil {
		return false
	}
	inventory.mu.RLock()
	defer inventory.mu.RUnlock()
	return inventory.snapshot.Available && !inventory.snapshot.Stale
}

func (m *Manager) SubscribeContainerSnapshots() (<-chan ContainerSnapshot, func()) {
	channel, _, unsubscribe := m.SubscribeContainerSnapshotsWithInitial()
	return channel, unsubscribe
}

func (m *Manager) SubscribeContainerSnapshotsWithInitial() (
	<-chan ContainerSnapshot,
	ContainerSnapshot,
	func(),
) {
	m.inventoryMu.Lock()
	inventory := m.inventory
	m.inventoryMu.Unlock()
	if inventory == nil {
		channel := make(chan ContainerSnapshot, 1)
		initial := m.ContainerSnapshot()
		channel <- initial
		close(channel)
		return channel, initial, func() {}
	}
	return inventory.subscribeWithSnapshot()
}

func (m *Manager) activeContainerInventory() *containerInventory {
	m.inventoryMu.Lock()
	defer m.inventoryMu.Unlock()
	if m.inventoryCancel == nil {
		return nil
	}
	select {
	case <-m.inventoryDone:
		return nil
	default:
		return m.inventory
	}
}

func (m *Manager) invalidateContainerInventory() {
	if inventory := m.activeContainerInventory(); inventory != nil {
		inventory.triggerRefresh()
	}
}

func (m *Manager) completeContainerCreation(ctx context.Context, id string, creationErr error) (string, error) {
	if creationErr != nil {
		return id, creationErr
	}
	if id == "" {
		return "", errors.New("container runtime returned no created container identity")
	}
	err := m.confirmContainerInventory(ctx, func(containers []Container) bool {
		for _, container := range containers {
			if container.ID == id {
				return true
			}
		}
		return false
	})
	if err != nil {
		return id, fmt.Errorf("container %q was created, but inventory confirmation failed: %w", id, err)
	}
	return id, nil
}

// lifecycleSettled recognizes a container's state after a completed lifecycle action. A started
// container may already have exited, which a new exit time reveals.
func lifecycleSettled(action string) func(before, after Container) bool {
	active := func(state string) bool {
		return state == "running" || state == "paused" || state == "pausing" || state == "restarting"
	}
	switch action {
	case "stop":
		return func(_, after Container) bool { return !active(after.State) }
	case "pause":
		return func(_, after Container) bool { return after.State == "paused" || !active(after.State) }
	case "start", "unpause":
		return func(before, after Container) bool { return after.State == "running" || after.ExitAt != before.ExitAt }
	case "restart":
		return func(before, after Container) bool {
			return (after.State == "running" && after.PID != before.PID) || after.ExitAt != before.ExitAt
		}
	default:
		return nil
	}
}

// confirmLifecycleInventory waits until listings show a completed lifecycle action, as Docker's
// do when the API call returns; Compose otherwise skips starting a container listed as running.
func (m *Manager) confirmLifecycleInventory(ctx context.Context, before Container, action string, settled func(before, after Container) bool) {
	err := m.confirmContainerInventory(ctx, func(containers []Container) bool {
		for _, container := range containers {
			if container.ID == before.ID {
				return settled(before, container)
			}
		}
		return true
	})
	if err != nil {
		slog.Warn("Container inventory did not confirm a lifecycle change", "container", before.ID, "action", action, "error", err)
	}
}

func (m *Manager) confirmContainerInventory(ctx context.Context, matches func([]Container) bool) error {
	inventory := m.activeContainerInventory()
	if inventory == nil || m.IsNativeWindows() {
		m.invalidateContainerInventory()
		return nil
	}
	updates, unsubscribe := inventory.subscribe()
	defer unsubscribe()
	inventory.triggerCommittedRefresh()
	timer := time.NewTimer(defaultInventoryOperationTimeout)
	defer timer.Stop()
	var err error
	for {
		select {
		case <-ctx.Done():
			err = context.Cause(ctx)
		case <-timer.C:
			err = errors.New("timed out waiting for the committed container change in inventory")
		case snapshot, ok := <-updates:
			if !ok {
				err = errors.New("container inventory subscription closed")
				break
			}
			if ctx.Err() != nil {
				err = context.Cause(ctx)
				break
			}
			if snapshot.Available && !snapshot.Stale && matches(snapshot.Containers) {
				return nil
			}
		}
		if err != nil {
			return err
		}
	}
}
