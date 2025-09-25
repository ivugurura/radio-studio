package stream

import (
	"net/http"
	"strings"
	"sync"
)

type Manager struct {
	studios map[string]*Studio
	mu      sync.RWMutex
}

func NewManager() *Manager {
	return &Manager{
		studios: make(map[string]*Studio),
	}
}

func (m *Manager) RegisterStudio(studioID string) {
	m.mu.Lock()
	defer m.mu.Lock()
	if _, exists := m.studios[studioID]; !exists {
		m.studios[studioID] = NewStudio(studioID)
	}
}

func (m *Manager) GetStudio(studioID string) (*Studio, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	studio, ok := m.studios[studioID]

	return studio, ok
}

func (m *Manager) RouteStudioRequest(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/studio/"), "/")
	if len(parts) < 2 {
		http.Error(w, "Invalid studio endpoint", http.StatusBadRequest)
		return
	}
}
