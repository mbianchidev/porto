package docker

import "sync"

type containerMutexes struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newContainerMutexes() *containerMutexes {
	return &containerMutexes{locks: make(map[string]*sync.Mutex)}
}

func (m *containerMutexes) lock(id string) func() {
	if m == nil {
		return func() {}
	}
	m.mu.Lock()
	lock := m.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		m.locks[id] = lock
	}
	m.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}
