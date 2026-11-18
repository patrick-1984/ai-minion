// Package otel exports log records to an OpenTelemetry collector over
// OTLP/HTTP (protobuf): sdk/log with a batch processor, the otlploghttp
// exporter built with every option explicit, and the otelslog bridge behind
// logx.RemoteSafe. It also routes the SDK's own errors and log lines to a
// local-only, rate-limited diagnostics handler.
//
// Owner: A49 PR 1 (T6).
package otel

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/go-logr/logr"
	gotel "go.opentelemetry.io/otel"

	"github.com/patrick-1984/ai-minion/internal/logx"
)

// DiagInterval is the shortest time between two diagnostic lines of one
// (sink, class) pair.
const DiagInterval = time.Minute

// Diag writes the diagnostics of the remote sinks (connection, export and
// SDK errors) to a local-only handler: one line per (sink, class) per
// [DiagInterval], with the number of suppressed occurrences. Error texts go
// through logx.ErrorText (URLs cut to scheme and host). It never writes to a
// remote sink, so a failing sink cannot feed itself.
type Diag struct {
	log *slog.Logger
	now func() time.Time

	mu    sync.Mutex
	state map[[2]string]*diagState
}

type diagState struct {
	last       time.Time
	suppressed int
}

// NewDiag returns diagnostics that write to local, which must be a handler
// without remote slots (logx.Fanout.Local).
func NewDiag(local slog.Handler) *Diag {
	return &Diag{log: slog.New(local), now: time.Now, state: map[[2]string]*diagState{}}
}

// Report writes one diagnostic line unless the pair (sink, class) was
// reported less than DiagInterval ago; then it is counted and the count is
// shown with the next line.
func (d *Diag) Report(sink, class string, err error) {
	if d == nil || err == nil {
		return
	}
	k := [2]string{sink, class}
	now := d.now()
	d.mu.Lock()
	st := d.state[k]
	if st == nil {
		st = &diagState{}
		d.state[k] = st
	}
	if !st.last.IsZero() && now.Sub(st.last) < DiagInterval {
		st.suppressed++
		d.mu.Unlock()
		return
	}
	suppressed := st.suppressed
	st.last, st.suppressed = now, 0
	d.mu.Unlock()
	args := []any{"sink", sink, "class", class, "error", logx.ErrorText(err)}
	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	d.log.Log(logx.LocalOnly(context.Background()), slog.LevelWarn, "log export problem", args...)
}

var installMu sync.Mutex

// InstallGlobal routes the OpenTelemetry SDK's error handler and internal
// logger to d. Call it before any exporter is built: the SDK reports through
// these globals, and the default error handler writes to the standard logger
// (stderr) without rate limit.
func InstallGlobal(d *Diag) {
	installMu.Lock()
	defer installMu.Unlock()
	gotel.SetErrorHandler(gotel.ErrorHandlerFunc(func(err error) { d.Report("otel", "sdk", err) }))
	gotel.SetLogger(logr.FromSlogHandler(sdkLogHandler{d: d}))
}

// sdkLogHandler turns the SDK's internal log lines (warnings such as dropped
// records) into rate-limited diagnostics keyed by message; its info and
// debug lines are dropped. The SDK logs through logr: Error is an slog error,
// a warning is V(1) (slog level -1), info V(4) and debug V(8). Attributes are
// not copied (they may hold endpoint details).
type sdkLogHandler struct{ d *Diag }

// sdkWarnLevel is the slog level of the SDK's logr V(1) warnings.
const sdkWarnLevel = slog.Level(-1)

func (h sdkLogHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= sdkWarnLevel }

func (h sdkLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.d.Report("otel", r.Message, sdkError(r.Message))
	return nil
}

func (h sdkLogHandler) WithAttrs(as []slog.Attr) slog.Handler { return h }
func (h sdkLogHandler) WithGroup(string) slog.Handler         { return h }

type sdkError string

func (e sdkError) Error() string { return string(e) }
