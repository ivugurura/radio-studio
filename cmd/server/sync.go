package main

import (
	"crypto/subtle"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ivugurura/radio-studio/config"
	"github.com/ivugurura/radio-studio/internal/netutil"
	"github.com/ivugurura/radio-studio/internal/stream"
)

// studioSyncer keeps the registered studios in step with the backend. It
// re-checks on a timer and whenever the backend asks (POST /internal/sync,
// sent after a studio is created, activated or deactivated). Every check runs
// in one goroutine, so two checks never register or remove studios at once.
type studioSyncer struct {
	interval time.Duration
	// wake holds at most one pending request: requests that arrive while a
	// check is queued or running merge into the next check, which reads the
	// latest state anyway.
	wake chan struct{}
	sync func()
}

func newStudioSyncer(interval time.Duration, sync func()) *studioSyncer {
	return &studioSyncer{interval: interval, wake: make(chan struct{}, 1), sync: sync}
}

// run checks on every tick and every wake-up until stop is closed.
func (s *studioSyncer) run(stop <-chan struct{}) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-s.wake:
		case <-stop:
			return
		}
		s.sync()
	}
}

// requestSync schedules a check soon without waiting for it. It reports
// whether this call queued a new check (false: one was already pending).
func (s *studioSyncer) requestSync() bool {
	select {
	case s.wake <- struct{}{}:
		return true
	default:
		return false
	}
}

// handleSyncRequest serves POST /internal/sync for the backend. It only
// queues a check and answers 202 at once: the check reads the active studios
// from the backend itself, so the request carries no state and a lost or
// repeated request can't leave studios out of step.
func (s *studioSyncer) handleSyncRequest(apiKey string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			netutil.ServerResponse(w, http.StatusMethodNotAllowed, "Method not allowed", nil)
			return
		}
		if !hasBackendToken(r, apiKey) {
			netutil.ServerResponse(w, http.StatusUnauthorized, "Not authorized", nil)
			return
		}
		queued := s.requestSync()
		netutil.ServerResponse(w, http.StatusAccepted, "Studio sync scheduled", map[string]bool{"queued": queued})
	}
}

// hasBackendToken reports whether r carries the backend's
// `Authorization: Bearer <BACKEND_API_KEY>`. An unset key accepts nothing.
func hasBackendToken(r *http.Request, apiKey string) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && apiKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) == 1
}

// reconcileStudios registers studios the backend lists as active and closes
// the ones it no longer lists, disconnecting their listeners. A failed fetch
// changes nothing, and an empty list removes nothing, so a backend hiccup
// never takes stations off air.
func reconcileStudios(manager *stream.Manager, cfg *config.Config) {
	slugs, err := fetchStudioSlugs(cfg)
	if err != nil {
		log.Printf("studios: sync failed: %v", err)
		return
	}
	if len(slugs) == 0 {
		log.Printf("studios: backend listed no active studios; keeping %v", manager.ListStudios())
		return
	}
	for _, slug := range slugs {
		registerStudio(manager, cfg, slug)
	}
	if removed := manager.RemoveStudiosExcept(slugs); len(removed) > 0 {
		log.Printf("studios: stopped %v (no longer active in the backend)", removed)
	}
}
