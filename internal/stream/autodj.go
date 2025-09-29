package stream

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type AutoDJ struct {
	AudioDir          string
	broadcast         func([]byte)
	targetBitrateKbps int // configure (e.g. 128)
}

func NewAutoDJ(audioDir string, bitrateKbps int, broadcast func([]byte)) *AutoDJ {
	return &AutoDJ{
		AudioDir:          audioDir,
		targetBitrateKbps: bitrateKbps,
		broadcast:         broadcast,
	}
}

func (a *AutoDJ) Play(ctx context.Context) {
	const chunkSize = 8192 // bigger chunk improves throughput vs overhead

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		files, err := os.ReadDir(a.AudioDir)
		if err != nil {
			log.Printf("AutoDJ: could not read dir %s: %v", a.AudioDir, err)
			time.Sleep(5 * time.Second)
			continue
		}
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
		// if len(files) == 0 {
		// 	time.Sleep(5 * time.Second)
		// 	continue
		// }
		// sort.Strings(playlist) //deterministic order

		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			if !strings.HasSuffix(strings.ToLower(name), ".mp3") {
				continue
			}
			select {
			case <-ctx.Done():
				return
			default:
			}

			path := filepath.Join(a.AudioDir, name)
			f, err := os.Open(path)
			if err != nil {
				log.Printf("AutoDJ: could not open file %s: %v", path, err)
				continue
			}
			log.Printf("AutoDJ: playing %s", path)

			bytesPerSec := (a.targetBitrateKbps * 1000) / 8
			start := time.Now()
			var sentBytes int64

			buf := make([]byte, chunkSize)
			for {
				select {
				case <-ctx.Done():
					f.Close()
					return
				default:
				}

				n, err := f.Read(buf)
				if n > 0 {
					// COPY the data so it is immutable for listeners
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					a.broadcast(chunk)
					sentBytes += int64(n)

					//pacing
					expectedElapsed := time.Duration(float64(bytesPerSec) * float64(time.Second))
					actual := time.Since(start)
					if expectedElapsed > actual {
						// clamp to avoid very long sleeps due to bitrate mismatch
						sleep := min(expectedElapsed-actual, 500*time.Millisecond)
						time.Sleep(sleep)
					}

					// (Optional) basic pacing if you find it necessary:
					// Assume average bitrate 128 kbps = 16000 bytes/sec
					// targetElapsed := time.Duration(float64(sentBytes)/16000.0*float64(time.Second))
					// delta := targetElapsed - time.Since(start)
					// if delta > 0 {
					//     time.Sleep(delta)
					// }
				}
				if err != nil {
					// EOF or read error -> go to next file
					break
				}
			}
			_ = f.Close()
			// Removed fake silence injection. If you need gaps, add real MP3 silence file.
		}
	}
}
