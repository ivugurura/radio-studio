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
	if s.SetCredentials(cfg.Username, cfg.Password) {
		log.Printf("streaming config: live-ingest credentials updated for studio %s", s.ID)
	}
	s.SetStationName(cfg.StationName, cfg.StationDescription)
}

func startStreamingCredentialRefresh(s *stream.Studio, backendAPI, backendAPIKey string) {
	loadStreamingCredentials(s, backendAPI, backendAPIKey)
	go func() {
		t := time.NewTicker(streamingConfigRefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				loadStreamingCredentials(s, backendAPI, backendAPIKey)
			case <-s.Done(): // studio removed
				return
			}
		}
	}()
}

const (
	studioSyncInterval   = 5 * time.Minute
	studioFetchAttempts  = 3
	studioFetchRetryBase = 2 * time.Second
)

// legacyStudioAliases keeps the pre-rename studio URLs (encoder mounts,
// shared /listen links) working. Remove once nothing requests them.
var legacyStudioAliases = map[string]string{
	"reformation-rw": "rw",
}

func fetchStudioSlugs(cfg *config.Config) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	studios, err := stream.FetchStudios(ctx, cfg.BackendAPI, cfg.BackendAPIKey)
	if err != nil {
		return nil, err
	}
	slugs := make([]string, 0, len(studios))
	for _, s := range studios {
		if s.Slug != "" {
			slugs = append(slugs, s.Slug)
		}
	}
	return slugs, nil
}

// initialStudioSlugs asks the backend for the active studios, retrying briefly,
// and falls back to STUDIOS so the server can start while the backend is down.
func initialStudioSlugs(cfg *config.Config) []string {
	if cfg.BackendAPI == "" {
		return cfg.FallbackStudios
	}
	for attempt := 1; attempt <= studioFetchAttempts; attempt++ {
		slugs, err := fetchStudioSlugs(cfg)
		if err == nil {
			return slugs
		}
		log.Printf("studios: fetch attempt %d/%d failed: %v", attempt, studioFetchAttempts, err)
		if attempt < studioFetchAttempts {
			time.Sleep(studioFetchRetryBase * time.Duration(attempt))
		}
	}
	log.Printf("studios: using fallback STUDIOS=%v", cfg.FallbackStudios)
	return cfg.FallbackStudios
}

// registerStudio registers a studio and starts its background work. It is a
// no-op for a studio that is already registered.
func registerStudio(manager *stream.Manager, cfg *config.Config, slug string) {
	if _, ok := manager.GetStudio(slug); ok {
		return
	}
	s := manager.RegisterStudio(slug)
	s.SetStationLinks(cfg.StationGenre, cfg.StationURL)
	s.SetSessionMergeWindow(cfg.SessionMergeWindow)
	if cfg.BackendAPI == "" {
		return
	}
	s.StartAnalytics(cfg.BackendAPI+"/studios/"+s.ID+"/listener-events", cfg.BackendAPIKey, cfg.EventFlushInterval)
	// Live-ingest credentials come from the backend, not .env.
	startStreamingCredentialRefresh(s, cfg.BackendAPI, cfg.BackendAPIKey)
}

// syncStudios keeps the registered studios in step with the backend's active
// list: it registers new studios and closes the ones that were deactivated or
// removed, disconnecting their listeners. A failed fetch changes nothing, and
// an empty list removes nothing, so a backend hiccup never takes stations
// off air.
func syncStudios(manager *stream.Manager, cfg *config.Config) {
	t := time.NewTicker(studioSyncInterval)
	defer t.Stop()
	for range t.C {
		slugs, err := fetchStudioSlugs(cfg)
		if err != nil {
			log.Printf("studios: sync failed: %v", err)
			continue
		}
		if len(slugs) == 0 {
			log.Printf("studios: backend listed no active studios; keeping %v", manager.ListStudios())
			continue
		}
		for _, slug := range slugs {
			registerStudio(manager, cfg, slug)
		}
		if removed := manager.RemoveStudiosExcept(slugs); len(removed) > 0 {
			log.Printf("studios: stopped %v (no longer active in the backend)", removed)
		}
	}
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
		stream.WithStudioAliases(legacyStudioAliases),
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

	// Studios come from the backend; STUDIOS in .env is only a fallback.
	if cfg.BackendAPI == "" {
		log.Printf("streaming config: BACKEND_API not set; live ingest auth will reject all sources")
	}
	slugs := initialStudioSlugs(cfg)
	if len(slugs) == 0 {
		log.Printf("studios: none registered yet; will retry every %s", studioSyncInterval)
	}
	for _, slug := range slugs {
		registerStudio(manager, cfg, slug)
	}
	if cfg.BackendAPI != "" {
		go syncStudios(manager, cfg)
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

	log.Printf("Streaming server running at %s", cfg.ListenAddr)

	if err := http.ListenAndServe(":"+cfg.ListenAddr, mux); err != nil {
		log.Fatal("Server failed ", err)
	}
}
