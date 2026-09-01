package docker

import (
	"context"
	"errors"
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
	go func() {
		defer close(done)
		inventory.run(inventoryContext)
	}()
	return nil
}

func (m *Manager) StopContainerInventory(ctx context.Context) error {
	m.inventoryMu.Lock()
	cancel := m.inventoryCancel
	done := m.inventoryDone
	inventory := m.inventory
	m.inventoryCancel = nil
	m.inventoryDone = nil
	m.inventoryMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
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
	m.inventoryMu.Lock()
	inventory := m.inventory
	m.inventoryMu.Unlock()
	if inventory == nil {
		channel := make(chan ContainerSnapshot, 1)
		channel <- m.ContainerSnapshot()
		close(channel)
		return channel, func() {}
	}
	return inventory.subscribe()
}

func (m *Manager) activeContainerInventory() *containerInventory {
	m.inventoryMu.Lock()
	defer m.inventoryMu.Unlock()
	if m.inventoryCancel == nil {
		return nil
	}
	return m.inventory
}

func (m *Manager) invalidateContainerInventory() {
	if inventory := m.activeContainerInventory(); inventory != nil {
		inventory.triggerRefresh()
	}
}

func (m *Manager) waitForContainerInventory(ctx context.Context, minimumRevision uint64) (ContainerSnapshot, error) {
	updates, unsubscribe := m.SubscribeContainerSnapshots()
	defer unsubscribe()
	timer := time.NewTimer(defaultInventoryOperationTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ContainerSnapshot{}, context.Cause(ctx)
		case <-timer.C:
			return ContainerSnapshot{}, errors.New("timed out waiting for container inventory")
		case snapshot, ok := <-updates:
			if !ok {
				return ContainerSnapshot{}, errors.New("container inventory subscription closed")
			}
			if snapshot.Revision > minimumRevision {
				return snapshot, nil
			}
		}
	}
}
