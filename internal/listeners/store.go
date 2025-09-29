package listeners

import (
	"sync"
)

type Store struct {
	mu        sync.RWMutex
	listeners map[string]*Listener
	byStudio  map[string]map[string]*Listener
}

func NewStore() *Store {
	return &Store{
		listeners: make(map[string]*Listener),
		byStudio:  make(map[string]map[string]*Listener),
	}
}

func (s *Store) Add(l *Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners[l.ID] = l
	if s.byStudio[l.StudioId] == nil {
		s.byStudio[l.StudioId] = make(map[string]*Listener)
	}
	s.byStudio[l.StudioId][l.ID] = l
}

func (s *Store) Remove(id string) *Listener {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.listeners[id]
	if !ok {
		return nil
	}
	delete(s.listeners, id)

	if m := s.byStudio[l.StudioId]; m != nil {
		delete(m, id)
		if len(m) == 0 {
			delete(s.byStudio, l.StudioId)
		}
	}
	return l
}

func (s *Store) Get(id string) (*Listener, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.listeners[id]
	return l, ok
}

func (s *Store) ActiveByStudio(studioId string) []*Listener {
	s.mu.Lock()
	defer s.mu.Unlock()
	var listeners []*Listener
	for _, l := range s.byStudio[studioId] {
		if l.DisconnectedAt.IsZero() {
			listeners = append(listeners, l)
		}
	}
	return listeners
}
