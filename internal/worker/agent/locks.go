package agent

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrLockTimeout is returned by [Locks.Lock] when the lock was not free
// within the wait. The executor reports it as failed_not_sent with reason busy
// (KEEP, AD-15).
var ErrLockTimeout = errors.New("agent: writer lock wait timed out")

// Locks is a set of named mutexes whose acquisition can time out and honours
// a context. The Worker has one set; every entry point that writes to a pane
// (the Hub op server, the local socket, the pipe) takes the pane's lock from
// that same set, so writers never interleave (AD-15: writer locks live only on
// the Worker, one per pane).
//
// The zero value is not usable; use [NewLocks].
type Locks struct {
	mu sync.Mutex
	m  map[string]*lockEntry
}

type lockEntry struct {
	token chan struct{} // holds one token while the lock is free
	refs  int           // holders and waiters, the entry is dropped at zero
}

// NewLocks returns an empty set of locks.
func NewLocks() *Locks { return &Locks{m: map[string]*lockEntry{}} }

// PaneKey is the lock name of a pane id (a tmux "%3" or the ConPTY pane id).
func PaneKey(paneID string) string { return "pane:" + paneID }

// requestKey is the lock name that serialises two requests with one request
// id, so that the second one finds the first one's cached result.
func requestKey(requestID string) string { return "req:" + requestID }

// Lock waits up to wait for the lock named key, then returns the function that
// releases it. It returns [ErrLockTimeout] when the wait ran out and the
// context's error when the context ended first. A wait of zero or less tries
// once without waiting. The returned function is safe to call more than once.
func (l *Locks) Lock(ctx context.Context, key string, wait time.Duration) (unlock func(), err error) {
	e := l.acquireEntry(key)
	// An ended context never takes a lock, not even a free one.
	if err := ctx.Err(); err != nil {
		l.releaseEntry(key, e)
		return nil, err
	}
	select {
	case <-e.token:
		return l.unlocker(key, e), nil
	default:
	}
	if wait <= 0 {
		l.releaseEntry(key, e)
		return nil, ErrLockTimeout
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-e.token:
		return l.unlocker(key, e), nil
	case <-timer.C:
		l.releaseEntry(key, e)
		return nil, ErrLockTimeout
	case <-ctx.Done():
		l.releaseEntry(key, e)
		return nil, ctx.Err()
	}
}

func (l *Locks) acquireEntry(key string) *lockEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[key]
	if e == nil {
		e = &lockEntry{token: make(chan struct{}, 1)}
		e.token <- struct{}{}
		l.m[key] = e
	}
	e.refs++
	return e
}

func (l *Locks) releaseEntry(key string, e *lockEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.refs--
	if e.refs == 0 && l.m[key] == e {
		delete(l.m, key)
	}
}

func (l *Locks) unlocker(key string, e *lockEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			e.token <- struct{}{}
			l.releaseEntry(key, e)
		})
	}
}

// held returns the number of lock names in use (holders or waiters). It is
// for tests.
func (l *Locks) held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.m)
}
