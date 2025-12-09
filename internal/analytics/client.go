package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	URL        string
	APIKey     string
	httpClient *http.Client
}

func NewClient(url, apiKey string) *Client {
	return &Client{
		URL:    url,
		APIKey: apiKey,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// sendJSON posts a JSON payload to the client's URL with auth/header handling.
func (c *Client) sendJSON(ctx context.Context, payload interface{}) error {
	if c.URL == "" {
		return nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("ingest failed: status=%d", res.StatusCode)
	}
	return nil
}

func (c *Client) SendListenerBatch(ctx context.Context, batch IngestListenerBatch) error {
	return c.sendJSON(ctx, batch)
}

func (c *Client) SendPlayerBatch(ctx context.Context, events []IngestPlayBatch) error {
	return c.sendJSON(ctx, events)
}
