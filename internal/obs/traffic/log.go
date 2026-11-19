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
// when standard error or a sink stalls.
type Log struct {
	side Side
	out  *logx.Fanout
	now  func() time.Time

	cfg     atomic.Pointer[filter]
	q       chan record
	dropped atomic.Uint64
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
}

type filter struct {
	config.TrafficConfig
	handler slog.Handler
}

type record struct {
	at       time.Time
	handler  slog.Handler
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
}

// New returns a log that is off until Configure turns it on.
func New(side Side, out *logx.Fanout) *Log {
	l := &Log{side: side, out: out, now: time.Now, q: make(chan record, QueueSize), stop: make(chan struct{}), done: make(chan struct{})}
	go l.run()
	return l
}

// Configure applies c. The destinations are slots of the process's fan-out
// (local output, syslog, OpenTelemetry), never the in-memory log; the record
// level is info whatever the slots' levels.
func (l *Log) Configure(c config.TrafficConfig) {
	if !c.Enabled {
		l.cfg.Store(nil)
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
	l.cfg.Store(&filter{TrafficConfig: c, handler: l.out.Pick(slots...)})
}

// Dropped counts the records lost to a full queue.
func (l *Log) Dropped() uint64 { return l.dropped.Load() }

// Close stops the writer; records still queued when ctx ends are dropped.
func (l *Log) Close(ctx context.Context) {
	l.once.Do(func() { close(l.stop) })
	select {
	case <-l.done:
	case <-ctx.Done():
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
	// The remote destinations are RemoteSafe handlers like for any record.
	_ = r.handler.Handle(context.Background(), rec)
}

// Tap returns the frame tap of one connection.
func (l *Log) Tap(p Peer) transport.FrameTap {
	return &tap{l: l, peer: p, open: map[uint32]pending{}}
}

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
	r := record{at: t.l.now(), handler: c.handler, peer: t.peer, sent: f.Sent, kind: f.Kind, typ: h.Type, size: f.Size}
	switch h.Type {
	case "op.request":
		r.op, r.reqID = h.Op, h.RequestID
		t.mu.Lock()
		if len(t.open) >= maxOpen {
			for k := range t.open {
				delete(t.open, k)
				break
			}
		}
		t.open[f.Stream] = pending{op: h.Op, reqID: h.RequestID, at: r.at}
		t.mu.Unlock()
	case "op.result", "error":
		t.mu.Lock()
		p, ok := t.open[f.Stream]
		delete(t.open, f.Stream)
		t.mu.Unlock()
		if ok {
			r.op, r.reqID, r.result, r.duration = p.op, p.reqID, true, r.at.Sub(p.at)
		}
		r.outcome = h.Result.Outcome
		if h.Type == "error" {
			r.outcome = h.Code
		}
		if r.outcome == "" && h.Type == "op.result" {
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
	select {
	case t.l.q <- r:
	default:
		t.l.dropped.Add(1)
	}
}

func (f *filter) matches(p Peer) bool {
	if len(f.Workers) == 0 || p.WorkerID == "" {
		return true
	}
	return slices.Contains(f.Workers, p.WorkerID) || slices.Contains(f.Workers, p.Name)
}
