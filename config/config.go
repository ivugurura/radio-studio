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
	// Comma-separated list of allowed browser origins for studio endpoints.
	// Use "*" to allow any origin.
	AllowedOrigins string

	// Geo analytics
	GeoIPDBPath string
	IPHashSalt  string
	EnableGeoIp bool

	DefaultBrKbps int
	DefaultSrHz   int
	DefaultCh     int

	// Backend integration
	BackendIngestURL   string
	BackendAPIKey      string
	BackendAPI         string
	EventFlushInterval time.Duration
	SnapshotInterval   time.Duration

	// Fallback track
	DefaultTrackFile string

	// Streaming credeentials
	User     string
	Password string
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
		ListenAddr:         get("LISTEN_ADDR", "7080"),
		AudioDir:           get("AUDIO_DIR", ""),
		AllowedOrigins:     get("ALLOWED_ORIGINS", ""),
		GeoIPDBPath:        get("GEOIP_DB_PATH", ""),
		IPHashSalt:         get("IP_HASH_SALT", ""),
		EnableGeoIp:        get("ENABLE_GEOIP", "1") == "1",
		BackendIngestURL:   get("BACKEND_INGEST_URL", ""), // e.g. https://api.example.com/internal/listener-events
		BackendAPIKey:      get("BACKEND_API_KEY", ""),
		BackendAPI:         get("BACKEND_API", ""),
		EventFlushInterval: durationEnv("EVENT_FLUSH_INTERVAL", 5*time.Second),
		SnapshotInterval:   durationEnv("SNAPSHOT_INTERVAL", 5*time.Second),
		DefaultBrKbps:      intEnv("DEFAULT_BR_KBPS", 128),
		DefaultSrHz:        intEnv("DEFAULT_SR_HZ", 48000),
		DefaultCh:          intEnv("DEFAULT_CH", 2),
		DefaultTrackFile:   get("DEFAULT_TRACK_FILE", ""),
		User:               get("STREAM_USER", ""),
		Password:           get("STREAM_PASSWORD", ""),
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
