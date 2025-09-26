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
	for {
		select {
		case <-ctx.Done():
			return
		default:
			files, err := os.ReadDir(a.AudioDir)
			if err != nil {
				log.Printf("AutoDJ: Could not read dir %s", a.AudioDir)
				continue
			}

			if len(files) == 0 {
				time.Sleep(5 * time.Second)
				continue
			}
			for _, f := range files {
				if f.IsDir() {
					continue
				}

				path := filepath.Join(a.AudioDir, f.Name())
				file, err := os.Open(path)
				if err != nil {
					log.Printf("AutoDJ: could not open file %s: %v", path, err)
					continue
				}
				buf := make([]byte, 4096)
				for {
					n, err := file.Read(buf)
					if n > 0 {
						a.broadcast(buf[:n])
					}
					if err != nil {
						break
					}
					select {
					case <-ctx.Done():
						file.Close()
						return
					default:
					}
				}
				file.Close()
				select {
				case <-ctx.Done():
					return
				default:
				}
			}
		}
	}
}
