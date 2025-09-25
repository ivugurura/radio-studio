package stream

import (
	"context"
	"log"
	"os"
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
		}
	}
}
