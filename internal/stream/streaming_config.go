package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// StreamingConfig holds a studio's live-ingest credentials and encoder
// format, fetched from the backend so a password rotation needs no redeploy.
type StreamingConfig struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	BitrateKbps  int    `json:"bitrate_kbps"`
	SampleRateHz int    `json:"sample_rate_hz"`
	Channels     int    `json:"channels"`
}

var streamingConfigHTTPClient = &http.Client{Timeout: 5 * time.Second}

// FetchStreamingConfig retrieves a studio's streaming credentials from the backend.
func FetchStreamingConfig(ctx context.Context, backendAPI, apiKey, studioID string) (StreamingConfig, error) {
	var cfg StreamingConfig
	if backendAPI == "" {
		return cfg, fmt.Errorf("backend API not configured")
	}
	url := backendAPI + "/studios/" + studioID + "/streaming-config"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return cfg, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	res, err := streamingConfigHTTPClient.Do(req)
	if err != nil {
		return cfg, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return cfg, fmt.Errorf("streaming config fetch failed: status=%d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}
