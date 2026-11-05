package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/ivugurura/radio-studio/internal/analytics"
	"github.com/ivugurura/radio-studio/internal/geo"
	"github.com/ivugurura/radio-studio/internal/listeners"
)

// defaultSessionMergeWindow is how long after a listener's last connection
// closes that a reconnect from the same listener continues the same session
// instead of starting a new one. It covers network drops and Wi-Fi/mobile
// handovers, which would otherwise each count as a new "listen".
const defaultSessionMergeWindow = 60 * time.Second

// Listener key prefixes. A "lid" key comes from the stable per-browser id the
// web player sends; an "fp" key is a salted IP + User-Agent fingerprint used
// for players that send no id (VLC, Winamp, apps).
const (
	keyPrefixLID         = "lid:"
	keyPrefixFingerprint = "fp:"
)

// listenerSession is one continuous period of listening by one listener. It can
// span several connections: overlapping ones (two tabs, or a stale socket that
// has not timed out yet while the player already reconnected) and sequential
// ones (a reconnect inside the merge window).
type listenerSession struct {
	id        string
	key       string
	startedAt time.Time
	// reportKey is the hashed listener key sent to the backend.
	reportKey string

	// closedBytes counts the bytes sent on this session's connections that
	// have closed. Bytes of open connections are read from their listeners.
	closedBytes atomic.Int64

	// Guarded by sessionRegistry.mu.
	conns   int
	endedAt time.Time
}

// sessionRegistry maps listener keys to their current session, so reconnects
// can be recognised, and counts sessions with at least one open connection.
type sessionRegistry struct {
	mu          sync.Mutex
	byKey       map[string]*listenerSession
	mergeWindow time.Duration
	hashKey     func(string) string

	// active is the number of sessions with at least one open connection.
	active atomic.Int64
}

// newSessionRegistry builds a registry; hashKey turns a listener key into the
// value reported to the backend (nil reports a plain SHA-256).
func newSessionRegistry(mergeWindow time.Duration, hashKey func(string) string) *sessionRegistry {
	if hashKey == nil {
		hashKey = func(key string) string {
			sum := sha256.Sum256([]byte(key))
			return hex.EncodeToString(sum[:])
		}
	}
	return &sessionRegistry{
		byKey:       make(map[string]*listenerSession),
		mergeWindow: mergeWindow,
		hashKey:     hashKey,
	}
}

// attach returns the session a new connection belongs to. It continues the
// key's session if that listener is still connected or left within the merge
// window, and otherwise starts a new one. An empty key always starts a new
// session.
//
// A fingerprint key only continues a session that has no open connection:
// many people can share one IP behind carrier-grade NAT, so two simultaneous
// connections with the same fingerprint are treated as two listeners.
func (r *sessionRegistry) attach(key string, now time.Time) (sess *listenerSession, resumed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if prev, ok := r.byKey[key]; ok && key != "" {
		concurrentOK := prev.conns > 0 && !isFingerprintKey(key)
		withinWindow := prev.conns == 0 && now.Sub(prev.endedAt) <= r.mergeWindow
		if concurrentOK || withinWindow {
			if prev.conns == 0 {
				r.active.Add(1)
			}
			prev.conns++
			return prev, true
		}
	}

	id := uuid.NewString()
	reportKey := key
	if reportKey == "" {
		// No identity: the session is its own listener.
		reportKey = "sid:" + id
	}
	sess = &listenerSession{
		id:        id,
		key:       key,
		startedAt: now,
		reportKey: r.hashKey(reportKey),
		conns:     1,
	}
	if key != "" {
		r.byKey[key] = sess
	}
	r.active.Add(1)
	return sess, false
}

// detach records that one of the session's connections closed. It reports
// whether that was the session's last open connection.
func (r *sessionRegistry) detach(sess *listenerSession, now time.Time) (closed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess.conns--
	if sess.conns == 0 {
		sess.endedAt = now
		r.active.Add(-1)
		return true
	}
	return false
}

// prune forgets sessions that have been closed for longer than the merge
// window; a reconnect after that starts a new session anyway.
func (r *sessionRegistry) prune(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, sess := range r.byKey {
		if sess.conns == 0 && now.Sub(sess.endedAt) > r.mergeWindow {
			delete(r.byKey, key)
		}
	}
}

func (r *sessionRegistry) setMergeWindow(d time.Duration) {
	r.mu.Lock()
	r.mergeWindow = d
	r.mu.Unlock()
}

func (r *sessionRegistry) activeCount() int {
	return int(r.active.Load())
}

// maxPendingEnded caps how many closed sessions wait for delivery while the
// backend is unreachable.
const maxPendingEnded = 10000

// endedSession is a closed session whose ended_at the backend has not
// received yet.
type endedSession struct {
	dto analytics.ListenerSession
}

// endedQueue holds closed sessions until a flush delivers them. Without it a
// session vanishes from the payload the moment its last connection closes, and
// the backend never learns that it ended.
type endedQueue struct {
	mu   sync.Mutex
	byID map[string]*endedSession
}

func newEndedQueue() *endedQueue {
	return &endedQueue{byID: make(map[string]*endedSession)}
}

func (q *endedQueue) add(dto analytics.ListenerSession) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.byID[dto.ID]; !ok && len(q.byID) >= maxPendingEnded {
		// The backend stays down; its last_seen expiry ends the session instead.
		return
	}
	q.byID[dto.ID] = &endedSession{dto: dto}
}

func (q *endedQueue) pending() []*endedSession {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*endedSession, 0, len(q.byID))
	for _, e := range q.byID {
		out = append(out, e)
	}
	return out
}

// ack drops delivered entries. An entry replaced while the flush was in
// flight (the session reopened and closed again) stays queued.
func (q *endedQueue) ack(entries []*endedSession) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range entries {
		if q.byID[e.dto.ID] == e {
			delete(q.byID, e.dto.ID)
		}
	}
}

func isFingerprintKey(key string) bool {
	return strings.HasPrefix(key, keyPrefixFingerprint)
}

// listenerKey identifies the listener behind a /listen request: the "lid"
// query parameter when it is a valid UUID, else a salted hash of the client IP
// and User-Agent, else "" (no identity, so no session merging).
func (s *Studio) listenerKey(r *http.Request, ip net.IP, userAgent string) string {
	if lid, err := uuid.Parse(r.URL.Query().Get("lid")); err == nil && lid != uuid.Nil {
		return keyPrefixLID + lid.String()
	}
	if ip == nil || s.geoResolver == nil {
		return ""
	}
	return keyPrefixFingerprint + s.geoResolver.Fingerprint(ip, userAgent)
}

// listenerKeyHasher salts listener keys with the IP-hash salt when a resolver
// is configured.
func listenerKeyHasher(r *geo.Resolver) func(string) string {
	if r == nil {
		return nil
	}
	return r.HashKey
}

// endedSessionDTO describes a session whose last connection (l) just closed.
// By then removeListener has moved every connection's bytes into closedBytes.
func endedSessionDTO(sess *listenerSession, l *listeners.Listener, endedAt time.Time) analytics.ListenerSession {
	dto := analytics.ListenerSession{
		ID:          sess.id,
		ListenerKey: sess.reportKey,
		StartedAt:   sess.startedAt,
		EndedAt:     &endedAt,
		TotalBytes:  sess.closedBytes.Load(),
	}
	fillSessionDetails(&dto, l)
	return dto
}

// SetSessionMergeWindow sets how long a disconnected listener has to reconnect
// and continue the same session. Non-positive values are ignored.
func (s *Studio) SetSessionMergeWindow(d time.Duration) {
	if d <= 0 {
		return
	}
	s.sessions.setMergeWindow(d)
}
