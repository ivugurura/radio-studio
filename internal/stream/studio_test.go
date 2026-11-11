package stream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/ivugurura/radio-studio/internal/analytics"
	"github.com/ivugurura/radio-studio/internal/geo"
)

// fakeAutoDJ pushes a chunk every few milliseconds until its context ends,
// standing in for the real AutoDJ (which needs files or a backend).
type fakeAutoDJ struct{ push func([]byte) }

func (f *fakeAutoDJ) Play(ctx context.Context) {
	chunk := make([]byte, audioChunkSize)
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.push(chunk)
		}
	}
}
func (f *fakeAutoDJ) Skip()        {}
func (f *fakeAutoDJ) ForceReload() {}
func (f *fakeAutoDJ) Stop()        {}
func (f *fakeAutoDJ) NowPlaying() (Track, Track, time.Time, bool) {
	return Track{}, Track{}, time.Time{}, false
}

func fakeAutoDJFactory(_ string, _ string, _ int, push func([]byte)) AutoDJ {
	return &fakeAutoDJ{push: push}
}

func newTestStudio(t *testing.T, id string) *Studio {
	t.Helper()
	geoR := geo.NewResolver("", "test-salt", false)
	return NewStudio(id, t.TempDir(), 128, 48000, 2, geoR, fakeAutoDJFactory, time.Hour)
}

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// openListener connects to the studio's /listen handler and waits for audio.
func openListener(t *testing.T, url string) *http.Response {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("listen: status %d", res.StatusCode)
	}
	if _, err := io.ReadFull(res.Body, make([]byte, audioChunkSize)); err != nil {
		t.Fatalf("listen: no audio: %v", err)
	}
	return res
}

func TestCloseStopsStudioGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()
	s := newTestStudio(t, "zz")
	waitFor(t, time.Second, "audio in the feed", func() bool {
		s.listenersMu.RLock()
		defer s.listenersMu.RUnlock()
		return s.burstBytes > 0
	})

	s.Close()
	s.Close() // idempotent

	waitFor(t, 2*time.Second, "studio goroutines to exit", func() bool {
		return runtime.NumGoroutine() <= baseline
	})
}

func TestCloseDisconnectsListenersAndRefusesNewOnes(t *testing.T) {
	s := newTestStudio(t, "zz")
	srv := httptest.NewServer(http.HandlerFunc(s.HandleListen))
	defer srv.Close()

	res := openListener(t, srv.URL)
	defer res.Body.Close()

	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()

	// The stream must end (EOF or reset) rather than hang.
	ended := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, res.Body)
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("listener stream did not end after Close")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
	if n := s.sessions.activeCount(); n != 0 {
		t.Fatalf("active sessions after Close = %d, want 0", n)
	}

	late, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("late listen: %v", err)
	}
	late.Body.Close()
	if late.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("late listen status = %d, want 503", late.StatusCode)
	}
}

func TestCloseRefusesLiveSource(t *testing.T) {
	s := newTestStudio(t, "zz")
	s.SetCredentials("zz", "secret")
	s.Close()

	req := httptest.NewRequest(http.MethodPut, "/studios/zz/live", nil)
	req.SetBasicAuth("zz", "secret")
	rec := httptest.NewRecorder()
	s.HandleLiveIngest(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("live ingest after Close = %d, want 503", rec.Code)
	}
}

func TestCloseSendsFinalAnalyticsBatch(t *testing.T) {
	batches := make(chan analytics.IngestListenerBatch, 8)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b analytics.IngestListenerBatch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode batch: %v", err)
		}
		batches <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	s := newTestStudio(t, "zz")
	// Never ticks during the test: the only batch is the final one.
	s.StartAnalytics(backend.URL, "", time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(s.HandleListen))
	defer srv.Close()
	res := openListener(t, srv.URL)
	defer res.Body.Close()

	s.Close()

	select {
	case b := <-batches:
		if b.StudioID != "zz" {
			t.Fatalf("batch studio = %q, want zz", b.StudioID)
		}
		if len(b.Sessions) != 1 || b.Sessions[0].EndedAt == nil {
			t.Fatalf("final batch sessions = %+v, want one ended session", b.Sessions)
		}
		if len(b.ConnectedSessionIDs) != 0 {
			t.Fatalf("connected after Close = %v, want none", b.ConnectedSessionIDs)
		}
		if len(b.Buckets) == 0 {
			t.Fatal("final batch has no buckets; unfinished buckets must be sent")
		}
	default:
		t.Fatal("Close returned before the final analytics batch was sent")
	}
}

func TestRemoveStudiosExcept(t *testing.T) {
	geoR := geo.NewResolver("", "test-salt", false)
	m := NewManager(t.TempDir(), geoR, WithAutoDJFactory(fakeAutoDJFactory))
	studios := map[string]*Studio{}
	for _, id := range []string{"rw", "en", "fr"} {
		studios[id] = m.RegisterStudio(id)
	}

	removed := m.RemoveStudiosExcept([]string{"rw"})
	if !slices.Equal(removed, []string{"en", "fr"}) {
		t.Fatalf("removed = %v, want [en fr]", removed)
	}
	if got := m.ListStudios(); !slices.Equal(got, []string{"rw"}) {
		t.Fatalf("registered = %v, want [rw]", got)
	}
	for _, id := range removed {
		if !studios[id].closed.Load() {
			t.Fatalf("studio %s was removed but not closed", id)
		}
	}
	if studios["rw"].closed.Load() {
		t.Fatal("kept studio rw was closed")
	}

	// A studio reactivated in the backend registers again as a fresh studio.
	again := m.RegisterStudio("en")
	if again == studios["en"] || again.closed.Load() {
		t.Fatal("re-registered studio reused the closed one")
	}

	if err := m.RemoveStudio("fr"); err == nil {
		t.Fatal("RemoveStudio of an unregistered studio should fail")
	}
	m.Shutdown()
	if len(m.ListStudios()) != 0 || !studios["rw"].closed.Load() || !again.closed.Load() {
		t.Fatal("Shutdown must remove and close every studio")
	}
}
