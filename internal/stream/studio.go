package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	sess      *listenerSession
	ch        chan []byte
	evicted   chan struct{}
	evictOnce sync.Once

	// resyncs holds the times this listener's buffer overflowed. Only the
	// distribute goroutine touches it.
	resyncs []time.Time
}

func newStreamListener(l *listeners.Listener, sess *listenerSession, capacity int) *streamListener {
	return &streamListener{
		l:       l,
		sess:    sess,
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

// connectBurstSeconds of recent audio are sent to each new listener up front,
// so its player starts with a buffer to ride out network jitter instead of
// receiving only real-time audio and stalling on the first hiccup. It is sized
// for mobile browsers, which play the stream through their native player
// (<audio src>): those wait for several seconds of buffered audio before they
// start, and anything the burst doesn't cover arrives only at real time, which
// the user feels as a slow play button. The cost is that a listener hears
// audio about this far behind the live edge.
const connectBurstSeconds = 12

// listenerHeadroomSeconds of free queue space each listener gets on top of the
// connect burst, to absorb network jitter before it has to be resynced.
const listenerHeadroomSeconds = 8

func bytesPerSecond(bitrateKbps int) int {
	if bitrateKbps <= 0 {
		bitrateKbps = 128
	}
	return bitrateKbps * 1000 / 8
}

// queueCapacity returns the number of audioChunkSize chunks needed to retain a
// short amount of audio at the studio's configured bitrate.
func queueCapacity(bitrateKbps, seconds int) int {
	bytes := bytesPerSecond(bitrateKbps) * seconds
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

	// Station details sent to listeners as icy-* headers.
	stationMu sync.RWMutex
	station   stationInfo

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

	// sessions groups connections into listener sessions, so a reconnect is
	// not counted as a new listener (sessions.go).
	sessions *sessionRegistry
	// ended holds closed sessions until analytics delivers their ended_at.
	ended *endedQueue

	// burst holds the most recent ~burstLimit bytes of the feed for new
	// listeners. Guarded by listenersMu.
	burst      [][]byte
	burstBytes int
	burstLimit int

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
		sessions:         newSessionRegistry(defaultSessionMergeWindow, listenerKeyHasher(geoR)),
		ended:            newEndedQueue(),
		burstLimit:       bytesPerSecond(brKbps) * connectBurstSeconds,
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
// It reports whether they changed (including the first load).
func (s *Studio) SetCredentials(user, password string) bool {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	changed := s.user != user || s.password != password
	s.user = user
	s.password = password
	return changed
}

func (s *Studio) credentials() (string, string) {
	s.credMu.RLock()
	defer s.credMu.RUnlock()
	return s.user, s.password
}

type stationInfo struct {
	name, description, genre, url string
}

// maxStationDescriptionRunes keeps icy-description to a header-sized blurb.
const maxStationDescriptionRunes = 256

// cleanHeaderValue collapses whitespace (including newlines, which are not
// allowed in a header value) and truncates to maxRunes when maxRunes > 0.
func cleanHeaderValue(v string, maxRunes int) string {
	v = strings.Join(strings.Fields(v), " ")
	if r := []rune(v); maxRunes > 0 && len(r) > maxRunes {
		v = strings.TrimSpace(string(r[:maxRunes-1])) + "…"
	}
	return v
}

// SetStationName updates the station name/description from the backend.
// Safe to call repeatedly (e.g. from the streaming-config refresh).
func (s *Studio) SetStationName(name, description string) {
	s.stationMu.Lock()
	s.station.name = cleanHeaderValue(name, 0)
	s.station.description = cleanHeaderValue(description, maxStationDescriptionRunes)
	s.stationMu.Unlock()
}

// SetStationLinks sets the station genre and website from config.
func (s *Studio) SetStationLinks(genre, url string) {
	s.stationMu.Lock()
	s.station.genre = cleanHeaderValue(genre, 0)
	s.station.url = cleanHeaderValue(url, 0)
	s.stationMu.Unlock()
}

// writeStationHeaders adds the Icecast/SHOUTcast headers that players such as
// VLC, car radios and directory apps show as the station's name and format.
func (s *Studio) writeStationHeaders(h http.Header) {
	s.stationMu.RLock()
	info := s.station
	s.stationMu.RUnlock()

	for key, value := range map[string]string{
		"icy-name":        info.name,
		"icy-description": info.description,
		"icy-genre":       info.genre,
		"icy-url":         info.url,
	} {
		if value != "" {
			h.Set(key, value)
		}
	}
	h.Set("icy-br", strconv.Itoa(s.bitrateKbps))
	h.Set("icy-sr", strconv.Itoa(s.srHz))
	h.Set("ice-audio-info", fmt.Sprintf("ice-samplerate=%d;ice-bitrate=%d;ice-channels=%d", s.srHz, s.bitrateKbps, s.ch))
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
			s.sessions.prune(time.Now())
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
	var liveFrameReceived bool
	var autodjChunk, liveChunk []byte

	for {
		select {
		case <-s.stop:
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

// removeListener drops a closed connection and moves its byte count into its
// session. Both happen under listenersMu, so collectSessions never counts the
// bytes twice or misses them.
func (s *Studio) removeListener(sl *streamListener) {
	s.listenersMu.Lock()
	delete(s.streamListeners, sl)
	sl.sess.closedBytes.Add(sl.l.ByteSent.Load())
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
	seen := make(map[string]struct{}, len(active))
	for _, l := range active {
		totalBytes += l.ByteSent.Load()
		// Several connections can share one session; count the listener once.
		if _, dup := seen[l.ID]; dup {
			continue
		}
		seen[l.ID] = struct{}{}
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
	var evictees []*streamListener
	for data := range s.feed {
		now := time.Now()
		// Write lock: the burst update and the fan-out must be atomic with
		// respect to HandleListen, or a new listener could get a chunk twice.
		// offer never blocks, so this stays short.
		s.listenersMu.Lock()
		s.rememberBurst(data)
		for ls := range s.streamListeners {
			if ls.offer(data, now) == offerEvict {
				evictees = append(evictees, ls)
			}
		}
		for _, ls := range evictees {
			delete(s.streamListeners, ls)
		}
		s.listenersMu.Unlock()

		if len(evictees) > 0 {
			for _, ls := range evictees {
				ls.evict()
				log.Printf("Studio %s: evicted slow listener id=%s (%d resyncs in %s)", s.ID, ls.l.ID, len(ls.resyncs), listenerResyncWindow)
			}
			clear(evictees)
			evictees = evictees[:0]
		}
	}
}

// rememberBurst appends data to the connect burst, dropping the oldest chunks
// beyond burstLimit. Chunks are shared, never mutated. Caller holds listenersMu.
func (s *Studio) rememberBurst(data []byte) {
	s.burst = append(s.burst, data)
	s.burstBytes += len(data)
	for len(s.burst) > 1 && s.burstBytes-len(s.burst[0]) >= s.burstLimit {
		s.burstBytes -= len(s.burst[0])
		s.burst[0] = nil
		s.burst = s.burst[1:]
	}
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
	s.writeStationHeaders(w.Header())
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

	ip := netutil.ExtractClientIp(r)
	now := time.Now()
	userAgent := r.Header.Get("User-Agent")
	// The listener's ID is its session ID: a reconnect within the merge window
	// keeps the ID, so the backend sees one session instead of a new listen.
	sess, resumed := s.sessions.attach(s.listenerKey(r, ip, userAgent), now)
	id := sess.id
	l := &listeners.Listener{
		ID:          id,
		StudioId:    s.ID,
		RemoteIP:    ip,
		UserAgent:   userAgent,
		ClientType:  netutil.ClassifyUserAgent(userAgent),
		ConnectedAt: now,
	}
	l.LastHeartbeat.Store(&now)
	// Enrich before the listener is published: the snapshot and analytics
	// goroutines read the geo fields without a lock, so they must be final by
	// the time listenersStore and streamListeners can hand l to them. A local
	// GeoIP lookup takes microseconds.
	s.geoResolver.Enrich(l)
	s.listenersStore.Add(l)

	s.listenersMu.Lock()
	// Prefill with the connect burst. Under the lock, so distribute can't
	// slip a chunk in between. The queue is sized from the burst's chunk
	// count (small live-ingest chunks can outnumber a byte-based estimate),
	// so the whole burst fits and the headroom stays free.
	burst := s.burst
	sl := newStreamListener(l, sess, queueCapacity(s.bitrateKbps, listenerHeadroomSeconds)+len(burst))
	for _, chunk := range burst {
		sl.ch <- chunk
	}
	s.streamListeners[sl] = struct{}{}
	s.listenersMu.Unlock()

	reason := "unknown"
	defer func() {
		l.MarkDisconnected()
		s.removeListener(sl)
		s.listenersStore.Remove(l)
		if endedAt := time.Now(); s.sessions.detach(sess, endedAt) {
			s.ended.add(endedSessionDTO(sess, l, endedAt))
		}
		log.Printf("Studio %s: listener disconnected id=%s client=%s reason=%s after=%s sent=%dB",
			s.ID, id, l.ClientType, reason, time.Since(now).Round(time.Second), l.ByteSent.Load())
	}()
	if resumed {
		log.Printf("Studio %s: listener reconnected, continuing session id=%s", s.ID, id)
	}

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
				reason = disconnectReason("write", err)
				return
			}
			if err := flush(); err != nil {
				reason = disconnectReason("flush", err)
				return
			}
		case <-sl.evicted:
			reason = "evicted"
			return
		case <-ctx.Done():
			// The client (or the proxy in front of us) closed the connection.
			reason = "client-closed"
			return
		case <-s.stop:
			reason = "studio-stopped"
			return
		}
	}
}

// disconnectReason labels a failed write/flush to a listener. A deadline means
// the client stopped reading for writeTimeout (e.g. a sleeping phone).
func disconnectReason(op string, err error) string {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return op + "-timeout"
	}
	return op + "-error: " + err.Error()
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
	sStatus := studioStatus{
		Studio:         s.ID,
		IsLive:         s.liveActive.Load(),
		ListenersCount: s.sessions.activeCount(),
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
