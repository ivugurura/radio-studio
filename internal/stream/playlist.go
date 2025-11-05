package stream

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Track struct {
	File        string
	Path        string
	Title       string
	Artist      string
	Album       string
	DurationSec float64
}

type playlistState struct {
	mu          sync.RWMutex
	tracks      []Track
	dir         string
	lastModTime time.Time
	idx         int
}

func newPlaylistState(dir string) *playlistState {
	return &playlistState{
		dir: dir,
		idx: -1,
	}
}

func (p *playlistState) reload(mod time.Time) {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return
	}
	var list []Track
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(strings.ToLower(name), ".mp3") {
			list = append(list, Track{
				File: name,
				Path: filepath.Join(p.dir, name),
			})
		}
		sort.Slice(list, func(i int, j int) bool {
			return list[i].File < list[j].File
		})
		p.mu.Lock()
		p.tracks = list
		p.lastModTime = mod
		tracksCount := len(p.tracks)
		if tracksCount == 0 {
			p.idx = -1
		} else if p.idx >= tracksCount {
			p.idx = 0
		}
		p.mu.Unlock()
	}
}

// ensure (re)loads playlist if directory mod time has advanced or empty
func (pls *playlistState) ensure() {
	info, err := os.Stat(pls.dir)
	if err != nil {
		return
	}
	mod := info.ModTime()
	pls.mu.RLock()
	stale := len(pls.tracks) == 0 || mod.After(pls.lastModTime)
	pls.mu.RUnlock()

	if !stale {
		return
	}
	pls.reload(mod)
}

func (p *playlistState) current() (Track, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.idx < 0 || p.idx >= len(p.tracks) {
		return Track{}, false
	}
	return p.tracks[p.idx], true
}

func (p *playlistState) nextTrack() (Track, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.tracks) == 0 {
		return Track{}, false
	}
	if p.idx < 0 {
		// Not started yet; "next" is the first
		return p.tracks[0], true
	}
	n := (p.idx + 1) % len(p.tracks)
	return p.tracks[n], true
}

func (p *playlistState) advance() (Track, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tracks) == 0 {
		p.idx = -1
		return Track{}, false
	}
	if p.idx < 0 {
		p.idx = 0
	} else {
		p.idx = (p.idx + 1) % len(p.tracks)
	}
	return p.tracks[p.idx], true
}

// force reload on external request (e.g., after upload)
func (p *playlistState) forceReload() {
	info, err := os.Stat(p.dir)
	if err != nil {
		return
	}
	p.reload(info.ModTime())
}
