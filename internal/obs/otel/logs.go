package otel

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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

// Reasons a record is dropped, as the status reports them.
const (
	DropQueueFull    = "queue_full"
	DropReconfigure  = "reconfigure"
	DropShutdown     = "shutdown"
	DropExportFailed = "export_failed"
)

// Stats is the state of a log pipeline.
type Stats struct {
	// Emitted counts the records handed to the pipeline, Sent those a
	// collector accepted, Failed those whose export failed for good.
	Emitted, Sent, Failed uint64
	// Queued is the number of records waiting or being exported now.
	Queued uint64
	// Dropped counts records that were never sent, by reason (queue_full,
	// reconfigure, shutdown).
	Dropped     map[string]uint64
	LastSuccess time.Time
	LastError   string
}

// Logs is one OTLP/HTTP log pipeline. It is built once from a validated
// configuration and never changed: a new configuration builds a new Logs.
// Once retired (Discard, or Shutdown past its deadline) it sends nothing
// more: every export in flight is cancelled and every later batch dropped.
type Logs struct {
	provider *sdklog.LoggerProvider
	handler  slog.Handler
	count    *counters
	exp      *countingExporter
}

type counters struct {
	emitted, sent, failed atomic.Uint64
	queueFull             atomic.Uint64
	reconfigure, shutdown atomic.Uint64
	// outstanding counts the records in the SDK queue or in an export.
	outstanding atomic.Int64
	lastOK      atomic.Int64
	lastErr     atomic.Pointer[string]
}

func (c *counters) drop(reason string, n int) {
	if n <= 0 {
		return
	}
	switch reason {
	case DropReconfigure:
		c.reconfigure.Add(uint64(n))
	default:
		c.shutdown.Add(uint64(n))
	}
}

// NewLogs builds the pipeline for cfg. The exporter is built with every
// option explicit, so it never reads an OTEL_EXPORTER_OTLP_* variable itself
// (an invalid header variable would otherwise be reported with its whole
// value). Its HTTP client refuses redirects (a 307 or 308 would replay the
// batch and the headers to another origin) and drops the body and reason
// phrase of every error response, so that no collector-provided text (which
// can echo a rejected credential) reaches an error, the status or a log.
// InstallGlobal must have been called before, so that export errors reach
// the local, rate-limited diagnostics.
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
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	client := &http.Client{
		Transport: errorBodyStripper{rt: tr},
		Timeout:   ExportTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	exp, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpointURL(cfg.LogsURL),
		otlploghttp.WithHeaders(headers),
		otlploghttp.WithTLSClientConfig(tlsCfg),
		otlploghttp.WithTimeout(ExportTimeout),
		otlploghttp.WithCompression(otlploghttp.GzipCompression),
		otlploghttp.WithRetry(retry),
		otlploghttp.WithHTTPClient(client),
	)
	if err != nil {
		return nil, err
	}
	c := &counters{}
	pctx, cancel := context.WithCancel(context.Background())
	ce := &countingExporter{Exporter: exp, c: c, pipe: pctx, cancel: cancel}
	proc := sdklog.NewBatchProcessor(ce,
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
	return &Logs{provider: lp, handler: logx.RemoteSafe(h), count: c, exp: ce}, nil
}

// errorBodyStripper replaces the body of every non-2xx response with an
// empty one and its status line with the standard text of the code: the
// exporter quotes both in its errors, and a gateway may echo a rejected
// credential in them. A 2xx body (the OTLP response) is kept.
type errorBodyStripper struct{ rt http.RoundTripper }

func (s errorBodyStripper) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.rt.RoundTrip(r)
	if err != nil || resp == nil || (resp.StatusCode >= 200 && resp.StatusCode <= 299) {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(strings.NewReader(""))
	resp.ContentLength = 0
	resp.Status = strconv.Itoa(resp.StatusCode) + " " + http.StatusText(resp.StatusCode)
	return resp, nil
}

// Handler returns the slog handler of the pipeline, behind logx.RemoteSafe.
func (l *Logs) Handler() slog.Handler { return l.handler }

// Shutdown flushes what is queued until ctx ends. When ctx ends first, the
// pipeline is retired: the export in flight (and its retries) is cancelled
// and what is left is dropped and counted as shutdown.
func (l *Logs) Shutdown(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { l.exp.retire(DropShutdown) })
	err := l.provider.Shutdown(ctx)
	stop()
	l.exp.retire(DropShutdown)
	l.dropRest(DropShutdown)
	return err
}

// Discard ends the pipeline at once: it is retired first (the export in
// flight is cancelled, nothing is sent any more), then the queue is drained
// into the drop count (reconfigure). Records queued under an old endpoint or
// credential are never sent with a new one, nor later with the old one.
func (l *Logs) Discard() {
	l.exp.retire(DropReconfigure)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = l.provider.Shutdown(ctx)
	l.dropRest(DropReconfigure)
}

func (l *Logs) dropRest(reason string) {
	if n := l.count.outstanding.Swap(0); n > 0 {
		l.count.drop(reason, int(n))
	}
}

// Stats returns the counters of the pipeline.
func (l *Logs) Stats() Stats {
	c := l.count
	s := Stats{Emitted: c.emitted.Load(), Sent: c.sent.Load(), Failed: c.failed.Load(), Dropped: map[string]uint64{}}
	if n := c.outstanding.Load(); n > 0 {
		s.Queued = uint64(n)
	}
	for reason, n := range map[string]uint64{DropQueueFull: c.queueFull.Load(), DropReconfigure: c.reconfigure.Load(), DropShutdown: c.shutdown.Load()} {
		if n > 0 {
			s.Dropped[reason] = n
		}
	}
	if ns := c.lastOK.Load(); ns != 0 {
		s.LastSuccess = time.Unix(0, ns)
	}
	if p := c.lastErr.Load(); p != nil {
		s.LastError = *p
	}
	return s
}

// countingProcessor counts the records handed to the batch processor and
// keeps the SDK queue from overflowing: a record that would exceed
// QueueSize outstanding records is dropped here and counted as queue_full,
// so that the SDK never evicts one silently and Queued stays exact.
type countingProcessor struct {
	sdklog.Processor
	c *counters
}

func (p *countingProcessor) OnEmit(ctx context.Context, r *sdklog.Record) error {
	p.c.emitted.Add(1)
	if p.c.outstanding.Add(1) > QueueSize {
		p.c.outstanding.Add(-1)
		p.c.queueFull.Add(1)
		return nil
	}
	return p.Processor.OnEmit(ctx, r)
}

// countingExporter counts exported and failed records, cancels its exports
// when the pipeline is retired and then drops every batch it is given.
type countingExporter struct {
	sdklog.Exporter
	c      *counters
	pipe   context.Context
	cancel context.CancelFunc
	reason atomic.Pointer[string]
}

func (e *countingExporter) retire(reason string) {
	e.reason.CompareAndSwap(nil, &reason)
	e.cancel()
}

func (e *countingExporter) retiredReason() (string, bool) {
	if p := e.reason.Load(); p != nil {
		return *p, true
	}
	return "", false
}

func (e *countingExporter) Export(ctx context.Context, recs []sdklog.Record) error {
	n := len(recs)
	defer e.c.outstanding.Add(-int64(n))
	if reason, ok := e.retiredReason(); ok {
		e.c.drop(reason, n)
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(e.pipe, cancel)
	defer stop()
	err := e.Exporter.Export(ctx, recs)
	if reason, ok := e.retiredReason(); ok && err != nil {
		e.c.drop(reason, n)
		return nil
	}
	if err != nil {
		e.c.failed.Add(uint64(n))
		safe := exportError(err)
		s := safe.Error()
		e.c.lastErr.Store(&s)
		// The batch processor hands the error to the SDK's error handler,
		// which InstallGlobal routes to the rate-limited diagnostics.
		return safe
	}
	e.c.sent.Add(uint64(n))
	e.c.lastOK.Store(time.Now().UnixNano())
	return nil
}

// ExportError is an export failure reduced to text that holds no
// collector-provided content: the destination as scheme and host, the
// transport error or the HTTP status code.
type ExportError struct{ text string }

func (e *ExportError) Error() string { return e.text }

// maxErrorText bounds the text of an export error.
const maxErrorText = 256

func exportError(err error) *ExportError {
	t := logx.ErrorText(err)
	if strings.Contains(strings.ToLower(t), "partial success") {
		t = "the collector rejected some records (partial success)"
	}
	return &ExportError{text: logx.CapText("export failed: "+t, maxErrorText)}
}
