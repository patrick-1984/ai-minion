package logx

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// Default bounds of the in-memory log of a process ([NewRing]).
const (
	DefaultRingLines = 2000
	DefaultRingBytes = 512 << 10
	// maxRingLine bounds one stored line, in bytes.
	maxRingLine = 4096
)

// Ring keeps the most recent log lines of a process in memory, bounded by a
// number of lines and a number of bytes; the oldest lines go first. It is an
// io.Writer: every Write is one or more lines (a slog handler writes one
// record per call). It is safe for concurrent use.
type Ring struct {
	mu       sync.Mutex
	lines    []string // circular, len == maxLines once full
	start    int      // index of the oldest line
	n        int      // number of stored lines
	bytes    int      // total bytes of the stored lines
	maxLines int
	maxBytes int
	dropped  uint64 // lines pushed out since the start
}

// NewRing returns a ring of at most maxLines lines and maxBytes bytes; a value
// of zero or less means the default.
func NewRing(maxLines, maxBytes int) *Ring {
	if maxLines <= 0 {
		maxLines = DefaultRingLines
	}
	if maxBytes <= 0 {
		maxBytes = DefaultRingBytes
	}
	return &Ring{lines: make([]string, maxLines), maxLines: maxLines, maxBytes: maxBytes}
}

// Write stores every non-empty line of p. It never fails.
func (r *Ring) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		line = strings.TrimRight(line, "\r")
		if line != "" {
			r.Add(line)
		}
	}
	return len(p), nil
}

// ringCut marks a line that was cut to fit.
const ringCut = "...(cut)"

// Add stores one line, cut to a bounded length that also fits the byte budget
// of the ring (the marker included). The line is copied, so that a retained
// line never keeps the larger buffer it was cut from alive.
func (r *Ring) Add(line string) {
	limit := min(maxRingLine, r.maxBytes)
	switch {
	case len(line) <= limit:
		line = strings.Clone(line)
	case limit > len(ringCut):
		line = line[:limit-len(ringCut)] + ringCut
	default:
		line = strings.Clone(line[:limit])
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.n > 0 && (r.n == r.maxLines || r.bytes+len(line) > r.maxBytes) {
		r.bytes -= len(r.lines[r.start])
		r.lines[r.start] = ""
		r.start = (r.start + 1) % r.maxLines
		r.n--
		r.dropped++
	}
	r.lines[(r.start+r.n)%r.maxLines] = line
	r.n++
	r.bytes += len(line)
}

// Reset drops every stored line (the count of dropped lines grows by them).
func (r *Ring) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.lines {
		r.lines[i] = ""
	}
	r.dropped += uint64(r.n)
	r.start, r.n, r.bytes = 0, 0, 0
}

// Tail returns the last n lines, oldest first; n <= 0 or more than stored
// returns every stored line.
func (r *Ring) Tail(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > r.n {
		n = r.n
	}
	out := make([]string, 0, n)
	for i := r.n - n; i < r.n; i++ {
		out = append(out, r.lines[(r.start+i)%r.maxLines])
	}
	return out
}

// Stats returns the number of stored lines, their bytes and the number of
// lines pushed out since the start.
func (r *Ring) Stats() (lines, bytes int, dropped uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n, r.bytes, r.dropped
}

// NewRingHandler returns a handler that renders records as key=value text
// lines into r at the level of leveler. Values of type [Secret] render as
// "[redacted]" like in every other handler of this package; on top of that,
// because the Hub can read the ring, secret attributes (pairing_code, token,
// password, authorization, cookie, ...), pairing codes in text and the user
// information and query of URLs are masked before a line is stored.
func NewRingHandler(r *Ring, leveler slog.Leveler) slog.Handler {
	return redactHandler{slog.NewTextHandler(r, &slog.HandlerOptions{Level: leveler})}
}

// Tee returns a handler that hands every record to each handler that is
// enabled for its level. A nil handler is skipped.
func Tee(hs ...slog.Handler) slog.Handler {
	var out teeHandler
	for _, h := range hs {
		if h != nil {
			out = append(out, h)
		}
	}
	return out
}

type teeHandler []slog.Handler

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range t {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (t teeHandler) Handle(ctx context.Context, rec slog.Record) error {
	var first error
	for _, h := range t {
		if !h.Enabled(ctx, rec.Level) {
			continue
		}
		if err := h.Handle(ctx, rec.Clone()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (t teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithAttrs(as)
	}
	return out
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	out := make(teeHandler, len(t))
	for i, h := range t {
		out[i] = h.WithGroup(name)
	}
	return out
}

// Live is the runtime log control of a long-running process: the level of
// its local output, which debug mode (Worker) or the panel (Hub) changes at
// runtime, the in-memory log of its recent lines (Worker), and the [Fanout]
// whose remote slots (syslog, OpenTelemetry) have levels of their own.
type Live struct {
	// Level is the level of the local output and of the ring.
	Level *slog.LevelVar
	// Base is the configured level, restored when debug mode ends.
	Base slog.Level
	// Ring holds the recent lines (all levels the local output writes); nil
	// for a process without one (the Hub).
	Ring *Ring
	// Fanout is the handler of the logger: local output, ring and the
	// remote sinks, each with its own level.
	Fanout *Fanout

	mu sync.Mutex
}

// SetDebug switches debug mode: on lowers the level to debug, off restores
// the configured level (which stays debug when the process was started so).
// It changes the local output and the ring only: a remote sink keeps its own
// level, so remote debug cannot raise what leaves the machine.
func (l *Live) SetDebug(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if on {
		l.Level.Set(min(l.Base, slog.LevelDebug))
		return
	}
	l.Level.Set(l.Base)
}

// SetBase changes the configured level and applies it (the Hub's live
// log_level).
func (l *Live) SetBase(level slog.Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Base = level
	l.Level.Set(level)
}

// Debug reports whether the logger writes debug lines now.
func (l *Live) Debug() bool { return l.Level.Level() <= slog.LevelDebug }

// NewLive is [New] with a runtime level and an in-memory ring of the recent
// lines: the logger writes to opts.Output and to the ring, both at the level
// that [Live.SetDebug] switches. The remote slots of [Live.Fanout] start
// empty.
func NewLive(opts Options, ring *Ring) (*slog.Logger, *Live, error) {
	if ring == nil {
		ring = NewRing(0, 0)
	}
	return newLive(opts, ring)
}

// NewLiveLocal is [NewLive] without a ring (the Hub): the local output at a
// runtime level plus empty remote slots.
func NewLiveLocal(opts Options) (*slog.Logger, *Live, error) {
	return newLive(opts, nil)
}

func newLive(opts Options, ring *Ring) (*slog.Logger, *Live, error) {
	level, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, nil, err
	}
	lv := new(slog.LevelVar)
	lv.Set(level)
	opts.leveler = lv
	base, err := New(opts)
	if err != nil {
		return nil, nil, err
	}
	f := NewFanout()
	f.Set(SlotLocal, base.Handler(), lv)
	if ring != nil {
		f.Set(SlotRing, NewRingHandler(ring, lv), lv)
	}
	l := &Live{Level: lv, Base: level, Ring: ring, Fanout: f}
	return slog.New(f), l, nil
}

// Dedup writes a recurring debug line only when its summary changes, so that
// a poll every second does not fill the log (and the ring) with the same
// line. It is safe for concurrent use; the zero value is ready.
type Dedup struct {
	mu      sync.Mutex
	last    string
	skipped int
}

// Debug logs msg with args at debug level unless key equals the key of the
// previous call; the line says how many identical results it replaces.
func (d *Dedup) Debug(log *slog.Logger, key, msg string, args ...any) {
	if log == nil || !log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	d.mu.Lock()
	if key == d.last {
		d.skipped++
		d.mu.Unlock()
		return
	}
	skipped := d.skipped
	d.last, d.skipped = key, 0
	d.mu.Unlock()
	if skipped > 0 {
		args = append(args, "unchanged_before", skipped)
	}
	log.Debug(msg, args...)
}

var _ io.Writer = (*Ring)(nil)
