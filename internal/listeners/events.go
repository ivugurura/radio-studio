package listeners

import "time"

type EventType string

const (
	EventConnected    EventType = "connected"
	EventDisconnected EventType = "disconnected"
	EventEnriched     EventType = "enriched"
	EventHeartbeat    EventType = "heartbeat"
)

type Event struct {
	Type      EventType
	Timestamp time.Time
	Listener  *Listener
}
