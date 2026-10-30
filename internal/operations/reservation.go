package operations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type reservationKey struct{}
type reservation struct {
	manager      *Manager
	resource, id string
}
type manualReservation struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (m *Manager) InJob(ctx context.Context, resource string) bool {
	owner, ok := ctx.Value(reservationKey{}).(reservation)
	if !ok || owner.manager != m || owner.resource != resource {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[owner.id] != nil && m.resources[resource] == owner.id
}

func (m *Manager) ownerContext(ctx context.Context, resource, id string) context.Context {
	return context.WithValue(ctx, reservationKey{}, reservation{m, resource, id})
}

func (m *Manager) BusyFor(ctx context.Context, resource string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.resources[resource]
	if id == "" {
		return false
	}
	owner, ok := ctx.Value(reservationKey{}).(reservation)
	return !ok || owner.manager != m || owner.resource != resource || owner.id != id
}

func (m *Manager) Reserve(ctx context.Context, resource string) (context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || len(m.resources) >= 16 || m.resources[resource] != "" {
		return ctx, nil, ErrBusy
	}
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return ctx, nil, err
	}
	id := hex.EncodeToString(bytes)
	ctx, cancel := context.WithCancel(ctx)
	current := &manualReservation{cancel: cancel, done: make(chan struct{})}
	m.reservations[id] = current
	m.resources[resource] = id
	release := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.resources[resource] == id {
			delete(m.resources, resource)
			delete(m.reservations, id)
			current.cancel()
			close(current.done)
		}
	}
	return m.ownerContext(ctx, resource, id), release, nil
}
