package stream

import (
	"context"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ivugurura/radio-studio/internal/geo"
	"github.com/ivugurura/radio-studio/internal/listeners"
	"github.com/ivugurura/radio-studio/internal/netutil"
)

type NowPlayingResponse struct {
	StudioID   string    `json:"studio_id"`
	Current    string    `json:"current"`
	Next       string    `json:"next,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	ElapsedSec float64   `json:"elapsed_sec"`
}

type StudioSnapshot struct {
	GeneratedAt time.Time      `json:"generated_at"`
	StudioID    string         `json:"studio_id"`
	Active      int            `json:"active"`
	Countries   map[string]int `json:"countries"`
	ClientTypes map[string]int `json:"client_types"`
	BytesTotal  int64          `json:"bytes_total"`
	LiveActive  bool           `json:"live_active"`
	Current     string         `json:"current"`
	Next        string         `json:"next"`
}

type studioStatus struct {
	Studio         string `json:"studio"`
	IsLive         bool   `json:"is_live"`
	ListenersCount int    `json:"listeners_count"`
}

type streamListener struct {
	l             *listeners.Listener
	ch            chan []byte
	droppedInARow int
}

// Studio represents a radio studio/channel
type Studio struct {
	ID          string
	audioDir    string
	bitrateKbps int

	// Live ingest (if present)
	liveMu     sync.RWMutex
	liveIngest io.ReadCloser
	liveActive atomic.Bool

	// In Studio struct
	liveMetaMu sync.RWMutex
	liveMeta   *LiveMeta

	// Central feed: all upstream audio goes here (AutoDJ or live)
	feed chan []byte

	// listeners receives bytes (fan-out)
	listenersMu     sync.RWMutex
	streamListeners map[*streamListener]struct{}
	listenersStore  *listeners.Store

	// snapshot
	snapshotMu       sync.RWMutex
	lastSnapshot     StudioSnapshot
	snapshotInterval time.Duration
	stop             chan struct{}

	geoResolver  *geo.Resolver
	autoDJ       AutoDJ
	autoDJCancel context.CancelFunc
}

func NewStudio(id string, dir string, brKbps int, geoR *geo.Resolver, autoDJF AutoDJFactory, snapIn time.Duration) *Studio {
	s := &Studio{
		ID:               id,
		audioDir:         dir,
		bitrateKbps:      brKbps,
		feed:             make(chan []byte, 4096),
		listenersStore:   listeners.NewStore(),
		streamListeners:  make(map[*streamListener]struct{}),
		geoResolver:      geoR,
		snapshotInterval: snapIn,
		stop:             make(chan struct{}),
	}

	// Start distributor + AutoDJ
	go s.distribute()
	if autoDJF != nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.autoDJCancel = cancel
		s.autoDJ = autoDJF(dir, brKbps, func(b []byte) {
			// If you want to suppress AutoDJ during live, check s.liveActive.Load() here
			if s.liveActive.Load() {
				return
			}
			s.push(b)
		})
		go s.autoDJ.Play(ctx)
	}
	go s.snapshotLoop()
	return s
}

func (s *Studio) setLiveMeta(m LiveMeta) {
	s.liveMetaMu.Lock()
	s.liveMeta = &m
	s.liveMetaMu.Unlock()
}

func (s *Studio) clearLiveMeta() {
	s.liveMetaMu.Lock()
	s.liveMeta = nil
	s.liveMetaMu.Unlock()
}

func (s *Studio) LiveMeta() *LiveMeta {
	s.liveMetaMu.RLock()
	defer s.liveMetaMu.RUnlock()
	if s.liveMeta == nil {
		return nil
	}
	// Return a copy
	m := *s.liveMeta
	return &m
}

// func (s *Studio) startAutoDJ() {
// 	ctx, cancel := context.WithCancel(context.Background())
// 	s.autoDJCancel = cancel
// 	runner := s.autoDJ(s.audioDir, s.bitrateKbps, func(b []byte) {
// 		// If you want to suppress AutoDJ during live, check s.liveActive.Load() here
// 		if s.liveActive.Load() {
// 			return
// 		}
// 		s.push(b)
// 	})
// 	go runner.Play(ctx)
// }

func (s *Studio) snapshotLoop() {
	t := time.NewTicker(s.snapshotInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.buildSnapshot()
		case <-s.stop:
			return
		}
	}
}

func (s *Studio) Close() {
	close(s.stop)
	if s.autoDJCancel != nil {
		s.autoDJCancel()
	}
	close(s.feed)
}

func (s *Studio) push(data []byte) {
	// Non-blocking feed send; if full, drop (rare if sized well)
	select {
	case s.feed <- data:
	default:
		// could log; but dropping at feed level should be exceptional
	}
}

func (s *Studio) removeListener(sl *streamListener) {
	s.listenersMu.Lock()
	delete(s.streamListeners, sl)
	s.listenersMu.Unlock()
}

func (s *Studio) buildSnapshot() {
	active := s.listenersStore.Active()
	snap := StudioSnapshot{
		GeneratedAt: time.Now().UTC(),
		StudioID:    s.ID,
		Countries:   make(map[string]int),
		ClientTypes: make(map[string]int),
	}
	var totalBytes int64
	for _, l := range active {
		snap.Active++
		c := l.Country
		if c == "" {
			c = "UN"
		}
		snap.Countries[c]++
		ct := l.ClientType
		if ct == "" {
			ct = "unknown"
		}
		snap.ClientTypes[ct]++
		totalBytes += l.ByteSent.Load()
	}
	snap.BytesTotal = totalBytes
	s.snapshotMu.Lock()
	s.lastSnapshot = snap
	s.snapshotMu.Unlock()
}

func (s *Studio) Snapshot() StudioSnapshot {
	s.snapshotMu.RLock()
	defer s.snapshotMu.RUnlock()
	return s.lastSnapshot
}

func (s *Studio) distribute() {
	log.Printf("Studio %s: distributer started", s.ID)
	for data := range s.feed {
		s.listenersMu.RLock()
		for ls := range s.streamListeners {
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
			ls.l.ByteSent.Add(int64(len(data)))
			// Heartbeat update every ~5s
			if hb := ls.l.LastHeartbeat.Load(); hb != nil {
				if time.Since(*hb) > 5*time.Second {
					now := time.Now()
					ls.l.LastHeartbeat.Store(&now)
				}
			}
		}
		s.listenersMu.RUnlock()
	}
	log.Printf("Studio %s: distributor stopped", s.ID)
}

// HandleLiveIngest is called when a live encoder (e.g., BUTT) streams audio to the server.
// Only one live stream at a time is supported per studio.
func (s *Studio) HandleLiveIngestV1(w http.ResponseWriter, r *http.Request) {
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
			s.push(chunk)
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

	id := uuid.NewString()
	ip := netutil.ExtractClientIp(r)
	now := time.Now()
	userAgent := r.Header.Get("User-Agent")
	l := &listeners.Listener{
		ID:          id,
		StudioId:    s.ID,
		RemoteIP:    ip,
		UserAgent:   userAgent,
		ClientType:  netutil.ClassifyUserAgent(userAgent),
		ConnectedAt: now,
	}
	l.LastHeartbeat.Store(&now)
	s.listenersStore.Add(l)

	// Enrich asynchronously (non-blocking)
	go s.geoResolver.Enrich(l)

	sl := &streamListener{
		l:  l,
		ch: make(chan []byte, 2048),
	}
	s.listenersMu.Lock()
	s.streamListeners[sl] = struct{}{}
	total := len(s.streamListeners)
	s.listenersMu.Unlock()
	log.Printf("Studio %s: new listener (total=%d)", s.ID, total)

	defer func() {
		l.MarkDisconnected()
		s.listenersMu.Lock()
		delete(s.streamListeners, sl)
		s.listenersMu.Unlock()
		s.listenersStore.Remove(l.ID)
		close(sl.ch)
		log.Printf("Studio %s: listener disconnected", s.ID)
	}()

	for data := range sl.ch {
		if _, err := w.Write(data); err != nil {
			break
		}
		flusher.Flush()
	}
}

// Example status endpoint (extend with richer JSON / metrics).
func (s *Studio) HandleStatus(w http.ResponseWriter, r *http.Request) {
	// Simple plain text (replace with JSON if you add a JSON encoder)
	s.listenersMu.RLock()
	listenerCount := len(s.streamListeners)
	s.listenersMu.RUnlock()

	live := s.liveActive.Load()

	sStatus := studioStatus{
		Studio:         s.ID,
		IsLive:         live,
		ListenersCount: listenerCount,
	}

	netutil.ServerResponse(w, 200, "Success", sStatus)
}

func (s *Studio) HandleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap := s.Snapshot()

	netutil.ServerResponse(w, 200, "Success", snap)
}

func (s *Studio) HandleNowPlaying(w http.ResponseWriter, r *http.Request) {
	var resp NowPlayingResponse
	if s.autoDJ != nil {
		cur, next, started, ok := s.autoDJ.NowPlaying()
		if ok {
			resp = NowPlayingResponse{
				StudioID:   s.ID,
				Current:    cur.File,
				Next:       next.File,
				StartedAt:  started,
				ElapsedSec: time.Since(started).Seconds(),
			}
		}
	}
	if resp.Current == "" {
		resp.StudioID = s.ID
	}
	netutil.ServerResponse(w, 200, "Success", resp)
}

func (s *Studio) HandleSkip(w http.ResponseWriter, r *http.Request) {
	if s.autoDJ == nil {
		netutil.ServerResponse(w, 400, "AutoDJ not active", nil)
		return
	}
	s.autoDJ.Skip()
	netutil.ServerResponse(w, 200, "Skip request", nil)
}
