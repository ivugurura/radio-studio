package listeners

import (
	"sync"
)

// Store holds one entry per open connection. It is keyed by the Listener
// pointer, not Listener.ID: the ID is the listener's session ID, which several
// connections can share (two tabs, or a reconnect while the old socket has not
// timed out yet).
type Store struct {
	mu        sync.RWMutex
	listeners map[*Listener]struct{}
}

func NewStore() *Store {
	return &Store{
		listeners: make(map[*Listener]struct{}),
	}
}

func (s *Store) Add(l *Listener) {
	s.mu.Lock()
	s.listeners[l] = struct{}{}
	s.mu.Unlock()
}

func (s *Store) Remove(l *Listener) {
	s.mu.Lock()
	delete(s.listeners, l)
	s.mu.Unlock()
}

func (s *Store) Active() []*Listener {
	s.mu.RLock()
	defer s.mu.RUnlock()
	listeners := make([]*Listener, 0, len(s.listeners))
	for l := range s.listeners {
		if l.DisconnectedAt.Load() == nil {
			listeners = append(listeners, l)
		}
	}
	return listeners
}
