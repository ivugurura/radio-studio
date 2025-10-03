package stream

import (
	"context"
	"errors"
	"io"
	"log"
	"os"

	// "path/filepath"
	// "strings"
	"time"
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
}

type AutoDJFactory func(audioDir string, bitrateKbps int, push func([]byte)) AutoDJ

type autoDJ struct {
	dir         string
	push        func([]byte)
	bitrateKbps int // configure (e.g. 128)

	ctrl chan djCommand

	playlist *playlistState

	// now playing metadata (guarded by playlistState's lock + this lightweight lock)
	nowMu      chan struct{} // simple channel semaphore (size 1)
	current    Track
	next       Track
	startedAt  time.Time
	activeFile string // internal guard to ensure skip t
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

func NewAutoDJ(audioDir string, bitrateKbps int, push func([]byte)) AutoDJ {
	return &autoDJ{
		dir:         audioDir,
		bitrateKbps: bitrateKbps,
		push:        push,
		ctrl:        make(chan djCommand, 8),
		playlist:    newPlaylistState(audioDir),
		nowMu:       make(chan struct{}, 1),
	}
}

func (a *autoDJ) streamFile(ctx context.Context, path string, bytesPerSec, chunkSize int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
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
				// only skip if still same active file
				a.lock()
				same := a.activeFile == path
				a.unlock()
				if same {
					log.Printf("autodj: skip current (%s)", path)
					return io.EOF
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
			if sleep := expected - time.Since(start); sleep > 0 && sleep < 700*time.Microsecond {
				time.Sleep(sleep)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return rerr
			}
			return rerr
		}
	}
}

func (a *autoDJ) Play(ctx context.Context) {
	const chunkSize = 8192 // bigger chunk improves throughput vs overhead
	// Naive pacing using target bitrate if provided; assume CBR.
	bytesPerSec := (a.bitrateKbps * 1000) / 8
	if bytesPerSec <= 0 {
		bytesPerSec = 16000 // fallback ~128 kbps
	}
	for {
		// Check for stop before scanning playlist.
		select {
		case <-ctx.Done():
			return
		default:
		}

		a.playlist.ensure()
		cur, ok := a.playlist.current()
		if !ok {
			if c2, ok2 := a.playlist.advance(); ok2 {
				cur = c2
				ok = true
			} else {
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
		a.activeFile = cur.Path
		a.unlock()

		log.Printf("AudioDJ: playing %s", cur.Path)
		err := a.streamFile(ctx, cur.Path, bytesPerSec, chunkSize)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			// log * continue to the enxt track
			log.Printf("AudioDJ: file ended (%s): %v", cur.File, err)
		}

		// After file finishes (or skipped) - advance
		a.playlist.ensure()
		a.playlist.advance()

		// entries, err := os.ReadDir(a.dir)
		// if err != nil {
		// 	time.Sleep(5 * time.Second)
		// 	continue
		// }
		// var playlist []string
		// for _, e := range entries {
		// 	if e.IsDir() {
		// 		continue
		// 	}
		// 	name := e.Name()
		// 	if strings.HasSuffix(strings.ToLower(name), ".mp3") {
		// 		playlist = append(playlist, name)
		// 	}
		// }
		// // sort.Strings(playlist) //deterministic order

		// if len(playlist) == 0 {
		// 	time.Sleep(5 * time.Second)
		// 	continue
		// }
		// for _, file := range playlist {
		// 	select {
		// 	case <-ctx.Done():
		// 		return
		// 	default:
		// 	}

		// 	path := filepath.Join(a.AudioDir, file)
		// 	f, err := os.Open(path)
		// 	if err != nil {
		// 		log.Printf("AutoDJ: could not open file %s: %v", path, err)
		// 		continue
		// 	}
		// 	log.Printf("AutoDJ: playing %s", path)

		// 	// bytesPerSec := (a.targetBitrateKbps * 1000) / 8
		// 	start := time.Now()
		// 	var sentBytes int64

		// 	buf := make([]byte, chunkSize)
		// 	for {
		// 		select {
		// 		case <-ctx.Done():
		// 			f.Close()
		// 			return
		// 		default:
		// 		}

		// 		n, err := f.Read(buf)
		// 		if n > 0 {
		// 			// COPY the data so it is immutable for listeners
		// 			chunk := make([]byte, n)
		// 			copy(chunk, buf[:n])
		// 			a.push(chunk)
		// 			sentBytes += int64(n)

		// 			//pacing
		// 			expectedElapsed := time.Duration(float64(bytesPerSec) * float64(time.Second))
		// 			actual := time.Since(start)
		// 			if expectedElapsed > actual {
		// 				// clamp to avoid very long sleeps due to bitrate mismatch
		// 				sleep := min(expectedElapsed-actual, 500*time.Millisecond)
		// 				time.Sleep(sleep)
		// 			}
		// 			// pacing
		// 			// expected := time.Duration(float64(sentBytes) / float64(bytesPerSec) * float64(time.Second))
		// 			// diff := expected - time.Since(start)
		// 			// if diff > 0 && diff < 700*time.Millisecond {
		// 			// 	time.Sleep(diff)
		// 			// }

		// 			// (Optional) basic pacing if you find it necessary:
		// 			// Assume average bitrate 128 kbps = 16000 bytes/sec
		// 			// targetElapsed := time.Duration(float64(sentBytes)/16000.0*float64(time.Second))
		// 			// delta := targetElapsed - time.Since(start)
		// 			// if delta > 0 {
		// 			//     time.Sleep(delta)
		// 			// }
		// 		}
		// 		if err != nil {
		// 			// EOF or read error -> go to next file
		// 			break
		// 		}
		// 		select {
		// 		case <-ctx.Done():
		// 			f.Close()
		// 			return
		// 		default:
		// 		}
		// 	}
		// 	_ = f.Close()
		// 	// Removed fake silence injection. If you need gaps, add real MP3 silence file.
		// }
	}
}
