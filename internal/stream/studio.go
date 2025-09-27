package stream

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type ListenerState struct {
	ch            chan []byte
	droppedInARow int
}

// Studio represents a radio studio/channel
type Studio struct {
	ID string

	// Live ingest (if present)
	liveMu     sync.RWMutex
	liveIngest io.ReadCloser
	liveActive atomic.Bool

	// Central feed: all upstream audio goes here (AutoDJ or live)
	feed chan []byte

	// listeners receives bytes (fan-out)
	listenersMu sync.RWMutex
	listeners   map[*ListenerState]struct{}

	droppedFrames atomic.Int64
}

func NewStudio(id string, dir string, audiodjFactory func(broadcast func([]byte)) *AutoDJ) *Studio {
	s := &Studio{
		ID:        id,
		feed:      make(chan []byte, 8192),
		listeners: make(map[*ListenerState]struct{}),
	}

	// Create AutoDJ with factory (lets you inject bitrate)
	autodj := audiodjFactory(func(data []byte) {
		if !s.liveActive.Load() {
			s.pushToFeed(data)
		}
	})

	// Start distributor + AutoDJ
	go s.distribute()
	go autodj.Play(context.Background())
	return s
}

func (s *Studio) pushToFeed(data []byte) {
	// Non-blocking feed send; if full, drop (rare if sized well)
	select {
	case s.feed <- data:
	default:
		// could log; but dropping at feed level should be exceptional
	}
}

func (s *Studio) removeListener(ls *ListenerState) {
	s.listenersMu.Lock()
	delete(s.listeners, ls)
	s.listenersMu.Unlock()
}

func (s *Studio) distribute() {
	log.Printf("Studio %s: distributer started", s.ID)
	for data := range s.feed {
		s.listenersMu.RLock()
		for ls := range s.listeners {
			select {
			case ls.ch <- data:
				ls.droppedInARow = 0
			default:
				ls.droppedInARow++
				if ls.droppedInARow > 50 {
					close(ls.ch)
					s.listenersMu.RUnlock()
					s.removeListener(ls)
					s.listenersMu.RLock()
					log.Printf("Studio %s: dropped slow listener", s.ID)
				}
			}
		}
		s.listenersMu.RUnlock()
		if v := s.droppedFrames.Load(); v > 0 && v%500 == 0 {
			log.Printf("Studio %s: dropped listener frames=%d (consider larger listener buffers)", s.ID, v)
		}
	}
	log.Printf("Studio %s: distributor stopped", s.ID)
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
	s.liveActive.Store(true)
	s.liveMu.Unlock()

	log.Printf("Studio %s: live stream started", s.ID)

	buf := make([]byte, 8192)
	for {
		n, err := s.liveIngest.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.pushToFeed(chunk)
		}
		if err != nil {
			break
		}
	}
	s.liveMu.Lock()
	if s.liveIngest != nil {
		_ = s.liveIngest.Close()
		s.liveIngest = nil
	}
	s.liveActive.Store(false)
	s.liveMu.Unlock()
	log.Printf("Studio %s: live stream ended", s.ID)
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

	ls := &ListenerState{
		ch: make(chan []byte, 512),
	}
	s.listenersMu.Lock()
	s.listeners[ls] = struct{}{}
	total := len(s.listeners)
	s.listenersMu.Unlock()
	log.Printf("Studio %s: new listener (total=%d)", s.ID, total)

	defer func() {
		s.removeListener(ls)
		log.Printf("Studio %s: listener disconnected", s.ID)
	}()

	// Optional “kickstart”; you can remove if you suspect issues.
	// w.Write([]byte{0, 0, 0, 0})
	// flusher.Flush()

	for data := range ls.ch {
		if len(data) == 0 {
			continue
		}
		if _, err := w.Write(data); err != nil {
			break
		}
		flusher.Flush()
	}
}

// Example status endpoint (extend with richer JSON / metrics).
func (s *Studio) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Simple plain text (replace with JSON if you add a JSON encoder)
	s.listenersMu.RLock()
	listenerCount := len(s.listeners)
	s.listenersMu.RUnlock()

	live := s.liveActive.Load()
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(
		strings.Join([]string{
			"studio=" + s.ID,
			"live=" + boolToString(live),
			"listeners=" + intToString(listenerCount),
			"", // newline at end
		}, "\n"),
	))
}

func SortedMP3Files(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(strings.ToLower(name), ".mp3") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func boolToString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func intToString(i int) string {
	return strconv.Itoa(i)
}
