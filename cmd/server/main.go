package main

import (
	"log"
	"net/http"

	"github.com/ivugurura/ivugurura-radio/config"
	"github.com/ivugurura/ivugurura-radio/internal/stream"
)

func main() {
	cfg := config.LoadConfig()

	manager := stream.NewManager()

	manager.RegisterStudio("reformation-rw")
	manager.RegisterStudio("reformantion-congo")

	http.HandleFunc("/studio/", func(w http.ResponseWriter, r *http.Request) {
		manager.RouteStudioRequest(w, r)
	})

	log.Printf("Streaming server running at %s\n", cfg.ListenAddr)

	if err := http.ListenAndServe(cfg.ListenAddr, nil); err != nil {
		log.Fatal("Server failed ", err)
	}
}
