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
// cannot be kept is counted by the sink. It returns false when the record was
// not queued (dropped), so that output deduplication does not rely on a body
// that never left.
type Sink interface {
	Emit(Record) bool
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

// generation is the unit a call is admitted under: the policy in force, the
// sink that belongs to it and the output dedupe of that sink. A call keeps the
// generation it began with, so what it records can only reach the destination
// its own policy named, never a replacement installed meanwhile (A49 review:
// retarget and live policy changes).
type generation struct {
	pol    Policy
	sink   Sink
	dedupe *Dedupe
	// retired is set when a newer generation replaced this one; records of a
	// call admitted under it are dropped and counted.
	retired atomic.Bool
}

// Monitor holds the policy in force, the sink and the recorder state as one
// atomic generation. It is safe for concurrent use. The zero policy is "off":
// a Hub without compliance monitoring pays one atomic load per call.
type Monitor struct {
	deps atomic.Pointer[Deps]
	gen  atomic.Pointer[generation]

	mu    sync.Mutex
	hist  map[int64]Policy
	order []int64
	subs  map[int]func(Policy)
	next  int

	// noSink counts the records that were due while no sink existed (the
	// exporter could not be built) and those of a retired generation.
	noSink atomic.Uint64
}

// maxHistory bounds the policies kept to judge what a client saw.
const maxHistory = 64

// NewMonitor returns a Monitor with the policy off.
func NewMonitor(d Deps) *Monitor {
	m := &Monitor{hist: map[int64]Policy{}, subs: map[int]func(Policy){}}
	m.gen.Store(&generation{pol: Policy{Level: LevelOff}, dedupe: NewDedupe(DedupeScopes, DedupeTurns)})
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
	return m.gen.Load().pol
}

// Install makes p and s the policy and the sink in force as one generation:
// the new generation starts with a fresh output dedupe, the previous one is
// retired (its late records are dropped and counted), and the subscribers (the
// live notice) are told. The policy is remembered, so that a client that saw an
// older version can be judged. The caller seeds s with its policy record
// before Install, so that no capture is admitted before the destination knows
// the policy.
func (m *Monitor) Install(p Policy, s Sink) {
	if p.Level == "" {
		p.Level = LevelOff
	}
	g := &generation{pol: p, sink: s, dedupe: NewDedupe(DedupeScopes, DedupeTurns)}
	m.mu.Lock()
	if _, ok := m.hist[p.Version]; !ok {
		m.order = append(m.order, p.Version)
		if len(m.order) > maxHistory {
			delete(m.hist, m.order[0])
			m.order = m.order[1:]
		}
	}
	m.hist[p.Version] = p
	old := m.gen.Swap(g)
	subs := make([]func(Policy), 0, len(m.subs))
	for _, f := range m.subs {
		subs = append(subs, f)
	}
	m.mu.Unlock()
	old.retired.Store(true)
	for _, f := range subs {
		f(p)
	}
}

// SetPolicy installs p with the sink in force (tests; the Hub installs both).
func (m *Monitor) SetPolicy(p Policy) { m.Install(p, m.gen.Load().sink) }

// SetSink installs the sink with the policy in force; nil removes it.
func (m *Monitor) SetSink(s Sink) { m.Install(m.gen.Load().pol, s) }

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

// NoSink returns how many records were due while no sink existed or came from
// a retired generation.
func (m *Monitor) NoSink() uint64 { return m.noSink.Load() }

func (g *generation) emit(m *Monitor, r Record) bool {
	if g.sink == nil || g.retired.Load() {
		m.noSink.Add(1)
		return false
	}
	return g.sink.Emit(r)
}

// Emit sends a record built by the caller (the policy record, a gap) to the
// sink of the generation in force.
func (m *Monitor) Emit(r Record) { m.gen.Load().emit(m, r) }

// Stale decides the live-notice rule of design §5.7 for a client that last
// saw policy version seen (haveSeen false: it sent no version). The call is
// refused when the policy in force is stricter than the one the client saw and
// op carries content. A version this process does not know counts as a policy
// that recorded nothing; a looser policy never refuses.
func (m *Monitor) Stale(seen int64, haveSeen bool, op string) (notice string, refuse bool, cur Policy) {
	if m == nil {
		return "", false, Policy{Level: LevelOff}
	}
	cur = m.Policy()
	if !haveSeen || !ContentBearing(op) {
		return "", false, cur
	}
	if !cur.Active() || seen == cur.Version {
		return "", false, cur
	}
	m.mu.Lock()
	old, ok := m.hist[seen]
	m.mu.Unlock()
	if !ok {
		old = Policy{Level: LevelOff}
	}
	if !cur.StricterThan(old) {
		return "", false, cur
	}
	return cur.Notice() + ". Check it and repeat the request; nothing was sent.", true, cur
}
