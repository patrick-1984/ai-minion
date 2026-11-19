package obs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/obs/otel"
	"github.com/patrick-1984/ai-minion/internal/obs/syslog"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Sink names, as the status reports them.
const (
	SinkSyslog = "syslog"
	SinkOTel   = "otel"
)

// Process describes the exporting process (service name, version, ids).
type Process = otel.Process

// Manager owns the remote log sinks of one process: it builds them from a
// validated configuration, puts their handlers into the remote slots of the
// process's logx.Fanout, swaps them atomically when the configuration
// changes, reports their state and shuts them down. It is safe for
// concurrent use; Apply calls are serialised.
type Manager struct {
	fanout *logx.Fanout
	diag   *otel.Diag
	proc   Process

	mu     sync.Mutex
	sys    *syslogPipe
	otl    *otelPipe
	gens   map[string]uint64
	carry  map[string]map[string]uint64 // dropped counts of discarded pipelines
	errs   map[string]string            // last build error per sink
	warned string                       // the warnings last written
	closed bool
}

type syslogPipe struct {
	cfg config.SyslogConfig
	w   *syslog.Writer
}

type otelPipe struct {
	cfg  config.OTLPConfig
	logs *otel.Logs
}

// New returns a Manager for f. It routes the OpenTelemetry SDK's diagnostics
// to the local slots of f (rate-limited) before any exporter exists.
func New(f *logx.Fanout, proc Process) *Manager {
	diag := otel.NewDiag(f.Local())
	otel.InstallGlobal(diag)
	return &Manager{
		fanout: f, diag: diag, proc: proc,
		gens: map[string]uint64{}, carry: map[string]map[string]uint64{}, errs: map[string]string{},
	}
}

// SetProcess changes the identity used for pipelines built from now on (a
// Worker learns its id from the enrollment after the logger exists).
func (m *Manager) SetProcess(p Process) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proc = p
}

// Apply makes s the effective configuration. A sink whose configuration did
// not change keeps running. A changed sink is built new, swapped into its
// slot, and the old pipeline is discarded with its queue (counted as
// reconfigure): records queued under an old endpoint or credential are never
// sent with the new one. A sink that cannot be built is left off and its
// error returned (and shown in the status). The warnings of s are written to
// the local log.
func (m *Manager) Apply(ctx context.Context, s config.Sinks) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("obs: the log sinks are shut down")
	}
	// The same configuration applied again (the environment first, then
	// the panel) does not repeat its warnings. The lines are queued: a
	// blocked stderr never holds a change (or the lock).
	if joined := strings.Join(s.Warnings, "\n"); joined != m.warned {
		m.warned = joined
		for _, w := range s.Warnings {
			m.diag.Notice(slog.LevelWarn, "log export: "+w)
		}
	}
	return errors.Join(m.applySyslog(s.Syslog), m.applyOTel(ctx, s.OTLP))
}

// applySyslog builds the new pipeline first and then publishes it with one
// slot update, so that no record falls into an empty slot during a change;
// only then is the old pipeline retired. A pipeline that cannot be built
// leaves the sink off (fail closed): the old one is retired all the same.
func (m *Manager) applySyslog(c config.SyslogConfig) error {
	old := m.sys
	if old != nil && c.Enabled && old.cfg.Digest == c.Digest {
		return nil
	}
	if old == nil && !c.Enabled {
		delete(m.errs, SinkSyslog)
		return nil
	}
	delete(m.errs, SinkSyslog)
	m.gens[SinkSyslog]++
	var next *syslogPipe
	var h slog.Handler
	var level slog.Level
	var err error
	if c.Enabled {
		level, err = logx.ParseLevel(c.Level)
		if err == nil {
			host, _ := os.Hostname()
			f := &syslog.Formatter{Hostname: host, AppName: m.proc.Service, ProcID: strconv.Itoa(os.Getpid()), MaxBytes: c.MaxBytes}
			opts := syslog.Options{Network: c.Network, Addr: c.Addr,
				Diag: func(class string, err error) { m.diag.Report(SinkSyslog, class, err) }}
			if c.Network == "tls" {
				opts.TLS = c.TLSConfig()
			}
			w := syslog.NewWriter(opts)
			next = &syslogPipe{cfg: c, w: w}
			h = syslog.NewHandler(f, w)
		}
	}
	m.sys = next
	m.fanout.Set(logx.SlotSyslog, h, level)
	if old != nil {
		old.w.Discard()
		m.carryOver(SinkSyslog, old.w.Stats().Dropped)
	}
	if err != nil {
		m.errs[SinkSyslog] = err.Error()
	}
	return err
}

// applyOTel is applySyslog for the OpenTelemetry sink.
func (m *Manager) applyOTel(ctx context.Context, c config.OTLPConfig) error {
	old := m.otl
	if old != nil && c.Enabled && old.cfg.Digest == c.Digest {
		return nil
	}
	if old == nil && !c.Enabled {
		delete(m.errs, SinkOTel)
		return nil
	}
	delete(m.errs, SinkOTel)
	m.gens[SinkOTel]++
	var next *otelPipe
	var h slog.Handler
	var level slog.Level
	var err error
	if c.Enabled {
		level, err = logx.ParseLevel(c.Level)
		if err == nil {
			res, rerr := otel.NewResource(ctx, m.proc)
			if rerr != nil {
				m.diag.Report(SinkOTel, "resource", rerr)
			}
			var logs *otel.Logs
			logs, err = otel.NewLogs(ctx, c, res)
			if err != nil {
				err = fmt.Errorf("otel log sink: %s", logx.ErrorText(err))
			} else {
				next = &otelPipe{cfg: c, logs: logs}
				h = logs.Handler()
			}
		}
	}
	m.otl = next
	m.fanout.Set(logx.SlotOTel, h, level)
	if old != nil {
		old.logs.Discard()
		m.carryOver(SinkOTel, old.logs.Stats().Dropped)
	}
	if err != nil {
		m.errs[SinkOTel] = err.Error()
	}
	return err
}

// Disable turns both sinks off and retires their pipelines (fail closed):
// for a configuration that could not be resolved or applied. reason is
// shown as the last error of both sinks; it must hold no secret.
func (m *Manager) Disable(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	_ = m.applySyslog(config.SyslogConfig{})
	_ = m.applyOTel(context.Background(), config.OTLPConfig{})
	m.errs[SinkSyslog] = logx.CapText(reason, maxStatusError)
	m.errs[SinkOTel] = logx.CapText(reason, maxStatusError)
}

// maxStatusError bounds an error text in the status.
const maxStatusError = 256

func (m *Manager) carryOver(sink string, dropped map[string]uint64) {
	if len(dropped) == 0 {
		return
	}
	c := m.carry[sink]
	if c == nil {
		c = map[string]uint64{}
		m.carry[sink] = c
	}
	for k, v := range dropped {
		c[k] += v
	}
}

// Shutdown empties the remote slots, then flushes both sinks until ctx ends
// (in parallel); what is left is dropped and counted as shutdown. A summary
// of records that were never sent is written once to the local log. The
// Manager cannot be used afterwards.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	sys, otl := m.sys, m.otl
	m.fanout.Set(logx.SlotSyslog, nil, nil)
	m.fanout.Set(logx.SlotOTel, nil, nil)
	m.mu.Unlock()

	var wg sync.WaitGroup
	var sysErr, otlErr error
	if sys != nil {
		wg.Add(1)
		go func() { defer wg.Done(); sysErr = sys.w.Close(ctx) }()
	}
	if otl != nil {
		wg.Add(1)
		go func() { defer wg.Done(); otlErr = otl.logs.Shutdown(ctx) }()
	}
	wg.Wait()
	for _, st := range m.Status() {
		var lost uint64
		for _, n := range st.Dropped {
			lost += n
		}
		if lost > 0 {
			m.diag.Notice(slog.LevelWarn, "log export: records were never sent", "sink", st.Name, "dropped", st.Dropped)
		}
	}
	if sysErr != nil || otlErr != nil {
		return errors.New("obs: the log sinks did not flush in time; the rest was dropped")
	}
	return nil
}

// Status returns the effective state of both sinks, syslog first.
func (m *Manager) Status() []ops.LogSinkStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	sys := ops.LogSinkStatus{Name: SinkSyslog, Generation: m.gens[SinkSyslog], LastError: m.errs[SinkSyslog]}
	if p := m.sys; p != nil {
		st := p.w.Stats()
		sys.Enabled, sys.Destination, sys.Level = true, p.cfg.Destination(), p.cfg.Level
		sys.Queued, sys.Sent = uint64(st.Queued), st.Sent
		sys.LastSuccess = stamp(st.LastSuccess)
		if sys.LastError == "" {
			sys.LastError = logx.CapText(st.LastError, maxStatusError)
		}
		sys.Dropped = merge(m.carry[SinkSyslog], st.Dropped)
	} else {
		sys.Dropped = merge(m.carry[SinkSyslog], nil)
	}
	otl := ops.LogSinkStatus{Name: SinkOTel, Generation: m.gens[SinkOTel], LastError: m.errs[SinkOTel]}
	if p := m.otl; p != nil {
		st := p.logs.Stats()
		otl.Enabled, otl.Destination, otl.Level = true, logx.SafeURL(p.cfg.LogsURL), p.cfg.Level
		otl.Queued, otl.Sent = st.Queued, st.Sent
		otl.LastSuccess = stamp(st.LastSuccess)
		if otl.LastError == "" {
			otl.LastError = logx.CapText(st.LastError, maxStatusError)
		}
		d := st.Dropped
		if st.Failed > 0 {
			d = merge(d, map[string]uint64{"export_failed": st.Failed})
		}
		otl.Dropped = merge(m.carry[SinkOTel], d)
	} else {
		otl.Dropped = merge(m.carry[SinkOTel], nil)
	}
	return []ops.LogSinkStatus{sys, otl}
}

func stamp(t time.Time) *ops.Timestamp {
	if t.IsZero() {
		return nil
	}
	ts := ops.Timestamp(t.UTC())
	return &ts
}

func merge(a, b map[string]uint64) map[string]uint64 {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := map[string]uint64{}
	for k, v := range a {
		out[k] += v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}

// StopContext returns the context of a sink shutdown inside a process's stop
// budget: at most limit, and done half a second before ctx's deadline, so
// that what follows in the stop sequence still has time (Hub: 3 s of 15 s,
// Worker: 2 s of 10 s).
func StopContext(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	d := limit
	if dl, ok := ctx.Deadline(); ok {
		if rest := time.Until(dl) - 500*time.Millisecond; rest < d {
			d = max(rest, 0)
		}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), d)
}
