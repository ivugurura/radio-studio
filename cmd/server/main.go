package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	// Registered on http.DefaultServeMux by their init funcs (expvar ->
	// /debug/vars with memstats, pprof -> /debug/pprof/*). The public server
	// below uses its own mux, so these are only reachable through the
	// localhost-only debug listener started when ENABLE_PPROF is set.
	_ "expvar"
	_ "net/http/pprof"

	"github.com/ivugurura/radio-studio/config"
	"github.com/ivugurura/radio-studio/internal/geo"
	"github.com/ivugurura/radio-studio/internal/netutil"
	"github.com/ivugurura/radio-studio/internal/stream"
	"github.com/joho/godotenv"
)

const streamingConfigRefreshInterval = 5 * time.Minute

func loadStreamingCredentials(s *stream.Studio, backendAPI, backendAPIKey string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := stream.FetchStreamingConfig(ctx, backendAPI, backendAPIKey, s.ID)
	if err != nil {
		log.Printf("streaming config: fetch failed for studio %s: %v", s.ID, err)
		return
	}
	s.SetCredentials(cfg.Username, cfg.Password)
	log.Printf("streaming config: loaded credentials for studio %s (user=%s)", s.ID, cfg.Username)
}

func startStreamingCredentialRefresh(s *stream.Studio, backendAPI, backendAPIKey string) {
	loadStreamingCredentials(s, backendAPI, backendAPIKey)
	go func() {
		t := time.NewTicker(streamingConfigRefreshInterval)
		defer t.Stop()
		for range t.C {
			loadStreamingCredentials(s, backendAPI, backendAPIKey)
		}
	}()
}

// backendOnlyActions are control actions only the backend may trigger; the
// admin UI goes through the backend, which authorizes the user first.
var backendOnlyActions = map[string]bool{"skip": true}

func requireBackendToken(apiKey string) stream.RequestValidator {
	return func(r *http.Request, studioID, action string) error {
		if !backendOnlyActions[action] {
			return nil
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || apiKey == "" || subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
			return errors.New("Not authorized")
		}
		return nil
	}
}

func main() {
	_ = godotenv.Load()
	cfg := config.LoadConfig()
	geoResolver := geo.NewResolver(cfg.GeoIPDBPath, cfg.IPHashSalt, cfg.EnableGeoIp)
	defer geoResolver.Close()

	opts := []stream.ManagerOption{
		stream.WithDefaultBr(cfg.DefaultBrKbps),
		stream.WithDefaultSr(cfg.DefaultSrHz),
		stream.WithDefaultCh(cfg.DefaultCh),
		stream.WithSnapshotInterval(cfg.SnapshotInterval),
		stream.WithRequestValidator(requireBackendToken(cfg.BackendAPIKey)),
	}

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

	if cfg.BackendAPI != "" {
		backendIngestURL := cfg.BackendAPI + "/studios/" + s1.ID + "/listener-events"
		s1.StartAnalytics(backendIngestURL, cfg.BackendAPIKey, cfg.EventFlushInterval)
	}

	// Live-ingest credentials come from the backend now, not .env.
	if cfg.BackendAPI != "" {
		startStreamingCredentialRefresh(s1, cfg.BackendAPI, cfg.BackendAPIKey)
	} else {
		log.Printf("streaming config: BACKEND_API not set; live ingest auth will reject all sources")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/studios/", netutil.WithCORS(manager.RouteStudioRequest, cfg.AllowedOrigins))

	stopMon := make(chan struct{})
	// manager.StartMonitor(30*time.Second, stopMon)

	defer func() {
		close(stopMon)
		manager.Shutdown()
	}()

	// ENABLE_PPROF starts a localhost-only debug listener exposing
	// /debug/pprof/* (goroutine count ~= live listeners) and /debug/vars
	// (memstats). Handy for the /listen stress test; off by default.
	if os.Getenv("ENABLE_PPROF") != "" {
		pprofAddr := os.Getenv("PPROF_ADDR")
		if pprofAddr == "" {
			pprofAddr = "127.0.0.1:6060"
		}
		go func() {
			log.Printf("pprof/expvar debug listener on %s", pprofAddr)
			if err := http.ListenAndServe(pprofAddr, nil); err != nil {
				log.Printf("pprof/expvar listener stopped: %v", err)
			}
		}()
	}

	log.Printf("Streaming server running at %s\n", cfg.ListenAddr)

	if err := http.ListenAndServe(":"+cfg.ListenAddr, mux); err != nil {
		log.Fatal("Server failed ", err)
	}
}
