package ui

// SetApp sets the app reference for data providers.
// This is called after the app is created to avoid circular dependencies.
func (m *Model) SetApp(app any) {
	m.app = app
}
