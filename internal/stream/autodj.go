package stream

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/ivugurura/radio-studio/internal/analytics"
)

// control commands
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
	}
}

func (a *autoDJ) streamFile(ctx context.Context, path string, bytesPerSec, chunkSize int) error {
	f, err := os.Open(path)
	if err != nil {
		return &TrackError{Path: path, Kind: "open", Err: err}
	}
	defer f.Close()

	start := time.Now()
	var sent int64
	buf := make([]byte, chunkSize)

	for {
		select {
		case <-ctx.Done():
			return context.Canceled
		case cmd := <-a.ctrl:
			switch cmd {
			case cmdSkip:
				a.lock()
				// TODO: Please check this Carefully
				same := a.activeFile == path
				a.unlock()
				if same {
					return &TrackError{Path: path, Kind: "skipped", Err: io.EOF}
				}
			case cmdForceReload:
				a.playlist.forceReload()
			case cmdStop:
				return context.Canceled
			}
		default:
		}

		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			a.push(chunk)
			sent += int64(n)
			// pacing
			expected := time.Duration(float64(sent) / float64(bytesPerSec) * float64(time.Second))
			elapsed := time.Since(start)
			if expected > elapsed {
				time.Sleep(expected - elapsed)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				a.client.SendPlayerBatch(ctx, []analytics.IngestPlayBatch{{
					Type:    "track_ended",
					TrackID: a.current.ID,
					File:    a.current.File,
					Source:  "AUTO",
					EndedAt: time.Now().UTC().Format(time.RFC3339),
				}})
				return nil // normal end
			}
			return &TrackError{Path: path, Kind: "read", Err: rerr}
		}
	}
}

// attempt to stream the fallback track if configured and present.
// returns true if it started streaming fallback, false otherwise.
func (a *autoDJ) tryFallback(ctx context.Context, bytesPerSec, chunkSize int) bool {
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

	a.client.SendPlayerBatch(ctx, []analytics.IngestPlayBatch{{
		Type:      "track_started",
		TrackID:   a.current.ID,
		File:      a.current.File,
		Source:    "AUTO",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}})

	a.unlock()

	err := a.streamFile(ctx, a.fallbackPath, bytesPerSec, chunkSize)
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
	chunkSize := 4096

	for {
		// Check for stop before scanning playlist.
		select {
		case <-ctx.Done():
			return
		default:
		}

		// ensure we have a playlist
		a.playlist.ensure()
		cur, ok := a.playlist.current()
		if !ok {
			if c2, ok2 := a.playlist.advance(); ok2 {
				cur = c2
				ok = true
			} else {
				if a.tryFallback(ctx, bytesPerSec, chunkSize) {
					continue
				}
				time.Sleep(3 * time.Second)
				continue
			}
		}
		next, _ := a.playlist.nextTrack()

		// Update now playing
		a.lock()
		a.current = cur
		a.next = next
		a.startedAt = time.Now()
		a.activeFile = cur.File

		a.client.SendPlayerBatch(ctx, []analytics.IngestPlayBatch{{
			Type:      "track_started",
			TrackID:   a.current.ID,
			File:      a.current.File,
			Source:    "AUTO",
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		}})

		a.unlock()

		log.Printf("AudioDJ: playing %s", cur.Title)
		if err := a.streamFile(ctx, cur.File, bytesPerSec, chunkSize); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			// log * continue to the enxt track
			log.Printf("AudioDJ: file ended (%s): %v", cur.Title, err)
		}

		// After file finishes (or skipped) - advance
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
