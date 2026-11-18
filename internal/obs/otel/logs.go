package otel

import (
	"context"
	"crypto/tls"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
)

// Fixed parameters of the log pipeline (design §3.4): with records capped at
// 16 KiB (logx.RemoteSafe) a request of 256 records stays under 4 MiB
// uncompressed, far below a collector's default 20 MiB request limit.
const (
	ExportTimeout  = 5 * time.Second
	QueueSize      = 1024
	BatchSize      = 256
	ExportInterval = time.Second
	// ScopeName is the instrumentation scope of Minion's own log records.
	ScopeName = "github.com/patrick-1984/ai-minion"
)

// Retry policy of the exporter.
var retry = otlploghttp.RetryConfig{
	Enabled:         true,
	InitialInterval: time.Second,
	MaxInterval:     10 * time.Second,
	MaxElapsedTime:  30 * time.Second,
}

// Stats is the state of a log pipeline.
type Stats struct {
	// Emitted counts the records handed to the pipeline, Sent those a
	// collector accepted, Failed those whose export failed for good.
	Emitted, Sent, Failed uint64
	// Dropped counts records that were never sent, by reason (reconfigure,
	// shutdown); records the batch queue dropped when it was full are in
	// Pending until the pipeline ends.
	Dropped     map[string]uint64
	LastSuccess time.Time
	LastError   string
}

// Pending is the number of records emitted and neither sent nor failed.
func (s Stats) Pending() uint64 {
	done := s.Sent + s.Failed
	for _, n := range s.Dropped {
		done += n
	}
	if done > s.Emitted {
		return 0
	}
	return s.Emitted - done
}

// Logs is one OTLP/HTTP log pipeline. It is built once from a validated
// configuration and never changed: a new configuration builds a new Logs.
type Logs struct {
	provider *sdklog.LoggerProvider
	handler  slog.Handler
	count    *counters
}

type counters struct {
	emitted, sent, failed atomic.Uint64
	reconfigure, shutdown atomic.Uint64
	lastOK                atomic.Int64
	lastErr               atomic.Pointer[string]
}

// NewLogs builds the pipeline for cfg. The exporter is built with every
// option explicit, so it never reads an OTEL_EXPORTER_OTLP_* variable itself
// (an invalid header variable would otherwise be reported with its whole
// value). InstallGlobal must have been called before, so that export errors
// reach the local, rate-limited diagnostics.
func NewLogs(ctx context.Context, cfg config.OTLPConfig, res *resource.Resource) (*Logs, error) {
	return newLogs(ctx, cfg, res, retry)
}

func newLogs(ctx context.Context, cfg config.OTLPConfig, res *resource.Resource, retry otlploghttp.RetryConfig) (*Logs, error) {
	headers := cfg.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	// The TLS option is set in both cases, so that the exporter never loads
	// OTEL_EXPORTER_OTLP_CERTIFICATE and friends: nil for http://.
	var tlsCfg *tls.Config
	if strings.HasPrefix(cfg.LogsURL, "https://") {
		tlsCfg = cfg.TLSConfig()
	}
	exp, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpointURL(cfg.LogsURL),
		otlploghttp.WithHeaders(headers),
		otlploghttp.WithTLSClientConfig(tlsCfg),
		otlploghttp.WithTimeout(ExportTimeout),
		otlploghttp.WithCompression(otlploghttp.GzipCompression),
		otlploghttp.WithRetry(retry),
	)
	if err != nil {
		return nil, err
	}
	c := &counters{}
	proc := sdklog.NewBatchProcessor(&countingExporter{Exporter: exp, c: c},
		sdklog.WithMaxQueueSize(QueueSize),
		sdklog.WithExportMaxBatchSize(BatchSize),
		sdklog.WithExportInterval(ExportInterval),
		sdklog.WithExportTimeout(ExportTimeout),
	)
	opts := []sdklog.LoggerProviderOption{sdklog.WithProcessor(&countingProcessor{Processor: proc, c: c})}
	if res != nil {
		opts = append(opts, sdklog.WithResource(res))
	}
	lp := sdklog.NewLoggerProvider(opts...)
	h := otelslog.NewHandler(ScopeName, otelslog.WithLoggerProvider(lp))
	return &Logs{provider: lp, handler: logx.RemoteSafe(h), count: c}, nil
}

// Handler returns the slog handler of the pipeline, behind logx.RemoteSafe.
func (l *Logs) Handler() slog.Handler { return l.handler }

// Shutdown flushes what is queued until ctx ends; what is left then is
// counted as dropped (shutdown). Retries end with ctx.
func (l *Logs) Shutdown(ctx context.Context) error {
	err := l.provider.Shutdown(ctx)
	l.dropRest(&l.count.shutdown)
	return err
}

// Discard ends the pipeline at once: the queue is dropped and counted as
// reconfigure, so records queued under an old endpoint or credential are
// never sent with a new one, nor later with the old one.
func (l *Logs) Discard() {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = l.provider.Shutdown(ctx)
	l.dropRest(&l.count.reconfigure)
}

func (l *Logs) dropRest(into *atomic.Uint64) {
	s := l.Stats()
	if p := s.Pending(); p > 0 {
		into.Add(p)
	}
}

// Stats returns the counters of the pipeline.
func (l *Logs) Stats() Stats {
	c := l.count
	s := Stats{Emitted: c.emitted.Load(), Sent: c.sent.Load(), Failed: c.failed.Load(), Dropped: map[string]uint64{}}
	if n := c.reconfigure.Load(); n > 0 {
		s.Dropped["reconfigure"] = n
	}
	if n := c.shutdown.Load(); n > 0 {
		s.Dropped["shutdown"] = n
	}
	if ns := c.lastOK.Load(); ns != 0 {
		s.LastSuccess = time.Unix(0, ns)
	}
	if p := c.lastErr.Load(); p != nil {
		s.LastError = *p
	}
	return s
}

// countingProcessor counts the records handed to the batch processor.
type countingProcessor struct {
	sdklog.Processor
	c *counters
}

func (p *countingProcessor) OnEmit(ctx context.Context, r *sdklog.Record) error {
	p.c.emitted.Add(1)
	return p.Processor.OnEmit(ctx, r)
}

// countingExporter counts exported and failed records.
type countingExporter struct {
	sdklog.Exporter
	c *counters
}

func (e *countingExporter) Export(ctx context.Context, recs []sdklog.Record) error {
	err := e.Exporter.Export(ctx, recs)
	if err != nil {
		e.c.failed.Add(uint64(len(recs)))
		s := logx.ErrorText(err)
		e.c.lastErr.Store(&s)
		// The batch processor hands err to the SDK's error handler, which
		// InstallGlobal routes to the rate-limited diagnostics.
		return err
	}
	e.c.sent.Add(uint64(len(recs)))
	e.c.lastOK.Store(time.Now().UnixNano())
	return nil
}
