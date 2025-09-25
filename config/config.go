package config

import "os"

type Config struct {
	ListenAddr string
}

func LoadConfig() *Config {
	add := os.Getenv("LISTEN_ADDRESS")

	if add == "" {
		add = ":8000"
	}

	return &Config{
		ListenAddr: add,
	}
}
