package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// StudioInfo is one active studio as listed by the backend.
type StudioInfo struct {
	Slug         string `json:"slug"`
	DisplayName  string `json:"display_name"`
	BitrateKbps  int    `json:"bitrate_kbps"`
	SampleRateHz int    `json:"sample_rate_hz"`
	Channels     int    `json:"channels"`
}

// FetchStudios retrieves the active studios this server should stream.
func FetchStudios(ctx context.Context, backendAPI, apiKey string) ([]StudioInfo, error) {
	if backendAPI == "" {
		return nil, fmt.Errorf("backend API not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, backendAPI+"/studios", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	res, err := streamingConfigHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("studios fetch failed: status=%d", res.StatusCode)
	}
	var studios []StudioInfo
	if err := json.NewDecoder(res.Body).Decode(&studios); err != nil {
		return nil, err
	}
	return studios, nil
}
