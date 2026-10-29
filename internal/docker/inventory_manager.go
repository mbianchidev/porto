package docker

import (
	"context"
	"errors"
	"fmt"
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

func (m *Manager) ContainerSnapshot() ContainerSnapshot {
	m.inventoryMu.Lock()
	inventory := m.inventory
	m.inventoryMu.Unlock()
	if inventory == nil {
		return ContainerSnapshot{
			InstanceID:   "inactive",
			Message:      "Container inventory is not running",
			Containers:   []Container{},
			Capabilities: containerCapabilities(),
		}
	}
	return inventory.snapshotValue()
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

func (m *Manager) confirmContainerInventory(ctx context.Context, matches func([]Container) bool) error {
	inventory := m.activeContainerInventory()
	if inventory == nil || m.IsNativeWindows() {
		m.invalidateContainerInventory()
		return nil
	}
	updates, unsubscribe := inventory.subscribe()
	defer unsubscribe()
	inventory.triggerRefresh()
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
