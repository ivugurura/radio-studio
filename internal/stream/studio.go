package stream

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// Studio represents a radio studio/channel
type Studio struct {
	ID       string
	audioDir string

	// liveIngest is the current live stream source, if any
	liveMu     sync.RWMutex
	liveIngest io.ReadCloser

	// listeners receives bytes (fan-out)
	listenersMu sync.RWMutex
	listeners   map[chan []byte]struct{}

	// TODO: Add playlist/AutoDJ support
	autoDJ    *AutoDJ
	cancelADJ context.CancelFunc
}

func NewStudio(id string, dir string) *Studio {
	studio := &Studio{
		ID:        id,
		audioDir:  dir,
		listeners: make(map[chan []byte]struct{}),
	}
	studio.startAutoDJ()
	return studio
}

func (s *Studio) startAutoDJ() {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancelADJ = cancel
	s.autoDJ = NewAutoDJ(s.audioDir, func(data []byte) {
		s.broadcast(data)
	})
	go s.autoDJ.Play(ctx)
}

func (s *Studio) stopAutoDJ() {
	if s.cancelADJ != nil {
		s.cancelADJ()
	}
}

// HandleLiveIngest is called when a live encoder (e.g., BUTT) streams audio to the server.
// Only one live stream at a time is supported per studio.
func (s *Studio) HandleLiveIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.liveMu.Lock()
	if s.liveIngest != nil {
		s.liveIngest.Close() // Stop any previous live stream
	}

	s.liveIngest = r.Body
	s.liveMu.Unlock()

	defer func() {
		s.liveMu.Lock()
		s.liveIngest = nil
		s.liveMu.Unlock()
		s.startAutoDJ()
	}()

	buf := make([]byte, 4096)
	for {
		n, err := s.liveIngest.Read(buf)
		if n > 0 {
			s.broadcast(buf[:n])
		}
		if err != nil {
			break
		}
	}
}

// HandleListen streams audio (live or AutoDJ) to a listener.
func (s *Studio) HandleListen(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/mpeg") // or appropriate mime type
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch := make(chan []byte, 256)
	s.listenersMu.Lock()
	s.listeners[ch] = struct{}{}
	s.listenersMu.Unlock()
	defer func() {
		s.listenersMu.Lock()
		delete(s.listeners, ch)
		s.listenersMu.Unlock()
		close(ch)
	}()

	// TODO: If not live, play AutoDJ (playlist)
	for data := range ch {
		_, _ = w.Write(data)
		flusher.Flush()
	}
}

func (s *Studio) broadcast(data []byte) {
	s.listenersMu.RLock()
	defer s.listenersMu.RUnlock()

	for ch := range s.listeners {
		// Non-blocking send to prevent slow listeners from blocking
		select {
		case ch <- data:
		default:
		}
	}
}
