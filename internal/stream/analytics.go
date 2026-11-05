package stream

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/ivugurura/radio-studio/internal/analytics"
	"github.com/ivugurura/radio-studio/internal/listeners"
)

type bucketState struct {
	mu sync.Mutex
	// keyed by interval ("MINUTE","FIVE_MIN", "HOUR") and bucketStart
	data map[string]map[time.Time]*analytics.ListenerBucket
}

func newBucketState() *bucketState {
	return &bucketState{
		data: map[string]map[time.Time]*analytics.ListenerBucket{
			"MINUTE":   {},
			"FIVE_MIN": {},
			"HOUR":     {},
		},
	}
}

func trunc(t time.Time, d time.Duration) time.Time {
	return t.Truncate(d).UTC()
}

func (b *bucketState) addSample(now time.Time, active int, countries map[string]int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	type def struct {
		key string
		dur time.Duration
	}
	defs := []def{
		{"MINUTE", time.Minute},
		{"FIVE_MIN", 5 * time.Minute},
		{"HOUR", time.Hour},
	}
	for _, v := range defs {
		start := trunc(now, v.dur)
		m, ok := b.data[v.key]
		if !ok {
			m = map[time.Time]*analytics.ListenerBucket{}
			b.data[v.key] = m
		}
		bkt, ok := m[start]
		if !ok {
			bkt = &analytics.ListenerBucket{
				Interval:    v.key,
				BucketStart: start,
				Countries:   map[string]int{},
			}
			m[start] = bkt
		}
		if active > bkt.ActivePeak {
			bkt.ActivePeak = active
		}
		// ListenerMinutes are not accrued here; accrueListenerMinutes adds them
		// separately using the actual elapsed time since the last flush.
		for c, n := range countries {
			if n > bkt.Countries[c] {
				bkt.Countries[c] = n
			}
		}
	}
}

func (b *bucketState) drainReady(cutoff time.Time) []analytics.ListenerBucket {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []analytics.ListenerBucket
	for key, mm := range b.data {
		for start, bkt := range mm {
			var dur time.Duration
			switch key {
			case "MINUTE":
				dur = time.Minute
			case "FIVE_MIN":
				dur = 5 * time.Minute
			case "HOUR":
				dur = time.Hour
			}
			if start.Add(dur).Before(cutoff) || start.Add(dur).Equal(cutoff) {
				bkt.ListenerMinutes = int(bkt.ListenerSeconds/60 + 0.5)
				out = append(out, *bkt)
				delete(mm, start)
			}
		}
	}
	return out
}

func (b *bucketState) accrueListenerMinutes(delta time.Duration, active int) {
	if active <= 0 || delta <= 0 {
		return
	}
	secs := delta.Seconds() * float64(active)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, mm := range b.data {
		for _, bkt := range mm {
			bkt.ListenerSeconds += secs
		}
	}
}

// StartAnalytics launches a goroutine that periodically flushes listener sessions and buckets to the backend
func (s *Studio) StartAnalytics(ingestURL, apiKey string, flushEvery time.Duration) (stop chan struct{}) {
	if ingestURL == "" || flushEvery <= 0 {
		return nil
	}
	client := analytics.NewClient(ingestURL, apiKey)
	stop = make(chan struct{})
	bk := newBucketState()

	go func() {
		defer close(stop)
		tick := time.NewTicker(flushEvery)
		defer tick.Stop()

		last := time.Now().UTC()

		const (
			listenerResendDeltaBytes   = 1 << 20 // ~64s of audio at 128 kbps
			listenerFullReconcileEvery = 12      // at 15s flush => every ~3 min
		)
		sentBytes := map[string]int64{}
		flushN := 0

		for {
			select {
			case <-tick.C:
			case <-stop:
				return
			}

			flushN++
			fullReconcile := flushN%listenerFullReconcileEvery == 1

			now := time.Now().UTC()
			active, countries, sessions, ended := s.collectSessions()
			bk.addSample(now, active, countries)
			bk.accrueListenerMinutes(now.Sub(last), active)
			last = now

			toSend := make([]analytics.ListenerSession, 0, len(sessions))
			present := make(map[string]struct{}, len(sessions))
			connectedIDs := make([]string, 0, len(sessions))
			for _, sess := range sessions {
				present[sess.ID] = struct{}{}
				if sess.EndedAt == nil {
					connectedIDs = append(connectedIDs, sess.ID)
				}
				prev, known := sentBytes[sess.ID]
				if fullReconcile || !known || sess.EndedAt != nil ||
					sess.TotalBytes-prev >= listenerResendDeltaBytes {
					toSend = append(toSend, sess)
				}
			}

			buckets := bk.drainReady(now.Add(-1 * time.Second))
			if len(toSend) == 0 && len(buckets) == 0 && len(connectedIDs) == 0 {
				continue
			}

			batch := analytics.IngestListenerBatch{
				StudioID:            s.ID,
				Sessions:            toSend,
				Buckets:             buckets,
				ConnectedSessionIDs: connectedIDs,
			}

			// send but don't block streaming on errors
			if err := client.SendListenerBatch(context.Background(), batch); err != nil {
				continue // keep sentBytes and s.ended as-is so these sessions retry next flush
			}

			s.ended.ack(ended)

			for _, sess := range toSend {
				sentBytes[sess.ID] = sess.TotalBytes
			}
			for id := range sentBytes {
				if _, ok := present[id]; !ok {
					delete(sentBytes, id)
				}
			}
		}
	}()

	return stop
}

// collectSessions turns the open connections into one DTO per listener session
// and counts the active sessions and their countries. A session can have
// several connections (two tabs, or a reconnect before the old socket timed
// out); it is counted once and its bytes are summed.
//
// It also adds the closed sessions waiting in s.ended, and returns those
// entries as consumed so the caller can ack them once the batch is delivered.
// A queued session that has reopened (a reconnect within the merge window) is
// reported as open instead.
func (s *Studio) collectSessions() (active int, countries map[string]int, sessions []analytics.ListenerSession, consumed []*endedSession) {
	countries = map[string]int{}
	byID := map[string]*analytics.ListenerSession{}
	open := map[string]bool{}

	s.listenersMu.RLock()
	for sl := range s.streamListeners {
		l := sl.l
		session, ok := byID[l.ID]
		if !ok {
			session = &analytics.ListenerSession{
				ID:          l.ID,
				ListenerKey: sl.sess.reportKey,
				StartedAt:   sl.sess.startedAt,
				TotalBytes:  sl.sess.closedBytes.Load(),
			}
			byID[l.ID] = session
			fillSessionDetails(session, l)
		}
		session.TotalBytes += l.ByteSent.Load()

		if t := l.DisconnectedAt.Load(); t == nil {
			open[l.ID] = true
		} else if session.EndedAt == nil || t.After(*session.EndedAt) {
			session.EndedAt = t
		}
	}
	// Read the queue under listenersMu: a connection is removed from
	// streamListeners before its session is queued, so a queued entry for a
	// session seen open above comes from an earlier close, and the open state
	// is the current one. Reading after unlocking could report one session
	// twice, which the backend's bulk upsert rejects.
	consumed = s.ended.pending()
	s.listenersMu.RUnlock()

	sessions = make([]analytics.ListenerSession, 0, len(byID)+len(consumed))
	for id, session := range byID {
		if open[id] {
			session.EndedAt = nil
			active++
			if session.Country != "" {
				countries[session.Country]++
			}
		}
		sessions = append(sessions, *session)
	}
	for _, e := range consumed {
		if _, reopened := byID[e.dto.ID]; !reopened {
			sessions = append(sessions, e.dto)
		}
	}

	// Emit sessions in a stable order (map iteration above is randomized). The
	// backend ingest locks listener_sessions rows in payload order; a consistent
	// order across overlapping flushes avoids lock-order deadlocks. Sort after
	// releasing the read lock so it does not compete with distribute() during a
	// connect/disconnect storm.
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return
}

func fillSessionDetails(session *analytics.ListenerSession, l *listeners.Listener) {
	session.IPHash = l.IPHash
	session.UserAgent = l.UserAgent
	session.ClientType = l.ClientType
	session.Country = l.Country
	session.Region = l.Region
	session.City = l.City
	session.Lat = l.Lat
	session.Lon = l.Lon
}
