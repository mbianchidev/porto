package operations

import "context"

func (m *Manager) Wait(ctx context.Context, id string) (Operation, error) {
	m.mu.Lock()
	current := m.jobs[id]
	m.mu.Unlock()
	if current != nil {
		select {
		case <-current.done:
		case <-ctx.Done():
			return Operation{}, ctx.Err()
		}
	}
	return m.Get(id)
}
