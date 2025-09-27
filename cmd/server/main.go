package main

import (
	"log"
	"net/http"
	"time"

	"github.com/ivugurura/ivugurura-radio/config"
	"github.com/ivugurura/ivugurura-radio/internal/stream"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()
	cfg := config.LoadConfig()

	manager := stream.NewManager(cfg.AudioDir, 128)

	manager.RegisterStudio("reformation-rw")
	manager.RegisterStudio("reformation-congo")

	http.HandleFunc("/studio/", manager.RouteStudioRequest)

	// optional monitoring
	stopMon := make(chan struct{})
	manager.StartMonitor(30*time.Second, stopMon)

	defer func() {
		close(stopMon)
		manager.Shutdown()
	}()
	log.Printf("Streaming server running at %s\n", cfg.ListenAddr)

	if err := http.ListenAndServe(cfg.ListenAddr, nil); err != nil {
		log.Fatal("Server failed ", err)
	}
}
