package analytics

import "time"

type StudioSnapshot struct {
	StudioID  string         `json:"studio_id"`
	Active    int            `json:"active"`
	Countries map[string]int `json:"countries"`
}

type Snapshot struct {
	GeneratedAt time.Time                 `json:"generated_at"`
	TotalActive int                       `json:"total_active"`
	Studios     map[string]StudioSnapshot `json:"studios"`
	ClientTypes map[string]int            `json:"client_types"`
}
