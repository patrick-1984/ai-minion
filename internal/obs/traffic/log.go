// Package traffic records the framed traffic between the Hub and its Workers
// (design §4.1, metadata level): one record per frame on the op, ca and ev
// streams with the direction, the Worker, the stream kind, the frame type,
// the op, the request id, the size and, for a result, the duration since its
// request and the outcome or error code. It never keeps or records a frame
// body. The same package counts the traffic for the metrics (metrics.go).
//
// Owner: A49 PR 2 (T11, T12).
package traffic

import (
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Side is the process that taps.
type Side int

const (
	// SideHub taps every Worker connection of the Hub.
	SideHub Side = iota
	// SideWorker taps the Worker's own connection to its Hub.
	SideWorker
)

// Peer is the Worker of a tapped connection (Hub side; empty on a Worker).
type Peer struct {
	WorkerID, Name string
}

// Fixed limits of the log.
const (
	// QueueSize bounds the records waiting for the writer; a full queue
	// drops the new record (counted).
	QueueSize = 4096
	// maxOpen bounds the requests waiting for their result per connection.
	maxOpen = 1024
)

// Log is the traffic log of one process. Records go through a bounded queue
// to a writer goroutine, so a tap never blocks the stream it observes, also
// when standard error or a sink stalls. Every queued record is bound to the
// sinks of the moment it was queued: after a change of a destination or of
// the traffic configuration it is dropped (counted), never delivered to a
// replacement collector.
type Log struct {
	side Side
	out  *logx.Fanout
	now  func() time.Time

	cfg     atomic.Pointer[filter]
	prev    atomic.Pointer[filter] // the filter Suspend took down
	bound   atomic.Pointer[binding]
	q       chan record
	dropped atomic.Uint64
	// gate orders Close against the taps: a tap enqueues under the read
	// lock and sees closed, so nothing is queued after the writer's last
	// drain.
	gate   sync.RWMutex
	closed bool
	stop   chan struct{}
	abort  chan struct{}
	done   chan struct{}
	once   sync.Once
}

type filter struct {
	config.TrafficConfig
	slots []int
}

// binding is the handler of a filter over the sinks of one fan-out
// generation.
type binding struct {
	f       *filter
	gen     uint64
	handler slog.Handler
}

type record struct {
	at       time.Time
	b        *binding
	peer     Peer
	sent     bool
	kind     transport.Kind
	typ      string
	op       string
	reqID    string
	size     int
	duration time.Duration
	result   bool
	outcome  string
	// payload is the redacted frame body (payload level): valid JSON, at most
	// logx.RemotePayloadMax bytes, a copy that the tap made (the frame body
	// is valid only during the tap call).
	payload string
}

// New returns a log that is off until Configure turns it on.
func New(side Side, out *logx.Fanout) *Log {
	l := &Log{side: side, out: out, now: time.Now, q: make(chan record, QueueSize),
		stop: make(chan struct{}), abort: make(chan struct{}), done: make(chan struct{})}
	go l.run()
	return l
}

// Configure applies c. The destinations are slots of the process's fan-out
// (local output, syslog, OpenTelemetry), never the in-memory log; the record
// level is info whatever the slots' levels. Records queued under the previous
// configuration are dropped.
func (l *Log) Configure(c config.TrafficConfig) {
	if !c.Enabled {
		l.cfg.Store(nil)
		l.prev.Store(nil)
		return
	}
	var slots []int
	for _, d := range c.To {
		switch d {
		case config.TrafficToLocal:
			slots = append(slots, logx.SlotLocal)
		case config.TrafficToSyslog:
			slots = append(slots, logx.SlotSyslog)
		case config.TrafficToOTel:
			slots = append(slots, logx.SlotOTel)
		}
	}
	old := l.cfg.Load()
	if old == nil {
		old = l.prev.Load()
	}
	l.prev.Store(nil)
	if old != nil && reflect.DeepEqual(old.TrafficConfig, c) {
		l.cfg.Store(old) // nothing changed (a save of an unrelated setting): keep what is queued
		return
	}
	l.cfg.Store(&filter{TrafficConfig: c, slots: slots})
}

// Suspend takes the traffic log down until the next Configure: no frame is
// recorded in between. A change of destinations and of the Worker filter is
// published as Suspend, the sink change, Configure, so that a frame never
// meets the new sinks with the old filter. A record that was bound before
// stays bound to the old filter and is dropped unless Configure restores that
// very filter (the same configuration).
func (l *Log) Suspend() {
	if f := l.cfg.Swap(nil); f != nil {
		l.prev.Store(f)
	}
}

// bind returns the binding of f to the sinks of now.
func (l *Log) bind(f *filter) *binding {
	gen := l.out.Gen()
	if b := l.bound.Load(); b != nil && b.f == f && b.gen == gen {
		return b
	}
	b := &binding{f: f, gen: gen, handler: l.out.Bind(f.slots...)}
	l.bound.Store(b)
	return b
}

// current reports whether b is still the binding records are written with.
func (l *Log) current(b *binding) bool {
	return l.cfg.Load() == b.f && l.out.Gen() == b.gen
}

// Dropped counts the records lost: a full queue, a change of configuration
// or destination, or the end of the stop budget.
func (l *Log) Dropped() uint64 { return l.dropped.Load() }

// Close stops accepting records and writes what is queued until ctx ends;
// what is left then is dropped and counted, and no record is written after
// Close returned. A second Close returns at once.
func (l *Log) Close(ctx context.Context) {
	first := false
	l.once.Do(func() {
		first = true
		l.gate.Lock()
		l.closed = true
		l.gate.Unlock()
		close(l.stop)
	})
	if !first {
		return
	}
	select {
	case <-l.done:
	case <-ctx.Done():
		close(l.abort)
	}
}

func (l *Log) run() {
	defer close(l.done)
	for {
		select {
		case r := <-l.q:
			l.write(r)
		case <-l.stop:
			for {
				select {
				case r := <-l.q:
					l.write(r)
				default:
					return
				}
			}
		}
	}
}

func (l *Log) write(r record) {
	select {
	case <-l.abort:
		l.dropped.Add(1)
		return
	default:
	}
	if !l.current(r.b) {
		l.dropped.Add(1)
		return
	}
	dir := "received"
	if r.sent {
		dir = "sent"
	}
	if l.side == SideHub {
		dir = "worker->hub"
		if r.sent {
			dir = "hub->worker"
		}
	}
	rec := slog.NewRecord(r.at, slog.LevelInfo, "worker traffic", 0)
	rec.AddAttrs(slog.String("component", "traffic"), slog.String("direction", dir))
	if r.peer.WorkerID != "" {
		rec.AddAttrs(slog.String("worker_id", r.peer.WorkerID), slog.String("worker", r.peer.Name))
	}
	rec.AddAttrs(slog.String("stream", r.kind.String()), slog.String("frame_type", r.typ), slog.Int("bytes", r.size))
	if r.op != "" {
		rec.AddAttrs(slog.String("op", r.op))
	}
	if r.reqID != "" {
		rec.AddAttrs(slog.String("request_id", r.reqID))
	}
	if r.result {
		rec.AddAttrs(slog.Int64("duration_ms", r.duration.Milliseconds()))
		if r.outcome != "" {
			rec.AddAttrs(slog.String("outcome", r.outcome))
		}
	}
	if r.payload != "" {
		rec.AddAttrs(slog.String(logx.PayloadKey, r.payload))
	}
	// The remote destinations are RemoteSafe handlers like for any record.
	_ = r.b.handler.Handle(context.Background(), rec)
}

// Tap returns the frame tap of one connection.
func (l *Log) Tap(p Peer) transport.FrameTap {
	return &tap{l: l, peer: p, open: map[uint32]pending{}}
}

// ForSession implements transport.SessionTapper: every session of a
// reconnecting Worker starts with no open request.
func (t *tap) ForSession() transport.FrameTap { return t.l.Tap(t.peer) }

type pending struct {
	op, reqID string
	at        time.Time
}

type tap struct {
	l    *Log
	peer Peer
	mu   sync.Mutex
	open map[uint32]pending
}

// head is the part of a frame the log reads: never a body field.
type head struct {
	Type      string `json:"type"`
	Op        string `json:"op"`
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Result    struct {
		Outcome string `json:"outcome"`
	} `json:"result"`
}

// Bounds of the metadata a record keeps. The peer chooses these fields, so a
// value is kept only when it is short and made of identifier characters;
// anything else becomes "other" (op, type, outcome) or is left out (request
// id). A queued record is therefore a few hundred bytes at most.
const (
	maxTypeLen    = 32
	maxIDLen      = 64
	maxOutcomeLen = 48
)

// ident returns s when it is 1..max characters of [A-Za-z0-9._-], else
// other.
func ident(s string, max int, other string) string {
	if s == "" || len(s) > max {
		if s == "" {
			return ""
		}
		return other
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
			return other
		}
	}
	return s
}

func (t *tap) TapFrame(f transport.TapFrame) {
	if f.End {
		t.mu.Lock()
		delete(t.open, f.Stream)
		t.mu.Unlock()
		return
	}
	c := t.l.cfg.Load()
	if c == nil || !c.matches(t.peer) {
		return
	}
	var h head
	_ = json.Unmarshal(f.Body, &h)
	typ := ident(h.Type, maxTypeLen, "other")
	op := ident(h.Op, maxIDLen, "other")
	reqID := ident(h.RequestID, maxIDLen, "")
	r := record{at: t.l.now(), b: t.l.bind(c), peer: t.peer, sent: f.Sent, kind: f.Kind, typ: typ, size: f.Size}
	switch typ {
	case "op.request":
		r.op, r.reqID = op, reqID
		t.mu.Lock()
		if len(t.open) >= maxOpen {
			t.evictOldest()
		}
		t.open[f.Stream] = pending{op: op, reqID: reqID, at: r.at}
		t.mu.Unlock()
	case "op.result", "error":
		t.mu.Lock()
		p, ok := t.open[f.Stream]
		delete(t.open, f.Stream)
		t.mu.Unlock()
		if ok {
			r.op, r.reqID, r.result, r.duration = p.op, p.reqID, true, r.at.Sub(p.at)
		}
		r.outcome = ident(h.Result.Outcome, maxOutcomeLen, "other")
		if typ == "error" {
			r.outcome = ident(h.Code, maxOutcomeLen, "other")
		}
		if r.outcome == "" && typ == "op.result" {
			r.outcome = "ok"
		}
	case "op.progress":
		t.mu.Lock()
		p, ok := t.open[f.Stream]
		t.mu.Unlock()
		if ok {
			r.op, r.reqID = p.op, p.reqID
		}
	}
	if c.Payload {
		// The body is masked and cut here, once: the queue holds text that
		// may leave the machine, never the frame bytes.
		r.payload = logx.RedactPayload(f.Body, logx.RemotePayloadMax)
	}
	t.l.gate.RLock()
	defer t.l.gate.RUnlock()
	if t.l.closed {
		t.l.dropped.Add(1)
		return
	}
	select {
	case t.l.q <- r:
	default:
		t.l.dropped.Add(1)
	}
}

// evictOldest removes the oldest open request (the map is full).
func (t *tap) evictOldest() {
	var key uint32
	var at time.Time
	first := true
	for k, p := range t.open {
		if first || p.at.Before(at) {
			key, at, first = k, p.at, false
		}
	}
	delete(t.open, key)
}

func (f *filter) matches(p Peer) bool {
	if len(f.Workers) == 0 || p.WorkerID == "" {
		return true
	}
	return slices.Contains(f.Workers, p.WorkerID) || slices.Contains(f.Workers, p.Name)
}
