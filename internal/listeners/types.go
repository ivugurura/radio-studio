package listeners

import (
	"net"
	"sync/atomic"
	"time"
)

type Listener struct {
	ID       string
	StudioId string

	// Connection metadata
	ConnectedAt    time.Time
	DisconnectedAt atomic.Pointer[time.Time]

	// Network / Client
	RemoteIP   net.IP
	IPHash     string
	Country    string
	Region     string
	City       string
	Lat, Lon   float64
	UserAgent  string
	ClientType string

	// Stats
	ByteSent      atomic.Int64
	LastHeartbeat atomic.Pointer[time.Time]

	// Internal flags
	Enriched atomic.Bool
}

func (l *Listener) MarkDisconnected() {
	now := time.Now()
	l.DisconnectedAt.Store(&now)
}
