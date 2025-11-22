package stream

import (
	"context"
	"sync"
	"time"

	"github.com/ivugurura/radio-studio/internal/analytics"
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
		// peak
		if active > bkt.ActivePeak {
			bkt.ActivePeak = active
		}
		// accrue listener-minutes proportionally to sampling period (we'll add per flush)
		// the caller will add ListenerMinutes outside with actual elapsed minutes
		// merge countries
		for c, n := range countries {
			bkt.Countries[c] += n
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
	minutes := int(delta.Minutes() + 0.5) //round to nearest minute
	if minutes <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, mm := range b.data {
		for _, bkt := range mm {
			if key == "MINUTE" {
				bkt.ListenerMinutes += minutes * active
			} else if key == "FIVE_MIN" {
				bkt.ListenerMinutes += minutes * active
			} else if key == "HOUR" {
				bkt.ListenerMinutes += minutes * active
			}
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

		for {
			select {
			case <-tick.C:
			case <-stop:
				return
			}

			now := time.Now().UTC()
			active, countries, sessions := s.collectSessions()
			// add a sample to peak/countries, and accrue listener-minutes since last flush
			bk.addSample(now, active, countries)
			bk.accrueListenerMinutes(now.Sub(last), active)
			last = now

			// Build batch
			batch := analytics.IngestListenerBatch{
				StudioID: s.ID,
				Sessions: sessions,
				Buckets:  bk.drainReady(now.Add(-1 * time.Second)),
			}

			// send but don't block streaming on errors
			_ = client.SendListenerBatch(context.Background(), batch)
		}
	}()

	return stop
}

// collectSessions reads current and recently disconnected listeners into DTOs and aggregates counts
func (s *Studio) collectSessions() (active int, countries map[string]int, sessions []analytics.ListenerSession) {
	countries = map[string]int{}

	s.listenersMu.RLock()
	defer s.listenersMu.RUnlock()

	for sl := range s.streamListeners {
		l := sl.l
		// aggregate
		if l.DisconnectedAt.Load() == nil {
			active++
		}
		if l.Country != "" {
			countries[l.Country]++
		}

		session := analytics.ListenerSession{
			ID:         l.ID,
			StartedAt:  l.ConnectedAt,
			IPHash:     l.IPHash,
			UserAgent:  l.UserAgent,
			ClientType: l.ClientType,
			Country:    l.Country,
			Region:     l.Region,
			City:       l.City,
			Lat:        l.Lat,
			Lon:        l.Lon,
			TotalBytes: l.ByteSent.Load(),
		}
		if t := l.DisconnectedAt.Load(); t != nil {
			session.EndedAt = t
		}
		sessions = append(sessions, session)
	}
	return
}
