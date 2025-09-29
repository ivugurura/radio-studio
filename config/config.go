package config

import (
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

	// Backend integration
	BackendIngestURL   string
	BackendAPIKey      string
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
		AudioDir:           get("AUDIO_DIR", "./audio"),
		GeoIPDBPath:        get("GEOIP_DB_PATH", "./GeoLite2-City.mmdb"),
		IPHashSalt:         get("IP_HASH_SALT", "change-me"),
		EnableGeoIp:        get("ENABLE_GEOIP", "1") == "1",
		BackendIngestURL:   get("BACKEND_INGEST_URL", ""), // e.g. https://api.example.com/internal/listener-events
		BackendAPIKey:      get("BACKEND_API_KEY", ""),
		EventFlushInterval: durationEnv("EVENT_FLUSH_INTERVAL", 5*time.Second),
		SnapshotInterval:   durationEnv("SNAPSHOT_INTERVAL", 5*time.Second),
	}

	return cfg
}

func durationEnv(key string, dfault time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return d
		}
	}
	return dfault
}
