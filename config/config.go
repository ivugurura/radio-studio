package config

import "os"

type Config struct {
	ListenAddr string
	AudioDir   string
}

func LoadConfig() *Config {
	addr := os.Getenv("LISTEN_ADDRESS")
	if addr == "" {
		addr = ":8000"
	}

	audioDir := os.Getenv("AUDIO_DIR")
	if audioDir == "" {
		audioDir = "./audio"
	}

	return &Config{
		ListenAddr: addr,
		AudioDir:   audioDir,
	}
}
