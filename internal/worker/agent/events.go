package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Timing of the event stream.
const (
	// DefaultReopenDelay is the wait before the Worker opens a new event
	// stream after the Hub closed or broke the previous one on a live link.
	DefaultReopenDelay = 2 * time.Second
)

// SnapshotFrame is the "events.snapshot" frame, the first frame of every
// event stream (AD-3): the shared sessions with kind and state, and no screen
// content. Seq is 0; the events that follow count from 1.
type SnapshotFrame struct {
	Type     string         `json:"type"`
	Seq      uint64         `json:"seq"`
	Time     ops.Timestamp  `json:"time"`
	Sessions []SessionState `json:"sessions"`
	// Share is the share rule (with its revision) that Sessions was filtered
	// by. It is additive: the Hub keeps the rule, not only the sessions, so that
	// it can tell for a session the list does not name (closed, offline or
	// unshared) whether what it stored of it may be shown. Absent from a Worker
	// that predates it.
	Share *ops.ShareDecl `json:"share,omitempty"`
}

// EventFrame is an "event" frame. Seq grows by exactly one per frame on one
// event stream, so the Hub detects a gap and reopens the stream. Event is one
// of the Event* names. Kind, State and Task are set for session_added and
// state_changed (a change of the task is a state_changed), ScreenHash for
// session_added and screen_changed; a session_removed names only the session.
//
// Task is a pointer so that the wire tells "no task" from "not reported": on
// session_added and state_changed it is always present, as "" when the
// session has no task (a cleared task), and on the other events it is absent.
type EventFrame struct {
	Type       string        `json:"type"`
	Seq        uint64        `json:"seq"`
	Time       ops.Timestamp `json:"time"`
	Event      string        `json:"event"`
	Session    string        `json:"session"`
	Kind       string        `json:"kind,omitempty"`
	State      string        `json:"state,omitempty"`
	Task       *string       `json:"task,omitempty"`
	ScreenHash string        `json:"screen_hash,omitempty"`
	// Share is the share rule now in force and its revision. It is set on the
	// first event of a look in which the rule changed (or on a share_changed
	// event of its own, which names no session, when the look has no other
	// event) and comes before every event the new rule makes visible, on the
	// same ordered stream.
	Share *ops.ShareDecl `json:"share,omitempty"`
}

// EventShareChanged is the event that carries a new share rule. It is not
// part of [Diff]: the publisher sends it itself, ahead of the session events
// of the same look.
const EventShareChanged = "share_changed"

// eventFrameOf builds the frame of a change.
func eventFrameOf(seq uint64, at time.Time, c Change) EventFrame {
	f := EventFrame{Type: proto.TypeEvent, Seq: seq, Time: ops.Timestamp(at), Event: c.Event, Session: c.Session.Name}
	switch c.Event {
	case EventSessionAdded:
		task := c.Session.Task
		f.Kind, f.State, f.Task, f.ScreenHash = c.Session.Kind, c.Session.State, &task, c.Session.ScreenHash
	case EventStateChanged:
		task := c.Session.Task
		f.Kind, f.State, f.Task = c.Session.Kind, c.Session.State, &task
	case EventScreenChanged:
		f.ScreenHash = c.Session.ScreenHash
	}
	return f
}

// EventStream is one event stream as the publisher uses it. *transport.Stream
// implements it; tests use a fake.
type EventStream interface {
	ReadFrame() (json.RawMessage, error)
	WriteFrame(v any) error
	Close() error
	Reset()
	SetWriteDeadline(t time.Time) error
}

var _ EventStream = (*transport.Stream)(nil)

// EventOpener opens the event stream of a link.
type EventOpener func(ctx context.Context) (EventStream, error)

// openerOf is the opener of a session: its OpenEvents.
func openerOf(s Session) EventOpener {
	return func(ctx context.Context) (EventStream, error) {
		st, err := s.OpenEvents(ctx)
		if err != nil {
			return nil, err
		}
		return st, nil
	}
}

// EventConfig configures an [EventPublisher]. Observer is required.
type EventConfig struct {
	Observer Observer
	// Interval is the poll interval; zero means [DefaultPollInterval].
	Interval time.Duration
	// Clock is the time source of the poll and the reopen wait; nil means the
	// system clock.
	Clock transport.Clock
	// ReopenDelay is the wait before a new stream is opened on a live link;
	// zero means [DefaultReopenDelay].
	ReopenDelay time.Duration
	// WriteTimeout bounds one frame write; zero means [DefaultWriteTimeout].
	WriteTimeout time.Duration
	// OnFrame receives every frame the Hub sends on the event stream, with
	// its type. Nil ignores them. A module that does not build the publisher
	// registers with [EventPublisher.OnFrame] instead.
	OnFrame func(frameType string, body json.RawMessage)
	// Logger receives operational messages: never terminal content or
	// secrets (AD-20). Nil discards them.
	Logger *slog.Logger
}

// EventPublisher publishes the Worker's session events to the Hub (AD-3). For
// every welcomed link it opens one event stream and sends events.snapshot
// first, so the Hub's picture of this Worker is replaced atomically after
// each (re)connect, then an event for every change it sees, with seq rising by
// one per frame. The stream only carries what the [Observer] shows, which is
// the shared sessions: an unshared session appears in no frame.
//
// If the Hub closes or breaks the stream while the link lives (a gap in seq
// makes it do so), the publisher opens a new one after [EventConfig.ReopenDelay]
// and starts again with a snapshot and seq 0.
type EventPublisher struct {
	cfg EventConfig
	log *slog.Logger
	clk transport.Clock

	mu       sync.Mutex
	handlers map[string][]func(body json.RawMessage)
}

// OnFrame registers fn for the frames of type frameType that the Hub sends on
// the event stream (for example proto.TypeTurnAck, the acknowledgement of the
// pipe in Story 7.4). It is how a later module receives inbound frames
// without editing this file or the module that builds the publisher: it takes
// the publisher with Require[*EventPublisher](env) and registers.
//
// It is safe to call before or while the publisher serves, from any
// goroutine; a handler registered while a stream runs sees the frames that
// arrive after the registration, and one registered once stays for every later
// stream and link. fn runs on the stream's reader goroutine, so it must not
// block for long; body is the whole frame and is only valid during the call
// unless fn copies it. Several handlers for one type run in registration
// order.
func (p *EventPublisher) OnFrame(frameType string, fn func(body json.RawMessage)) {
	if fn == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.handlers == nil {
		p.handlers = map[string][]func(json.RawMessage){}
	}
	p.handlers[frameType] = append(p.handlers[frameType], fn)
}

// dispatch hands a frame from the Hub to the configured and registered
// handlers.
func (p *EventPublisher) dispatch(frameType string, body json.RawMessage) {
	if p.cfg.OnFrame != nil {
		p.cfg.OnFrame(frameType, body)
	}
	p.mu.Lock()
	hs := slices.Clone(p.handlers[frameType])
	p.mu.Unlock()
	for _, h := range hs {
		h(body)
	}
}

// NewEventPublisher validates cfg and returns the publisher.
func NewEventPublisher(cfg EventConfig) (*EventPublisher, error) {
	if cfg.Observer == nil {
		return nil, errors.New("agent: the event publisher needs an observer")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultPollInterval
	}
	if cfg.ReopenDelay <= 0 {
		cfg.ReopenDelay = DefaultReopenDelay
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	p := &EventPublisher{cfg: cfg, log: cfg.Logger, clk: cfg.Clock}
	if p.log == nil {
		p.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if p.clk == nil {
		p.clk = transport.SystemClock
	}
	return p, nil
}

// Serve publishes events on one link until the link ends. It is a
// [LinkHandler].
func (p *EventPublisher) Serve(l *Link) { p.serve(l.Context(), openerOf(l.Session)) }

func (p *EventPublisher) serve(ctx context.Context, open EventOpener) {
	for ctx.Err() == nil {
		// Look before the stream opens: a snapshot of a failed look would
		// tell the Hub that every session is gone.
		view, decl, ok := p.observe(ctx)
		if !ok {
			if !p.wait(ctx, p.cfg.Interval) {
				return
			}
			continue
		}
		st, err := open(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.log.Debug("cannot open the event stream", "error", err.Error())
		} else {
			if p.run(ctx, st, view, decl) {
				return
			}
		}
		if !p.wait(ctx, p.cfg.ReopenDelay) {
			return
		}
	}
}

// ShareObserver is an [Observer] that also reports the share rule its View was
// filtered by, and can read that rule alone, cheaply. The publisher declares
// the rule to the Hub on the stream (the first frame and every change), so the
// Hub keeps the Worker's current rule and revision and does not have to infer
// what is shared from which sessions happen to be listed. [HostObserver]
// implements it.
type ShareObserver interface {
	Observer
	// ObserveShare is Observe together with the share state that filtered the
	// view; the view holds exactly the sessions that state shares.
	ObserveShare(ctx context.Context) (View, ops.ShareDecl, error)
	// ShareDecl reads only the share state.
	ShareDecl(ctx context.Context) (ops.ShareDecl, error)
}

// observe takes one look and logs a failure at debug level (a failing tmux
// must not fill the log every second). The declaration is nil for an Observer
// that does not report the share rule.
func (p *EventPublisher) observe(ctx context.Context) (View, *ops.ShareDecl, bool) {
	var (
		v    View
		d    *ops.ShareDecl
		err  error
		decl ops.ShareDecl
	)
	if so, ok := p.cfg.Observer.(ShareObserver); ok {
		v, decl, err = so.ObserveShare(ctx)
		d = &decl
	} else {
		v, err = p.cfg.Observer.Observe(ctx)
	}
	if err != nil {
		if ctx.Err() == nil {
			p.log.Debug("cannot read the sessions for the event stream", "error", err.Error())
		}
		return nil, nil, false
	}
	return v, d, true
}

// sameDecl reports whether two declarations are the same rule and revision.
func sameDecl(a, b ops.ShareDecl) bool {
	return a.Revision == b.Revision && a.Rule.Mode == b.Rule.Mode && slices.Equal(a.Rule.Names, b.Rule.Names)
}

func (p *EventPublisher) wait(ctx context.Context, d time.Duration) bool {
	select {
	case <-p.clk.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// run publishes on one stream and reports whether the link is over. It
// returns false when only the stream ended and a new one should be opened.
func (p *EventPublisher) run(ctx context.Context, st EventStream, view View, declared *ops.ShareDecl) (linkDone bool) {
	var seq uint64
	snap := SnapshotFrame{Type: proto.TypeEventsSnap, Time: ops.Timestamp(p.clk.Now()), Sessions: view.Sorted(), Share: declared}
	if err := p.write(st, snap); err != nil {
		st.Reset()
		return ctx.Err() != nil
	}

	gone := make(chan struct{})
	go p.drain(st, gone)

	so, _ := p.cfg.Observer.(ShareObserver)
	for {
		select {
		case <-ctx.Done():
			_ = st.Close()
			return true
		case <-gone:
			// The Hub ended the stream; the link may still live.
			st.Reset()
			return ctx.Err() != nil
		case <-p.clk.After(p.cfg.Interval):
		}
		cur, d, ok := p.observe(ctx)
		if !ok && so != nil && declared != nil {
			// The sessions cannot be listed (a failing terminal), but a change
			// of what is shared does not depend on them: read the rule alone, so
			// that it still reaches the Hub.
			if rd, err := so.ShareDecl(ctx); err == nil {
				d = &rd
			}
		}
		// A new rule travels ahead of the events of the look that used it: on
		// the first of them (a field of that frame), or on a frame of its own
		// when the look has no event. The Hub applies the rule first, so events
		// the rule hides are dropped there and events it shows are kept.
		var newRule *ops.ShareDecl
		if d != nil && declared != nil && !sameDecl(*declared, *d) {
			newRule = d
		}
		var changes []Change
		if ok {
			changes = Diff(view, cur)
		}
		if newRule != nil && len(changes) == 0 {
			seq++
			f := EventFrame{Type: proto.TypeEvent, Seq: seq, Time: ops.Timestamp(p.clk.Now()), Event: EventShareChanged, Share: newRule}
			if err := p.write(st, f); err != nil {
				st.Reset()
				return ctx.Err() != nil
			}
			declared, newRule = newRule, nil
		}
		for _, c := range changes {
			seq++
			f := eventFrameOf(seq, p.clk.Now(), c)
			if newRule != nil {
				f.Share = newRule
			}
			if err := p.write(st, f); err != nil {
				st.Reset()
				return ctx.Err() != nil
			}
			if newRule != nil {
				declared, newRule = newRule, nil
			}
		}
		if ok {
			view = cur
		}
	}
}

func (p *EventPublisher) write(st EventStream, v any) error {
	_ = st.SetWriteDeadline(time.Now().Add(p.cfg.WriteTimeout))
	return st.WriteFrame(v)
}

// drain reads what the Hub sends on the event stream, dispatches it to the
// frame handlers and closes gone when the stream ends.
func (p *EventPublisher) drain(st EventStream, gone chan<- struct{}) {
	defer close(gone)
	for {
		body, err := st.ReadFrame()
		if err != nil {
			return
		}
		if typ, err := proto.FrameType(body); err == nil {
			p.dispatch(typ, body)
		}
	}
}
