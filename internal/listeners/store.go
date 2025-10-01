package listeners

import (
	"sync"
)

type Store struct {
	mu        sync.RWMutex
	listeners map[string]*Listener
}

func NewStore() *Store {
	return &Store{
		listeners: make(map[string]*Listener),
	}
}

func (s *Store) Add(l *Listener) {
	s.mu.Lock()
	s.listeners[l.ID] = l
	s.mu.Unlock()
}

func (s *Store) Remove(id string) *Listener {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.listeners[id]
	delete(s.listeners, id)

	return l
}

func (s *Store) Get(id string) (*Listener, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.listeners[id]
	return l, ok
}

func (s *Store) Active() []*Listener {
	s.mu.RLock()
	defer s.mu.RUnlock()
	listeners := make([]*Listener, 0, len(s.listeners))
	for _, l := range s.listeners {
		if l.DisconnectedAt.Load() == nil {
			listeners = append(listeners, l)
		}
	}
	return listeners
}
