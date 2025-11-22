package main

import (
	"log"
	"net/http"

	"github.com/ivugurura/radio-studio/config"
	"github.com/ivugurura/radio-studio/internal/geo"
	"github.com/ivugurura/radio-studio/internal/stream"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	cfg := config.LoadConfig()
	geoResolver := geo.NewResolver(cfg.GeoIPDBPath, cfg.IPHashSalt, cfg.EnableGeoIp)
	defer geoResolver.Close()

	opts := []stream.ManagerOption{
		stream.WithDefaultBitrate(cfg.DefaultBitrateKbps),
		stream.WithSnapshotInterval(cfg.SnapshotInterval),
	}

	// If playlist URL is configured, use backend-driven AutoDJ
	if cfg.BackendAPI != "" {
		opts = append(opts, stream.WithAutoDJFactory(func(dir string, studioID string, bitrate int, push func([]byte)) stream.AutoDJ {
			studioEndpoint := cfg.BackendAPI + "/studios/" + studioID
			return stream.NewAutoDJ(dir, studioID, bitrate, push, studioEndpoint, cfg.BackendAPIKey, cfg.DefaultTrackFile)
		}))
	}

	manager := stream.NewManager(
		cfg.AudioDir,
		geoResolver,
		opts...,
	)

	s1 := manager.RegisterStudio("reformation-rw")
	// manager.RegisterStudio("reformation-congo")

	// Start analytics sync if configured
	if cfg.BackendAPI != "" {
		backendIngestURL := cfg.BackendAPI + "/studios/" + s1.ID + "/listener-events"
		s1.StartAnalytics(backendIngestURL, cfg.BackendAPIKey, cfg.EventFlushInterval)
	}

	http.HandleFunc("/studio/", manager.RouteStudioRequest)

	// optional monitoring
	stopMon := make(chan struct{})
	// manager.StartMonitor(30*time.Second, stopMon)

	defer func() {
		close(stopMon)
		manager.Shutdown()
	}()
	log.Printf("Streaming server running at %s\n", cfg.ListenAddr)

	if err := http.ListenAndServe(cfg.ListenAddr, nil); err != nil {
		log.Fatal("Server failed ", err)
	}
}
