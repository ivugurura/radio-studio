package config

import (
	"fmt"
	"log"
	"os"
	"time"
)

type Config struct {
	ListenAddr string
	AudioDir   string

	// Geo analytics
	GeoIPDBPath string
	IPHashSalt  string
	EnableGeoIp bool

	DefaultBitrateKbps int

	// Backend integration
	BackendIngestURL   string
	BackendAPIKey      string
	BackendPlaylistURL string
	EventFlushInterval time.Duration
	SnapshotInterval   time.Duration
}

func LoadConfig() *Config {
	get := func(key, dfault string) string {
		v := os.Getenv(key)
		if v == "" {
			return dfault
		}
		return v
	}

	cfg := &Config{
		ListenAddr:         get("LISTEN_ADDR", ":8000"),
		AudioDir:           get("AUDIO_DIR", ""),
		GeoIPDBPath:        get("GEOIP_DB_PATH", "./GeoLite2-City.mmdb"),
		IPHashSalt:         get("IP_HASH_SALT", "change-me"),
		EnableGeoIp:        get("ENABLE_GEOIP", "1") == "1",
		BackendIngestURL:   get("BACKEND_INGEST_URL", ""), // e.g. https://api.example.com/internal/listener-events
		BackendAPIKey:      get("BACKEND_API_KEY", ""),
		BackendPlaylistURL: get("BACKEND_PLAYLIST_URL", ""),
		EventFlushInterval: durationEnv("EVENT_FLUSH_INTERVAL", 5*time.Second),
		SnapshotInterval:   durationEnv("SNAPSHOT_INTERVAL", 5*time.Second),
		DefaultBitrateKbps: intEnv("DEFAULT_BITRATE_KBPS", 128),
	}

	return cfg
}

func durationEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return d
		}
		log.Printf("config: invalid duration in %s=%s (using default)", key, v)
	}
	return def
}

func intEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
