package logx

import (
	"context"
	"log/slog"
	"slices"
	"sync/atomic"
)

// Slots of a [Fanout]: one record, built once, goes to every slot whose level
// it meets.
const (
	// SlotLocal is the local output (stderr, the console of a service).
	SlotLocal = iota
	// SlotRing is the in-memory log of a Worker (worker_logs).
	SlotRing
	// SlotSyslog is the syslog sink.
	SlotSyslog
	// SlotOTel is the OpenTelemetry log sink.
	SlotOTel
	// NumSlots is the number of slots.
	NumSlots
)

// sinkEntry is the immutable content of a slot: a handler and the lowest
// level it receives.
type sinkEntry struct {
	h     slog.Handler
	level slog.Leveler
}

// Fanout is a handler that hands every record to up to [NumSlots] sinks, each
// with its own level (Serilog's restrictedToMinimumLevel, log4j2's AppenderRef
// level). A slot is swapped atomically with [Fanout.Set], without locks; a
// logger derived with With or WithGroup follows every later swap. Enabled is
// true when some slot wants the level, so a debug record is only built when a
// sink wants it.
type Fanout struct {
	slots *[NumSlots]atomic.Pointer[sinkEntry]
	// gen counts the slot changes (shared by derived Fanouts).
	gen *atomic.Uint64
	// chain is the WithAttrs and WithGroup history of a derived handler,
	// applied to a slot's handler on first use and cached per slot entry.
	chain []chainOp
	cache *[NumSlots]atomic.Pointer[derivedEntry]
}

type chainOp struct {
	attrs []slog.Attr
	group string
}

type derivedEntry struct {
	src *sinkEntry
	h   slog.Handler
}

// NewFanout returns a Fanout with every slot empty.
func NewFanout() *Fanout {
	return &Fanout{slots: new([NumSlots]atomic.Pointer[sinkEntry]), gen: new(atomic.Uint64), cache: new([NumSlots]atomic.Pointer[derivedEntry])}
}

// Set puts h into slot at level (a *slog.LevelVar lets the level change
// later); a nil h empties the slot. Records that are being handled while the
// slot changes go to the old or to the new handler, never to both.
func (f *Fanout) Set(slot int, h slog.Handler, level slog.Leveler) {
	defer f.gen.Add(1)
	if h == nil {
		f.slots[slot].Store(nil)
		return
	}
	if level == nil {
		level = slog.LevelInfo
	}
	f.slots[slot].Store(&sinkEntry{h: h, level: level})
}

// Handler returns the handler of slot as set (without the With history of a
// derived Fanout), or nil.
func (f *Fanout) Handler(slot int) slog.Handler {
	if e := f.slots[slot].Load(); e != nil {
		return e.h
	}
	return nil
}

// Local returns a handler over the local slots only (local output and ring):
// for records that must never reach a remote sink, such as the diagnostics of
// the remote sinks themselves.
func (f *Fanout) Local() slog.Handler { return localView{f} }

func (f *Fanout) wants(ctx context.Context, slot int, l slog.Level) (*sinkEntry, bool) {
	e := f.slots[slot].Load()
	if e == nil || l < e.level.Level() || !e.h.Enabled(ctx, l) {
		return nil, false
	}
	return e, true
}

// Enabled implements slog.Handler.
func (f *Fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for i := range NumSlots {
		if _, ok := f.wants(ctx, i, l); ok {
			return true
		}
	}
	return false
}

// Handle implements slog.Handler. The first error of a slot is returned after
// every slot had the record.
func (f *Fanout) Handle(ctx context.Context, rec slog.Record) error {
	return f.handle(ctx, rec, NumSlots)
}

func (f *Fanout) handle(ctx context.Context, rec slog.Record, upto int) error {
	var first error
	for i := range upto {
		e, ok := f.wants(ctx, i, rec.Level)
		if !ok {
			continue
		}
		if err := f.derived(i, e).Handle(ctx, rec.Clone()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// derived returns e's handler with this Fanout's With history applied.
func (f *Fanout) derived(slot int, e *sinkEntry) slog.Handler {
	if len(f.chain) == 0 {
		return e.h
	}
	if d := f.cache[slot].Load(); d != nil && d.src == e {
		return d.h
	}
	h := e.h
	for _, op := range f.chain {
		if op.group != "" {
			h = h.WithGroup(op.group)
		} else {
			// Each sink gets its own copy: a handler may keep or change
			// the slice it is given.
			h = h.WithAttrs(slices.Clone(op.attrs))
		}
	}
	f.cache[slot].Store(&derivedEntry{src: e, h: h})
	return h
}

func (f *Fanout) with(op chainOp) *Fanout {
	chain := make([]chainOp, len(f.chain), len(f.chain)+1)
	copy(chain, f.chain)
	return &Fanout{slots: f.slots, gen: f.gen, chain: append(chain, op), cache: new([NumSlots]atomic.Pointer[derivedEntry])}
}

// WithAttrs implements slog.Handler.
func (f *Fanout) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return f
	}
	return f.with(chainOp{attrs: append([]slog.Attr(nil), as...)})
}

// WithGroup implements slog.Handler.
func (f *Fanout) WithGroup(name string) slog.Handler {
	if name == "" {
		return f
	}
	return f.with(chainOp{group: name})
}

// localView is a Fanout restricted to the local slots.
type localView struct{ f *Fanout }

func (v localView) Enabled(ctx context.Context, l slog.Level) bool {
	for i := range SlotSyslog {
		if _, ok := v.f.wants(ctx, i, l); ok {
			return true
		}
	}
	return false
}

func (v localView) Handle(ctx context.Context, rec slog.Record) error {
	return v.f.handle(ctx, rec, SlotSyslog)
}

func (v localView) WithAttrs(as []slog.Attr) slog.Handler {
	return localView{v.f.WithAttrs(as).(*Fanout)}
}

func (v localView) WithGroup(name string) slog.Handler {
	return localView{v.f.WithGroup(name).(*Fanout)}
}

// Gen counts the slot changes: it differs whenever a slot was set since.
func (f *Fanout) Gen() uint64 { return f.gen.Load() }

// Bind returns a handler over the handlers that the given slots hold now,
// whatever their levels (a record class with a fixed level of its own, such
// as the Worker traffic log). It does not follow later changes: a record
// bound to a sink that was replaced since goes to the replaced sink (which
// drops it), never to its successor. A slot that is empty now and a slot not
// listed (the ring, for example) never see a record.
func (f *Fanout) Bind(slots ...int) slog.Handler {
	var hs []slog.Handler
	for _, i := range slots {
		if e := f.slots[i].Load(); e != nil {
			hs = append(hs, f.derived(i, e))
		}
	}
	return boundView(hs)
}

type boundView []slog.Handler

func (v boundView) Enabled(context.Context, slog.Level) bool { return true }

func (v boundView) Handle(ctx context.Context, rec slog.Record) error {
	var first error
	for _, h := range v {
		if err := h.Handle(ctx, rec.Clone()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (v boundView) WithAttrs(as []slog.Attr) slog.Handler {
	out := make(boundView, len(v))
	for i, h := range v {
		out[i] = h.WithAttrs(slices.Clone(as))
	}
	return out
}

func (v boundView) WithGroup(name string) slog.Handler {
	out := make(boundView, len(v))
	for i, h := range v {
		out[i] = h.WithGroup(name)
	}
	return out
}
