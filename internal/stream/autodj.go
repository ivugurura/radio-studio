package stream

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"
)

type AutoDJ struct {
	AudioDir  string
	broadcast func([]byte)
}

func NewAutoDJ(audioDir string, broadcast func([]byte)) *AutoDJ {
	return &AutoDJ{
		AudioDir:  audioDir,
		broadcast: broadcast,
	}
}

func (a *AutoDJ) Play(ctx context.Context) {
	const bufSize = 8192 // bigger chunk improves throughput vs overhead

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
		if len(files) == 0 {
			time.Sleep(5 * time.Second)
			continue
		}

		for _, f := range files {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if f.IsDir() {
				continue
			}

			path := filepath.Join(a.AudioDir, f.Name())
			file, err := os.Open(path)
			if err != nil {
				log.Printf("AutoDJ: could not open file %s: %v", path, err)
				continue
			}
			log.Printf("AutoDJ: streaming file %s", path)

			buf := make([]byte, bufSize)
			// start := time.Now()
			var sentBytes int64

			for {
				select {
				case <-ctx.Done():
					file.Close()
					return
				default:
				}

				n, err := file.Read(buf)
				if n > 0 {
					// COPY the data so it is immutable for listeners
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					a.broadcast(chunk)
					sentBytes += int64(n)

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
			file.Close()
			// Removed fake silence injection. If you need gaps, add real MP3 silence file.
		}
	}
}
