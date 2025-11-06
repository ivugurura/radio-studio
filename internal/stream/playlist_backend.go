package stream

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

type PlaylistSource interface {
	ensure()
	current() (Track, bool)
	nextTrack() (Track, bool)
	advance() (Track, bool)
	forceReload()
}

type backendPlaylist struct {
	mu        sync.RWMutex
	studioID  string
	dir       string
	endpoint  string
	apiKey    string
	tracks    []Track
	idx       int
	lastFetch time.Time
	ttl       time.Duration
	client    *http.Client
}

type backendTrack struct {
	ID              string  `json:"id"`
	File            string  `json:"file"`
	Title           string  `json:"title"`
	Artist          string  `json:"artist,omitempty"`
	Album           string  `json:"album,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
}

func newBackendPlaylist(dir string, studioID string, endpoint string, apiKey string) PlaylistSource {
	return &backendPlaylist{
		dir:      dir,
		studioID: studioID,
		endpoint: endpoint,
		apiKey:   apiKey,
		idx:      -1,
		ttl:      5 * time.Second,
		client:   &http.Client{Timeout: 5 * time.Second},
	}
}

func (b *backendPlaylist) fetch() {
	req, err := http.NewRequest("GET", b.endpoint, nil)
	if err != nil {
		return
	}
	res, err := b.client.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return
	}

	var bTracks []backendTrack
	if err := json.NewDecoder(res.Body).Decode(&bTracks); err != nil {
		return
	}
	var out []Track
	for _, t := range bTracks {
		out = append(out, Track{
			ID:          t.ID,
			File:        filepath.Join(b.dir, t.File),
			Title:       t.Title,
			Artist:      t.Artist,
			Album:       t.Album,
			DurationSec: t.DurationSeconds,
		})
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tracks = out

	if len(b.tracks) == 0 {
		b.idx = -1
	} else if b.idx >= len(b.tracks) {
		b.idx = 0
	}
	b.lastFetch = time.Now()
}

func (b *backendPlaylist) ensure() {
	b.mu.RLock()
	stale := len(b.tracks) == 0 || time.Since(b.lastFetch) > b.ttl
	b.mu.RUnlock()

	if stale {
		b.fetch()
	}
}

func (b *backendPlaylist) current() (Track, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.idx < 0 || b.idx >= len(b.tracks) {
		return Track{}, false
	}
	return b.tracks[b.idx], true
}

func (b *backendPlaylist) nextTrack() (Track, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.tracks) == 0 {
		return Track{}, false
	}
	if b.idx < 0 {
		return b.tracks[0], true
	}
	n := (b.idx + 1) % len(b.tracks)
	return b.tracks[n], true
}

func (b *backendPlaylist) advance() (Track, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.tracks) == 0 {
		b.idx = -1
		return Track{}, false
	}
	if b.idx < 0 {
		b.idx = 0
	} else {
		b.idx = (b.idx + 1) % len(b.tracks)
	}
	return b.tracks[b.idx], true
}

func (b *backendPlaylist) forceReload() {
	b.mu.Lock()
	b.lastFetch = time.Time{}
	b.mu.Unlock()
}
