package stream

import (
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

type Manager struct {
	studios      map[string]*Studio
	mu           sync.RWMutex
	audioBaseDir string
}

func NewManager(baseDir string) *Manager {
	return &Manager{
		studios:      make(map[string]*Studio),
		audioBaseDir: baseDir,
	}
}

func (m *Manager) RegisterStudio(studioID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.studios[studioID]; !exists {
		studioAudioDir := filepath.Join(m.audioBaseDir, studioID)
		m.studios[studioID] = NewStudio(studioID, studioAudioDir)
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
