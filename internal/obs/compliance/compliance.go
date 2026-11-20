// Package compliance sends session content to the OpenTelemetry log sink as
// minion.session.* events (setting compliance_monitoring). It is a thin
// emitter: the capture points (the Hub's Worker router and pipe broker) call
// Monitor.Emit, which masks Minion's own secret formats, splits long text into
// bounded chunks and hands them to the sink that is current at that moment.
// It never blocks: the sink's own bounded queue drops what does not fit.
package compliance

import (
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
	mode atomic.Int32
	// source returns the current sink, nil when there is none or the sink's
	// level filters INFO.
	source func() Sink
	now    func() time.Time
}

// New returns a Monitor that is off. source is asked once per observation.
func New(source func() Sink) *Monitor {
	return &Monitor{source: source, now: time.Now}
}

// SetMode sets the effective mode.
func (m *Monitor) SetMode(md Mode) { m.mode.Store(int32(md)) }

// Mode returns the effective mode (Off for a nil Monitor).
func (m *Monitor) Mode() Mode {
	if m == nil {
		return Off
	}
	return Mode(m.mode.Load())
}

// Status is the mode as the sink status shows it: with a note when the sink's
// level filters the events.
func (m *Monitor) Status() string {
	md := m.Mode()
	if md == Off {
		return ""
	}
	if m.source() == nil {
		return md.String() + " (not sent: otel_level filters info events)"
	}
	return md.String()
}

// Emit records one observation if the mode is at least min. text, when not
// empty, is masked with Minion's secret formats and then split into chunks of
// at most ChunkBytes, each an event with the observation id, chunk index and
// chunk count. All chunks go to the sink found at the start.
func (m *Monitor) Emit(min Mode, event string, text string, attrs ...KV) {
	if m.Mode() < min || m.source == nil {
		return
	}
	sink := m.source()
	if sink == nil {
		return
	}
	at := m.now()
	if text == "" {
		sink.EmitEvent(event, at, "", attrs)
		return
	}
	chunks := Chunks(logx.RedactText(strings.ToValidUTF8(text, "\uFFFD")), ChunkBytes)
	id := ops.NewUUIDv7()
	for i, c := range chunks {
		a := append(append([]KV(nil), attrs...),
			KV{"minion.observation.id", id}, KV{"minion.chunk.index", int64(i)}, KV{"minion.chunk.count", int64(len(chunks))})
		sink.EmitEvent(event, at, c, a)
	}
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
