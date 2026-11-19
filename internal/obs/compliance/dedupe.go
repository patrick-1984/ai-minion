package compliance

import (
	"container/list"
	"sync"
)

// Sizes of the dedupe memories (design §5.5).
const (
	DedupeScopes = 10000
	DedupeTurns  = 50000
)

// Dedupe decides whether output content is new. A screen or a capture is new
// when its fingerprint (SHA-256 of the payload that was actually returned)
// differs from the last one emitted for its scope (Worker, session, pane,
// result type); a pipe turn is new once per turn id. Both memories are LRUs:
// a forgotten entry only means that content is emitted once more.
type Dedupe struct {
	mu      sync.Mutex
	screens *lru
	turns   *lru
}

// NewDedupe returns a Dedupe that remembers scopes scopes and turns turns.
func NewDedupe(scopes, turns int) *Dedupe {
	return &Dedupe{screens: newLRU(scopes), turns: newLRU(turns)}
}

// ScreenChanged reports whether fp differs from the last fingerprint whose
// text was delivered for scope (true for a scope not seen). It remembers
// nothing: the caller calls MarkScreen once the record that carries the text
// was accepted by the sink, so a dropped first body is sent again by the next
// read.
func (d *Dedupe) ScreenChanged(scope, fp string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	old, ok := d.screens.get(scope)
	return !ok || old != fp
}

// MarkScreen records that the text of fp was accepted for scope.
func (d *Dedupe) MarkScreen(scope, fp string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.screens.put(scope, fp)
}

// TurnNew reports whether the text of the turn was not delivered before.
func (d *Dedupe) TurnNew(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.turns.get(key)
	return !ok
}

// MarkTurn records that the text of the turn was accepted.
func (d *Dedupe) MarkTurn(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.turns.put(key, "")
}

// Reset forgets everything (a new policy version starts every consumer of the
// stream from full content).
func (d *Dedupe) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.screens = newLRU(d.screens.cap)
	d.turns = newLRU(d.turns.cap)
}

type lru struct {
	cap int
	ll  *list.List
	m   map[string]*list.Element
}

type lruEntry struct{ k, v string }

func newLRU(n int) *lru {
	if n < 1 {
		n = 1
	}
	return &lru{cap: n, ll: list.New(), m: map[string]*list.Element{}}
}

func (l *lru) get(k string) (string, bool) {
	e, ok := l.m[k]
	if !ok {
		return "", false
	}
	l.ll.MoveToFront(e)
	return e.Value.(*lruEntry).v, true
}

func (l *lru) put(k, v string) {
	if e, ok := l.m[k]; ok {
		e.Value.(*lruEntry).v = v
		l.ll.MoveToFront(e)
		return
	}
	l.m[k] = l.ll.PushFront(&lruEntry{k: k, v: v})
	if l.ll.Len() > l.cap {
		last := l.ll.Back()
		l.ll.Remove(last)
		delete(l.m, last.Value.(*lruEntry).k)
	}
}
