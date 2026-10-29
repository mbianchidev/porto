package delivery

func (m *Manager) OwnsDefinition(id int64, path string) bool {
	return path != "" && path == m.currentPath(id)
}
