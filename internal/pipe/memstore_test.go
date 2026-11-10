package pipe

import (
	"context"
	"sync"
	"time"
)

// memStore is the in-memory Store used by the unit tests.
type memStore struct {
	mu       sync.Mutex
	nextID   int64
	messages []Message
	turns    []Turn
}

func newMemStore() *memStore { return &memStore{} }

func (s *memStore) AppendMessage(_ context.Context, m Message) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	m.ID = s.nextID
	s.messages = append(s.messages, m)
	return m, nil
}

func (s *memStore) Messages(_ context.Context, peer Peer, afterID int64, limit int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.messages {
		if m.Peer == peer && m.ID > afterID {
			out = append(out, m)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *memStore) CountMessagesSince(_ context.Context, peer Peer, since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.messages {
		if m.Peer == peer && !m.At.Before(since) {
			n++
		}
	}
	return n, nil
}

func (s *memStore) PutTurn(_ context.Context, t Turn) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.turns {
		if e.TurnID == t.TurnID {
			return false, nil
		}
	}
	s.turns = append(s.turns, t)
	return true, nil
}

func (s *memStore) Turns(_ context.Context, peer Peer, afterTurnID string, limit int) ([]Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := 0
	if afterTurnID != "" {
		found := false
		for i, t := range s.turns {
			if t.TurnID == afterTurnID && t.Peer == peer {
				start, found = i+1, true
				break
			}
		}
		if !found {
			return nil, newError(CodeNotFound, "unknown turn %s", afterTurnID)
		}
	}
	var out []Turn
	for _, t := range s.turns[start:] {
		if t.Peer == peer {
			out = append(out, t)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *memStore) DeleteBefore(_ context.Context, cutoff time.Time) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var km []Message
	for _, m := range s.messages {
		if !m.At.Before(cutoff) {
			km = append(km, m)
		}
	}
	var kt []Turn
	for _, t := range s.turns {
		if !t.At.Before(cutoff) {
			kt = append(kt, t)
		}
	}
	dm, dt := len(s.messages)-len(km), len(s.turns)-len(kt)
	s.messages, s.turns = km, kt
	return dm, dt, nil
}
