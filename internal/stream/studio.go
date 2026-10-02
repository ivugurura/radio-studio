package stream

import (
	"context"
	"errors"
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
	l         *listeners.Listener
	ch        chan []byte
	evicted   chan struct{}
	evictOnce sync.Once

	// resyncs holds the times this listener's buffer overflowed. Only the
	// distribute goroutine touches it.
	resyncs []time.Time
}

func newStreamListener(l *listeners.Listener, capacity int) *streamListener {
	return &streamListener{
		l:       l,
		ch:      make(chan []byte, capacity),
		evicted: make(chan struct{}),
	}
}

func (sl *streamListener) evict() {
	sl.evictOnce.Do(func() { close(sl.evicted) })
}

type offerResult int

const (
	offerOK offerResult = iota
	offerResynced
	offerEvict
)

const (
	// defaultListenerWriteTimeout bounds a single write to a listener's socket.
	// A client that accepts nothing for this long is treated as gone.
	defaultListenerWriteTimeout = 10 * time.Second

	// A listener whose buffer overflows is skipped forward to the live edge
	// (a "resync"). Too many resyncs in the window means it can't keep up.
	maxListenerResyncs   = 5
	listenerResyncWindow = 60 * time.Second
)

// offer hands a chunk to the listener without ever blocking the caller. When
// the buffer is full it discards the whole backlog and resumes at the live
// edge: one glitch instead of scattered gaps, and the listener's latency drops
// back to near zero. Only the distribute goroutine may call it.
func (sl *streamListener) offer(data []byte, now time.Time) offerResult {
	select {
	case sl.ch <- data:
		return offerOK
	default:
	}

	for drained := false; !drained; {
		select {
		case <-sl.ch:
		default:
			drained = true
		}
	}

	cutoff := now.Add(-listenerResyncWindow)
	kept := sl.resyncs[:0]
	for _, t := range sl.resyncs {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	sl.resyncs = append(kept, now)
	if len(sl.resyncs) >= maxListenerResyncs {
		return offerEvict
	}

	select {
	case sl.ch <- data:
	default:
	}
	return offerResynced
}

const audioChunkSize = 4096

// queueCapacity returns the number of audioChunkSize chunks needed to retain a
// short amount of audio at the studio's configured bitrate.
func queueCapacity(bitrateKbps, seconds int) int {
	if bitrateKbps <= 0 {
		bitrateKbps = 128
	}
	bytes := bitrateKbps * 1000 / 8 * seconds
	return max(1, (bytes+audioChunkSize-1)/audioChunkSize)
}

// Studio represents a radio studio/channel
type Studio struct {
	ID          string
	audioDir    string
	bitrateKbps int
	srHz        int
	ch          int

	// Live-ingest credentials, fetched from the backend (streamingconfig.go)
	// rather than read from .env.
	credMu   sync.RWMutex
	user     string
	password string

	// Live ingest (if present)
	liveMu     sync.RWMutex
	liveIngest io.ReadCloser
	liveActive atomic.Bool

	liveMetaMu sync.RWMutex
	liveMeta   *LiveMeta

	// Per-source feeds for warm switching
	autodjFeed chan []byte
	liveFeed   chan []byte

	// Central feed: all upstream audio goes here (AutoDJ or live)
	feed chan []byte

	// listeners receives bytes (fan-out)
	listenersMu     sync.RWMutex
	streamListeners map[*streamListener]struct{}
	listenersStore  *listeners.Store
	writeTimeout    time.Duration

	snapshotMu       sync.RWMutex
	lastSnapshot     StudioSnapshot
	snapshotInterval time.Duration
	stop             chan struct{}

	geoResolver  *geo.Resolver
	autoDJ       AutoDJ
	autoDJCancel context.CancelFunc
}

func NewStudio(id string, dir string, brKbps, srHz, ch int, geoR *geo.Resolver, autoDJF AutoDJFactory, snapIn time.Duration) *Studio {
	s := &Studio{
		ID:               id,
		audioDir:         dir,
		bitrateKbps:      brKbps,
		srHz:             srHz,
		ch:               ch,
		autodjFeed:       make(chan []byte, queueCapacity(brKbps, 2)),
		liveFeed:         make(chan []byte, queueCapacity(brKbps, 2)),
		feed:             make(chan []byte, queueCapacity(brKbps, 2)),
		listenersStore:   listeners.NewStore(),
		writeTimeout:     defaultListenerWriteTimeout,
		streamListeners:  make(map[*streamListener]struct{}),
		geoResolver:      geoR,
		snapshotInterval: snapIn,
		stop:             make(chan struct{}),
	}

	go s.distribute()
	go s.switcherLoop()
	if autoDJF != nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.autoDJCancel = cancel
		s.autoDJ = autoDJF(dir, id, brKbps, func(b []byte) {
			// Preserve compressed-byte order; a full queue applies backpressure.
			chunk := make([]byte, len(b))
			copy(chunk, b)
			select {
			case s.autodjFeed <- chunk:
			case <-s.stop:
			}
		})
		go s.autoDJ.Play(ctx)
	}
	go s.snapshotLoop()
	return s
}

// SetCredentials updates the live-ingest Basic Auth credentials. Safe to
// call repeatedly (e.g. from a refresh ticker) while a source is connected.
func (s *Studio) SetCredentials(user, password string) {
	s.credMu.Lock()
	s.user = user
	s.password = password
	s.credMu.Unlock()
}

func (s *Studio) credentials() (string, string) {
	s.credMu.RLock()
	defer s.credMu.RUnlock()
	return s.user, s.password
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

// switcherLoop implements warm switching between AutoDJ and live streams.
// It ensures seamless transitions by:
// - Only switching to live after receiving the first live frame (prevents starvation)
// - Continuing AutoDJ until live frames arrive (prevents silent gaps)
// - Immediately resuming AutoDJ when live disconnects
func (s *Studio) switcherLoop() {
	log.Printf("Studio %s: switcher loop started", s.ID)

	var liveFrameReceived bool
	var autodjChunk, liveChunk []byte

	for {
		select {
		case <-s.stop:
			log.Printf("Studio %s: switcher loop stopped", s.ID)
			return

		case autodjChunk = <-s.autodjFeed:
			if liveFrameReceived && !s.liveActive.Load() {
				log.Printf("Studio %s: live stream ended, resuming AutoDJ", s.ID)
				liveFrameReceived = false
			}

			if !s.liveActive.Load() || !liveFrameReceived {
				s.push(autodjChunk)
			}

		case liveChunk = <-s.liveFeed:
			// Discard buffered live frames once the session ends; forwarding them
			// interleaves stale live bytes with AutoDJ bytes and garbles the stream.
			if !s.liveActive.Load() {
				liveFrameReceived = false // reset so next session gets a clean warm-switch
				continue
			}
			if !liveFrameReceived {
				liveFrameReceived = true
				log.Printf("Studio %s: first live frame received, switching to live stream", s.ID)
			}
			s.push(liveChunk)
		}
	}
}

func (s *Studio) push(data []byte) {
	select {
	case s.feed <- data:
	case <-s.stop:
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
	var evictees []*streamListener
	for data := range s.feed {
		now := time.Now()
		s.listenersMu.RLock()
		for ls := range s.streamListeners {
			if ls.offer(data, now) == offerEvict {
				evictees = append(evictees, ls)
			}
		}
		s.listenersMu.RUnlock()

		if len(evictees) > 0 {
			s.listenersMu.Lock()
			for _, ls := range evictees {
				delete(s.streamListeners, ls)
			}
			s.listenersMu.Unlock()
			for _, ls := range evictees {
				ls.evict()
				log.Printf("Studio %s: evicted slow listener id=%s (%d resyncs in %s)", s.ID, ls.l.ID, len(ls.resyncs), listenerResyncWindow)
			}
			clear(evictees)
			evictees = evictees[:0]
		}
	}
	log.Printf("Studio %s: distributor stopped", s.ID)
}

// HandleListen streams audio (live or AutoDJ) to a listener.
func (s *Studio) HandleListen(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)

	// Every write gets its own deadline: a stalled client must not pin this
	// goroutine, its socket and its listener-store entry forever. The deadline
	// is per write, so the infinite stream itself is never cut off.
	write := func(data []byte) (int, error) {
		if err := rc.SetWriteDeadline(time.Now().Add(s.writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return 0, err
		}
		return w.Write(data)
	}
	flush := func() error {
		if err := rc.SetWriteDeadline(time.Now().Add(s.writeTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		return rc.Flush()
	}

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	// Do NOT set Accept-Ranges: an infinite stream is not seekable.
	// Do NOT manually set Transfer-Encoding; Go will add chunked automatically.
	w.WriteHeader(http.StatusOK)
	if err := flush(); err != nil { // send headers immediately so the client doesn't stall on initial connect
		if errors.Is(err, http.ErrNotSupported) {
			log.Printf("Studio %s: streaming unsupported by response writer", s.ID)
		}
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

	go s.geoResolver.Enrich(l)

	sl := newStreamListener(l, queueCapacity(s.bitrateKbps, 8))
	s.listenersMu.Lock()
	s.streamListeners[sl] = struct{}{}
	total := len(s.streamListeners)
	s.listenersMu.Unlock()
	log.Printf("Studio %s: new listener (total=%d)", s.ID, total)

	defer func() {
		l.MarkDisconnected()
		s.removeListener(sl)
		s.listenersStore.Remove(l.ID)
		log.Printf("Studio %s: listener disconnected", s.ID)
	}()

	ctx := r.Context()
	for {
		select {
		case data := <-sl.ch:
			n, err := write(data)
			if n > 0 {
				l.ByteSent.Add(int64(n))
				touchHeartbeat(l)
			}
			if err != nil {
				return
			}
			if err := flush(); err != nil {
				return
			}
		case <-sl.evicted:
			return
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		}
	}
}

// touchHeartbeat records that bytes reached the client. It is throttled so a
// ~4 chunk/s stream does not allocate a timestamp per write.
func touchHeartbeat(l *listeners.Listener) {
	if hb := l.LastHeartbeat.Load(); hb == nil || time.Since(*hb) > 5*time.Second {
		now := time.Now()
		l.LastHeartbeat.Store(&now)
	}
}

// Example status endpoint (extend with richer JSON / metrics).
func (s *Studio) HandleStatus(w http.ResponseWriter, r *http.Request) {
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
				Current:    cur.Title,
				Next:       next.Title,
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

// HandleSkip skips the track AutoDJ is currently playing.
func (s *Studio) HandleSkip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		netutil.ServerResponse(w, http.StatusMethodNotAllowed, "Method not allowed", nil)
		return
	}
	if s.autoDJ == nil {
		netutil.ServerResponse(w, 400, "AutoDJ not active", nil)
		return
	}
	if s.liveActive.Load() {
		netutil.ServerResponse(w, http.StatusConflict, "Live source is on air", nil)
		return
	}
	s.autoDJ.Skip()
	netutil.ServerResponse(w, 200, "Skip request", nil)
}
