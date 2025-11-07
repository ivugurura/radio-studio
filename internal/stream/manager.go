package stream

import (
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ivugurura/radio-studio/internal/geo"
	"github.com/ivugurura/radio-studio/internal/netutil"
)

// StudioFactory lets you customize how studios are structures
// Useful if later you inject DB handles, metrics, logger, bitrate, etc
type RequestValidator func(r *http.Request, studioID, action string) error

type StudioFactory func(id, audioDir string, bitrateKbps int, geoR *geo.Resolver, autoDJFactory AutoDJFactory, snapshotInterval time.Duration) *Studio

type ManagerOption func(*Manager)

func WithRequestValidator(v RequestValidator) ManagerOption {
	return func(m *Manager) { m.validator = v }
}

func WithStudioFactory(f StudioFactory) ManagerOption {
	return func(m *Manager) { m.factory = f }
}

func WithDefaultBitrate(kbps int) ManagerOption {
	return func(m *Manager) { m.defaultBitrateKbps = kbps }
}

func WithSnapshotInterval(d time.Duration) ManagerOption {
	return func(m *Manager) { m.snapshotInterval = d }
}

func WithAutoDJFactory(f AutoDJFactory) ManagerOption {
	return func(m *Manager) { m.autoDJFactory = f }
}

// Manager coordinates all studios
type Manager struct {
	mu           sync.RWMutex
	studios      map[string]*Studio
	audioBaseDir string
	geoResolver  *geo.Resolver

	defaultBitrateKbps int
	snapshotInterval   time.Duration
	autoDJFactory      AutoDJFactory

	validator RequestValidator
	factory   StudioFactory
}

// NewManager create a new Manager
// defaultBitrateKbps influencesthe AutoDJ pacing logic for all new studio(if you use pacing version)
func NewManager(baseDir string, geoR *geo.Resolver, opts ...ManagerOption) *Manager {
	m := &Manager{
		studios:            make(map[string]*Studio),
		audioBaseDir:       baseDir,
		defaultBitrateKbps: 128,
		geoResolver:        geoR,
		snapshotInterval:   5 * time.Second,
		// This line needs a close look
		// autoDJFactory:      NewAutoDJ,
		factory: func(id, dir string, bitrate int, geoR *geo.Resolver, dj AutoDJFactory, snapInt time.Duration) *Studio {
			return NewStudio(id, dir, bitrate, geoR, dj, snapInt)
		},
	}

	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Manager) SetFactory(f StudioFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.factory = f
}

func (m *Manager) RegisterStudio(studioID string) *Studio {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.studios[studioID]; ok {
		return s
	}
	dir := filepath.Join(m.audioBaseDir, studioID)
	studio := m.factory(studioID, dir, m.defaultBitrateKbps, m.geoResolver, m.autoDJFactory, m.snapshotInterval)
	m.studios[studioID] = studio
	log.Printf("Manager: registered studio %s (audioDir=%s)", studioID, dir)

	return studio
}

func (m *Manager) GetStudio(studioID string) (*Studio, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.studios[studioID]

	return s, ok
}

// ListStudios returns all studio IDs (snapshot)
func (m *Manager) ListStudios() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.studios))
	for id := range m.studios {
		out = append(out, id)
	}
	return out
}

// RemoveStudio stops and delete a studio
// Any listeners are disconnected; in-flight HTTP responses end
func (m *Manager) RemoveStudio(studioID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.studios[studioID]
	if !ok {
		return errors.New("Studio not found")
	}
	// s.Close() // Todo: Implement close in Studio for cleanup
	delete(m.studios, studioID)
	log.Printf("Manager: removed studio %s", studioID)
	return nil
}

// Shutdown closes all studios gracefully
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	log.Printf("Manager: shutting down (%d studios)", len(m.studios))
	var wg sync.WaitGroup
	for id, studio := range m.studios {
		wg.Add(1)
		go func(id string, st *Studio) {
			defer wg.Done()
			// st.Close()
			log.Printf("Manager: studio %s closed", id)
		}(id, studio)
	}
	wg.Wait()
}

// Example graceful shutdown hook usage:
// In main.go:
//  mgr := NewManager("./audio", 128)
//  defer mgr.Shutdown()

// (Optional) Periodic logging of manager state (for ops visibility)
func (m *Manager) StartMonitor(interval time.Duration, stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				ids := m.ListStudios()
				log.Printf("Manager monitor: studios=%v", ids)
			case <-stop:
				return
			}
		}
	}()
}

// RouteStudioRequest parses path and forwards to the appropriate studio handler.
// Expected pattern: /studio/{id}/{action}
// Actions: listen | live (extend as needed: metadata, status, etc.)
func (m *Manager) RouteStudioRequest(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/studio/"), "/")
	if len(parts) < 2 {
		netutil.ServerResponse(w, 400, "Invalid studio endpoint", nil)
		return
	}

	studioID, action := parts[0], parts[1]
	studio, ok := m.GetStudio(studioID)
	if !ok {
		netutil.ServerResponse(w, 404, "Studio not found", nil)
		return
	}

	switch action {
	case "live":
		studio.HandleLiveIngest(w, r)
	case "listen":
		studio.HandleListen(w, r)
	case "status":
		studio.HandleStatus(w, r)
	case "snapshot":
		studio.HandleSnapshot(w, r)
	case "skip":
		studio.HandleSkip(w, r)
	case "now":
		studio.HandleNowPlaying(w, r)
	default:
		netutil.ServerResponse(w, 404, "Unknown action", nil)
	}
}
