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
	m.mu.RLock()
	defer m.mu.RUnlock()
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

	studioID, action := parts[0], parts[1]
	studio, ok := m.GetStudio(studioID)
	if !ok {
		http.Error(w, "Studio not found", http.StatusNotFound)
		return
	}

	switch action {
	case "live":
		studio.HandleLiveIngest(w, r)
	case "listen":
		studio.HandleListen(w, r)
	default:
		http.Error(w, "Unkown action", http.StatusNotFound)
	}
}
