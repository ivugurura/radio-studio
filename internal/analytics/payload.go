package analytics

import "time"

type ListenerSession struct {
	ID         string     `json:"id"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	IPHash     string     `json:"ip_hash"`
	UserAgent  string     `json:"user_agent"`
	ClientType string     `json:"client_type"`
	Country    string     `json:"country"`
	Region     string     `json:"region"`
	City       string     `json:"city"`
	Lat        float64    `json:"lat"`
	Lon        float64    `json:"lon"`
	TotalBytes int64      `json:"total_bytes"`
}

type ListenerBucket struct {
	Interval        string         `json:"interval"`
	BucketStart     time.Time      `json:"bucket_start"`
	ActivePeak      int            `json:"active_peak"`
	ListenerMinutes int            `json:"listener_minutes"`
	Countries       map[string]int `json:"countries"`
}

type IngestListenerBatch struct {
	StudioID string            `json:"studio_id"`
	Sessions []ListenerSession `json:"sessions"`
	Buckets  []ListenerBucket  `json:"buckets"`
}

type IngestPlayBatch struct {
	Type      string `json:"type"`
	TrackID   string `json:"track_id,omitempty"`
	File      string `json:"file,omitempty"`
	Source    string `json:"source,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
}
