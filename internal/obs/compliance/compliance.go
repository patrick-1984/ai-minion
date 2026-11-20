// Package compliance sends session content to the OpenTelemetry log sink as
// minion.session.* events (setting compliance_monitoring). It is a thin
// emitter: the capture points (the Hub's Worker router and pipe broker) call
// Monitor.Emit, which masks Minion's own secret formats, splits long text into
// bounded chunks and hands them to the sink that is current at that moment.
// It never blocks: the sink's own bounded queue drops what does not fit.
package compliance

import (
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Event names (OTLP log record event names).
const (
	EventAction = "minion.session.action"
	EventOutput = "minion.session.output"
	EventInput  = "minion.session.input"
)

// ChunkBytes bounds the body of one event; longer text is split.
const ChunkBytes = 8 << 10

// Mode is the level of compliance monitoring, ordered.
type Mode int32

// The levels.
const (
	Off Mode = iota
	Actions
	Output
	Full
)

// ParseMode maps the setting value to a Mode; anything unknown is Off.
func ParseMode(s string) Mode {
	switch s {
	case config.ComplianceActions:
		return Actions
	case config.ComplianceOutput:
		return Output
	case config.ComplianceFull:
		return Full
	}
	return Off
}

// String is the setting value of m.
func (m Mode) String() string {
	return [...]string{config.ComplianceOff, config.ComplianceActions, config.ComplianceOutput, config.ComplianceFull}[m]
}

// KV is one attribute of an event: the value is a string, int64 or bool.
type KV struct {
	Key   string
	Value any
}

// Sink takes one event. It never blocks. *otel.Logs implements it.
type Sink interface {
	EmitEvent(name string, at time.Time, body string, attrs []KV)
}

// Monitor holds the current mode and finds the sink of an observation.
type Monitor struct {
	// state packs a generation (high bits) and the mode (low byte): every
	// SetMode changes it, so an observation can tell that the configuration
	// changed while it looked up its sink.
	state atomic.Uint64
	// source returns the current sink, nil when there is none or the sink's
	// level filters INFO.
	source func() Sink
	// origin says where the setting came from when that is not the panel
	// ("the environment"); shown in the status.
	origin atomic.Pointer[string]
	now    func() time.Time
}

// New returns a Monitor that is off. source is asked once per observation.
func New(source func() Sink) *Monitor {
	return &Monitor{source: source, now: time.Now}
}

// SetMode sets the effective mode.
func (m *Monitor) SetMode(md Mode) {
	if m == nil {
		return
	}
	for {
		old := m.state.Load()
		if m.state.CompareAndSwap(old, ((old>>8)+1)<<8|uint64(md)) {
			return
		}
	}
}

// SetOrigin records where the mode was set when it was not the settings panel.
func (m *Monitor) SetOrigin(o string) {
	if m != nil {
		m.origin.Store(&o)
	}
}

// Mode returns the effective mode (Off for a nil Monitor).
func (m *Monitor) Mode() Mode {
	if m == nil {
		return Off
	}
	return Mode(m.state.Load() & 0xff)
}

// Status is the mode as the sink status shows it: with a note when the sink's
// level filters the events.
func (m *Monitor) Status() string {
	md := m.Mode()
	if md == Off {
		return ""
	}
	st := md.String()
	if o := m.origin.Load(); o != nil && *o != "" {
		st += " (set by " + *o + ")"
	}
	if m.source == nil || m.source() == nil {
		st += " (not sent: otel_level filters info events)"
	}
	return st
}

// Emit records one observation if the mode is at least min. text, when not
// empty, is masked with Minion's secret formats and then split into chunks of
// at most ChunkBytes, each an event with the observation id, chunk index and
// chunk count. All chunks go to the sink found at the start.
func (m *Monitor) Emit(min Mode, event string, text string, attrs ...KV) {
	if m == nil || m.source == nil {
		return
	}
	// One snapshot: the mode, then the sink, then the mode again. If the
	// configuration changed in between (a retarget, a level change), the
	// record is dropped rather than sent to a sink the mode did not admit.
	before := m.state.Load()
	if Mode(before&0xff) < min {
		return
	}
	sink := m.source()
	if sink == nil || m.state.Load() != before {
		return
	}
	attrs = sanitize(attrs)
	at := m.now()
	if text == "" {
		sink.EmitEvent(event, at, "", attrs)
		return
	}
	chunks := Chunks(maskText(text), ChunkBytes)
	id := ops.NewUUIDv7()
	for i, c := range chunks {
		a := append(append([]KV(nil), attrs...),
			KV{"minion.observation.id", id}, KV{"minion.chunk.index", int64(i)}, KV{"minion.chunk.count", int64(len(chunks))})
		sink.EmitEvent(event, at, c, a)
	}
}

// MaxAttrBytes bounds a string attribute of an event.
const MaxAttrBytes = 256

// sanitize masks and bounds every string attribute (session names and
// outcomes come from callers and Workers, not from Minion).
func sanitize(in []KV) []KV {
	out := make([]KV, len(in))
	for i, a := range in {
		if v, ok := a.Value.(string); ok {
			a.Value = logx.CapText(logx.RedactText(strings.ToValidUTF8(v, "\uFFFD")), MaxAttrBytes)
		}
		out[i] = a
	}
	return out
}

// maskText masks Minion's secret formats. A terminal screen holds physical
// rows, so a secret that wrapped is split by a line break, and a secret at the
// start of a row follows a letter once the rows are joined. Both are found
// first, on the rows as they are and on the rows joined, and every span either
// search found is masked together (so one never undoes the other); URL and
// other text patterns are masked after.
func maskText(text string) string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	spans := logx.SecretSpans(text)
	if strings.Contains(text, "\n") {
		flat := make([]byte, 0, len(text))
		at := make([]int, 0, len(text)) // at[k]: where byte k of the joined text was
		for i := 0; i < len(text); i++ {
			if text[i] != '\n' && text[i] != '\r' {
				flat = append(flat, text[i])
				at = append(at, i)
			}
		}
		for _, sp := range logx.SecretSpans(string(flat)) {
			spans = append(spans, [2]int{at[sp[0]], at[sp[1]-1] + 1})
		}
	}
	if len(spans) == 0 {
		return logx.RedactText(text)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	var b strings.Builder
	pos := 0
	for _, sp := range spans {
		if sp[1] <= pos {
			continue
		}
		if sp[0] > pos {
			b.WriteString(text[pos:sp[0]])
		}
		b.WriteString(logx.Redacted)
		pos = sp[1]
	}
	b.WriteString(text[pos:])
	return logx.RedactText(b.String())
}

// Chunks splits s into pieces of at most max bytes, each valid UTF-8 (a cut
// never falls inside a rune), copying the bytes it keeps.
func Chunks(s string, max int) []string {
	var out []string
	for len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			cut = max
		}
		out = append(out, string([]byte(s[:cut])))
		s = s[cut:]
	}
	return append(out, string([]byte(s)))
}
