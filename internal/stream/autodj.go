package stream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/ivugurura/radio-studio/internal/analytics"
)

type djCommand int

const (
	cmdSkip djCommand = iota
	cmdForceReload
	cmdStop
)

type AutoDJ interface {
	Play(ctx context.Context)
	Skip()
	ForceReload()
	Stop()
	NowPlaying() (Track, Track, time.Time, bool) // current, next, startedAt, ok
}

// default factory (filesystem)
type AutoDJFactory func(dir string, studioID string, bitrate int, push func([]byte)) AutoDJ

type autoDJ struct {
	dir         string
	push        func([]byte)
	bitrateKbps int // configure (e.g. 128)

	ctrl chan djCommand

	playlist PlaylistSource

	// now playing metadata (guarded by playlistState's lock + this lightweight lock)
	nowMu      chan struct{} // simple channel semaphore (size 1)
	current    Track
	next       Track
	startedAt  time.Time
	activeFile string // internal guard to ensure skip t

	fallbackPath string

	client *analytics.Client
	// events queues play events for sendEvents, so backend latency never
	// holds up the audio loop.
	events chan analytics.IngestPlayBatch

	// pace and resumeTried are only touched by the Play goroutine.
	pace        pacer
	resumeTried bool
}

// maxPaceCatchUp bounds how far behind real time the AutoDJ may fall and still
// catch up by sending faster. Past it (e.g. after a long stall) the clock is
// rebased instead, so listeners never get a flood bigger than their queues.
const maxPaceCatchUp = 2 * time.Second

// pacer meters bytes out at the stream bitrate. One pacer spans every track,
// so time lost between tracks is made up by briefly sending faster instead of
// being taken out of every listener's buffer.
type pacer struct {
	bytesPerSec int
	start       time.Time
	sent        int64
}

// wait accounts for n bytes just sent and sleeps until they are due.
func (p *pacer) wait(n int) {
	now := time.Now()
	if p.start.IsZero() {
		p.start = now
	}
	p.sent += int64(n)
	expected := time.Duration(float64(p.sent) / float64(p.bytesPerSec) * float64(time.Second))
	elapsed := now.Sub(p.start)
	if elapsed-expected > maxPaceCatchUp {
		p.start = now.Add(-(expected + maxPaceCatchUp))
		elapsed = expected + maxPaceCatchUp
	}
	if expected > elapsed {
		time.Sleep(expected - elapsed)
	}
}

// queueEvent hands a play event to sendEvents without blocking.
func (a *autoDJ) queueEvent(ev analytics.IngestPlayBatch) {
	select {
	case a.events <- ev:
	default:
		log.Printf("autoDJ: play event queue full, dropping %s", ev.Type)
	}
}

// sendEvents posts queued play events to the backend in order.
func (a *autoDJ) sendEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-a.events:
			if err := a.client.SendPlayerBatch(ctx, []analytics.IngestPlayBatch{ev}); err != nil {
				log.Printf("autoDJ: sending %s event: %v", ev.Type, err)
			}
		}
	}
}

func (a *autoDJ) lock() {
	a.nowMu <- struct{}{}
}

func (a *autoDJ) unlock() {
	<-a.nowMu
}

func (a *autoDJ) Skip() {
	select {
	case a.ctrl <- cmdSkip:
	default:
	}
}

func (a *autoDJ) ForceReload() {
	select {
	case a.ctrl <- cmdForceReload:
	default:
	}
}

func (a *autoDJ) Stop() {
	select {
	case a.ctrl <- cmdStop:
	default:
	}
}

func (a *autoDJ) NowPlaying() (Track, Track, time.Time, bool) {
	a.lock()
	defer a.unlock()
	if a.current.File == "" {
		return Track{}, Track{}, time.Time{}, false
	}
	return a.current, a.next, a.startedAt, true
}

// NewAutoDJWithBackend selects backend-driven playlist if endpoint provided; falls back to filesystem otherwise.
func NewAutoDJ(audioDir string, studioID string, bitrateKbps int, push func([]byte), studioEndpoint string, apiKey string, fallbackFile string) AutoDJ {
	playlistEndpoint := studioEndpoint + "/playlist"
	ingestEndpoint := studioEndpoint + "/play-events"
	return &autoDJ{
		dir:          audioDir,
		bitrateKbps:  bitrateKbps,
		push:         push,
		ctrl:         make(chan djCommand, 8),
		playlist:     newBackendPlaylist(audioDir, studioID, playlistEndpoint, apiKey),
		nowMu:        make(chan struct{}, 1),
		fallbackPath: fallbackFile,
		client:       analytics.NewClient(ingestEndpoint, apiKey),
		events:       make(chan analytics.IngestPlayBatch, 64),
	}
}

func (a *autoDJ) streamFile(ctx context.Context, path string, chunkSize int) error {
	f, err := os.Open(path)
	if err != nil {
		return &TrackError{Path: path, Kind: "open", Err: err}
	}
	defer f.Close()

	err = a.streamReader(ctx, f, path, chunkSize)
	if err == nil {
		a.queueEvent(analytics.IngestPlayBatch{
			Type:    "track_ended",
			TrackID: a.current.ID,
			File:    a.current.File,
			Source:  "AUTO",
			EndedAt: time.Now().UTC().Format(time.RFC3339),
		})
	}
	return err
}

// streamSilence loops a short embedded silent clip so an empty playlist produces
// valid audio instead of dead air; it returns after ~1s so the caller can recheck the playlist.
func (a *autoDJ) streamSilence(ctx context.Context, chunkSize int) error {
	return a.streamReader(ctx, bytes.NewReader(silenceMP3), "", chunkSize)
}

// streamReader paces reads from r at the stream bitrate, pushing chunks to the studio feed.
// activeFile "" (used for silence) never matches a.activeFile, so a buffered skip is a no-op.
func (a *autoDJ) streamReader(ctx context.Context, r io.Reader, activeFile string, chunkSize int) error {
	buf := make([]byte, chunkSize)

	for {
		select {
		case <-ctx.Done():
			return context.Canceled
		case cmd := <-a.ctrl:
			switch cmd {
			case cmdSkip:
				a.lock()
				same := a.activeFile == activeFile
				a.unlock()
				if same {
					return &TrackError{Path: activeFile, Kind: "skipped", Err: io.EOF}
				}
			case cmdForceReload:
				a.playlist.forceReload()
			case cmdStop:
				return context.Canceled
			}
		default:
		}

		n, rerr := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			a.push(chunk)
			a.pace.wait(n)
			// Refresh mid-track (in the background), so the track boundary
			// doesn't have to wait on the backend.
			a.playlist.ensure()
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil // normal end
			}
			return &TrackError{Path: activeFile, Kind: "read", Err: rerr}
		}
	}
}

// attempt to stream the fallback track if configured and present.
// returns true if it started streaming fallback, false otherwise.
func (a *autoDJ) tryFallback(ctx context.Context, chunkSize int) bool {
	if a.fallbackPath == "" {
		return false
	}
	if _, err := os.Stat(a.fallbackPath); err != nil {
		return false
	}
	_, name := filepath.Split(a.fallbackPath)

	a.lock()
	a.current = Track{Title: name, File: a.fallbackPath}
	a.next = Track{}
	a.startedAt = time.Now()
	a.activeFile = a.fallbackPath

	a.queueEvent(analytics.IngestPlayBatch{
		Type:      "track_started",
		TrackID:   a.current.ID,
		File:      a.current.File,
		Source:    "AUTO",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	})

	a.unlock()

	err := a.streamFile(ctx, a.fallbackPath, chunkSize)
	if err != nil && !errors.Is(err, io.EOF) {
		log.Printf("autoDJ: error streaming fallback %s: %v", a.fallbackPath, err)
	}
	a.lock()
	a.activeFile = ""
	a.unlock()
	return true
}

func (a *autoDJ) Play(ctx context.Context) {
	// 128 kbps => 16 KB/s
	bytesPerSec := int(float64(a.bitrateKbps) * 1000.0 / 8.0)
	chunkSize := audioChunkSize
	a.pace = pacer{bytesPerSec: bytesPerSec}
	go a.sendEvents(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		a.playlist.ensure()
		cur, ok := a.playlist.current()
		if !ok && !a.resumeTried {
			cur, ok = a.resume()
		}
		if !ok {
			if c2, ok2 := a.playlist.advance(); ok2 {
				cur = c2
				ok = true
			} else {
				if a.tryFallback(ctx, chunkSize) {
					continue
				}
				if err := a.streamSilence(ctx, chunkSize); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("autoDJ: error streaming silence: %v", err)
					time.Sleep(500 * time.Millisecond)
				}
				continue
			}
		}
		next, _ := a.playlist.nextTrack()

		a.lock()
		a.current = cur
		a.next = next
		a.startedAt = time.Now()
		a.activeFile = cur.File

		a.queueEvent(analytics.IngestPlayBatch{
			Type:      "track_started",
			TrackID:   a.current.ID,
			File:      a.current.File,
			Source:    "AUTO",
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		})

		a.unlock()
		a.saveState(cur)

		log.Printf("autoDJ: playing %s", cur.Title)
		if err := a.streamFile(ctx, cur.File, chunkSize); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			var trackErr *TrackError
			if errors.As(err, &trackErr) && trackErr.Kind == "skipped" {
				log.Printf("autoDJ: skipped %s", cur.Title)
			} else {
				log.Printf("autoDJ: track %s failed: %v", cur.Title, err)
			}
		}

		a.playlist.ensure()
		a.playlist.advance()
	}
}

type TrackError struct {
	Path string
	Kind string
	Err  error
}

func (e *TrackError) Error() string {
	return e.Kind + ": " + e.Path + ": " + e.Err.Error()
}
