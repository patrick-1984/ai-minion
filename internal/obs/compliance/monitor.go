package compliance

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Attr is one attribute of a record: a string, an int64, a bool or a
// []string.
type Attr struct {
	Key   string
	Value any
}

// Record is one compliance record before it is exported.
type Record struct {
	// Event is the event name, such as minion.session.action.
	Event string
	Time  time.Time
	Attrs []Attr
	// Body is the content of an input or output record; empty otherwise.
	Body string
}

// Event names of the compliance stream.
const (
	EventAction = "minion.session.action"
	EventInput  = "minion.session.input"
	EventOutput = "minion.session.output"
	EventPolicy = "minion.compliance.policy"
	EventGap    = "minion.compliance.gap"
)

// Sink receives the records. Emit never blocks and never fails: a record that
// cannot be kept is counted by the sink.
type Sink interface {
	Emit(Record)
}

// Deps are the lookups the recorder needs from the Hub.
type Deps struct {
	// UserManaged reports whether the Worker is user-managed. Content of the
	// sessions of a Worker that is not (or whose state cannot be read) is
	// never recorded; actions are.
	UserManaged func(ctx context.Context, workerID string) (bool, error)
	// WorkerHasFeature reports whether the Worker's current connection
	// announced a feature (proto.FeatureArgsDigest, for the replayed flag).
	WorkerHasFeature func(workerID, feature string) bool
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Monitor holds the policy in force, the sink and the recorder state. It is
// safe for concurrent use. The zero policy is "off": a Hub without
// compliance monitoring pays one atomic load per call.
type Monitor struct {
	deps atomic.Pointer[Deps]
	cur  atomic.Pointer[Policy]
	sink atomic.Pointer[sinkBox]

	mu    sync.Mutex
	hist  map[int64]Policy
	order []int64
	subs  map[int]func(Policy)
	next  int

	dedupe *Dedupe
	// noSink counts the records that were due while no sink existed (the
	// exporter could not be built).
	noSink atomic.Uint64
}

type sinkBox struct{ s Sink }

// maxHistory bounds the policies kept to judge what a client saw.
const maxHistory = 64

// NewMonitor returns a Monitor with the policy off.
func NewMonitor(d Deps) *Monitor {
	m := &Monitor{hist: map[int64]Policy{}, subs: map[int]func(Policy){}, dedupe: NewDedupe(DedupeScopes, DedupeTurns)}
	off := Policy{Level: LevelOff}
	m.cur.Store(&off)
	m.deps.Store(&d)
	return m
}

// SetDeps replaces the lookups (the Hub learns some of them after the Monitor
// exists).
func (m *Monitor) SetDeps(d Deps) { m.deps.Store(&d) }

func (m *Monitor) now() time.Time {
	if d := m.deps.Load(); d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Policy returns the policy in force.
func (m *Monitor) Policy() Policy {
	if m == nil {
		return Policy{Level: LevelOff}
	}
	return *m.cur.Load()
}

// SetPolicy makes p the policy in force, remembers it (so that a client that
// saw an older version can be judged) and tells the subscribers (the live
// notice). The output dedupe starts afresh with every new version.
func (m *Monitor) SetPolicy(p Policy) {
	if p.Level == "" {
		p.Level = LevelOff
	}
	m.mu.Lock()
	old := m.cur.Load()
	m.cur.Store(&p)
	if _, ok := m.hist[p.Version]; !ok {
		m.order = append(m.order, p.Version)
		if len(m.order) > maxHistory {
			delete(m.hist, m.order[0])
			m.order = m.order[1:]
		}
	}
	m.hist[p.Version] = p
	subs := make([]func(Policy), 0, len(m.subs))
	for _, f := range m.subs {
		subs = append(subs, f)
	}
	m.mu.Unlock()
	if old == nil || old.Version != p.Version {
		m.dedupe.Reset()
	}
	for _, f := range subs {
		f(p)
	}
}

// Subscribe registers fn for every SetPolicy and returns its cancel function.
func (m *Monitor) Subscribe(fn func(Policy)) (cancel func()) {
	m.mu.Lock()
	id := m.next
	m.next++
	m.subs[id] = fn
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.subs, id)
		m.mu.Unlock()
	}
}

// SetSink sets the sink that receives the records; nil removes it (records
// are then counted as having no sink).
func (m *Monitor) SetSink(s Sink) {
	if s == nil {
		m.sink.Store(nil)
		return
	}
	m.sink.Store(&sinkBox{s: s})
}

// NoSink returns how many records were due while no sink existed.
func (m *Monitor) NoSink() uint64 { return m.noSink.Load() }

func (m *Monitor) emit(r Record) {
	if b := m.sink.Load(); b != nil {
		b.s.Emit(r)
		return
	}
	m.noSink.Add(1)
}

// Emit sends a record built by the caller (the policy record, a gap) to the
// sink.
func (m *Monitor) Emit(r Record) { m.emit(r) }

// Stale decides the live-notice rule of design §5.7 for a client that last
// saw policy version seen (haveSeen false: it sent no version). The call is
// refused when the policy in force is stricter than the one the client saw and
// op carries content. A version this process does not know counts as a policy
// that recorded nothing; a looser policy never refuses.
func (m *Monitor) Stale(seen int64, haveSeen bool, op string) (notice string, refuse bool) {
	if m == nil || !haveSeen || !ContentBearing(op) {
		return "", false
	}
	cur := m.Policy()
	if !cur.Active() || seen == cur.Version {
		return "", false
	}
	m.mu.Lock()
	old, ok := m.hist[seen]
	m.mu.Unlock()
	if !ok {
		old = Policy{Level: LevelOff}
	}
	if !cur.StricterThan(old) {
		return "", false
	}
	return cur.Notice() + ". Check it and repeat the request; nothing was sent.", true
}
