package stream

import (
	"context"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
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

	droppedFrames atomic.Int64
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
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.broadcast(chunk)
		}
		if err != nil {
			break
		}
	}
}

// HandleListen streams audio (live or AutoDJ) to a listener.
func (s *Studio) HandleListen(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	// Do NOT manually set Transfer-Encoding; Go will add chunked automatically.
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch := make(chan []byte, 2048) // larger buffer reduces drops
	s.listenersMu.Lock()
	s.listeners[ch] = struct{}{}
	curr := len(s.listeners)
	s.listenersMu.Unlock()

	log.Printf("Studio %s: new listener (total=%d)", s.ID, curr)

	defer func() {
		s.listenersMu.Lock()
		delete(s.listeners, ch)
		remaining := len(s.listeners)
		s.listenersMu.Unlock()
		close(ch)
		log.Printf("Studio %s: listener disconnected (total=%d)", s.ID, remaining)
	}()

	// Optional “kickstart”; you can remove if you suspect issues.
	// w.Write([]byte{0, 0, 0, 0})
	// flusher.Flush()

	for data := range ch {
		if len(data) == 0 {
			continue
		}
		if _, err := w.Write(data); err != nil {
			break
		}
		flusher.Flush()
	}
}

func (s *Studio) broadcast(data []byte) {
	s.listenersMu.RLock()
	defer s.listenersMu.RUnlock()
	for ch := range s.listeners {
		select {
		case ch <- data:
		default:
			s.droppedFrames.Add(1)
		}
	}
	// Occasionally log
	if v := s.droppedFrames.Load(); v > 0 && v%500 == 0 {
		log.Printf("Studio %s: dropped frames=%d (consider increasing listener buffer or redesign)", s.ID, v)
	}
}
