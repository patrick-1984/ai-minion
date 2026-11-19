package otel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	proc     sdklog.Processor
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
	// term serialises the terminal accounting of records (sent, failed,
	// dropped): once final is set (the pipeline ended and what was left
	// was counted), a late export counts nothing again.
	term    sync.Mutex
	final   bool
	lastOK  atomic.Int64
	lastErr atomic.Pointer[string]
}

// settle accounts n records that left the pipeline; fn counts them. It
// does nothing once the pipeline was finalised (they were counted then).
func (c *counters) settle(n int, fn func()) {
	c.term.Lock()
	defer c.term.Unlock()
	if c.final {
		return
	}
	c.outstanding.Add(-int64(n))
	fn()
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
	// The exporter treats an endpoint it cannot parse as unset and falls
	// back to OTEL_EXPORTER_OTLP_ENDPOINT or localhost, with these headers:
	// never hand it one.
	if u, err := url.Parse(cfg.LogsURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("the OTLP logs URL is not valid")
	}
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
	status := &statusRecorder{}
	client := &http.Client{
		Transport: responseFilter{rt: tr, status: status},
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
	ce := &countingExporter{Exporter: exp, c: c, pipe: pctx, cancel: cancel, status: status, dest: logx.SafeURL(cfg.LogsURL)}
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
	return &Logs{provider: lp, proc: proc, handler: logx.RemoteSafe(h), count: c, exp: ce}, nil
}

// statusRecorder keeps the HTTP status of the last response of an export.
type statusRecorder struct{ code atomic.Int64 }

// responseFilter keeps collector-provided text out of every error: a non-2xx
// response loses its body, its reason phrase and every header except a
// numeric Retry-After (so a refused redirect has no Location the client
// would parse and quote); a 2xx response must be an OTLP answer (empty, or
// protobuf), otherwise the export fails instead of counting a login page as
// delivery. The status code is recorded for the error class.
type responseFilter struct {
	rt     http.RoundTripper
	status *statusRecorder
}

func (f responseFilter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := f.rt.RoundTrip(r)
	if err != nil || resp == nil {
		return resp, err
	}
	f.status.code.Store(int64(resp.StatusCode))
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		// The whole answer is read first: empty only after a successful
		// read, protobuf (any spelling of the media type) when not empty,
		// handed on with the exact header the exporter checks before it
		// decodes (it reports success for anything else).
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxOTLPResponse+1))
		_ = resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		if len(body) > maxOTLPResponse {
			return nil, errNotOTLP
		}
		ct := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
		if len(body) > 0 && ct != "application/x-protobuf" {
			return nil, errNotOTLP
		}
		if len(body) > 0 {
			resp.Header.Set("Content-Type", "application/x-protobuf")
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	h := http.Header{}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if _, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
			h.Set("Retry-After", strings.TrimSpace(ra))
		}
	}
	resp.Header = h
	resp.Body = io.NopCloser(strings.NewReader(""))
	resp.ContentLength = 0
	resp.Status = strconv.Itoa(resp.StatusCode) + " " + http.StatusText(resp.StatusCode)
	return resp, nil
}

// maxOTLPResponse bounds an OTLP answer (a partial-success message).
const maxOTLPResponse = 64 << 10

// errNotOTLP is a 2xx answer that is not an OTLP response.
var errNotOTLP = errors.New("the endpoint answered, but not as an OTLP collector")

// Handler returns the slog handler of the pipeline, behind logx.RemoteSafe.
func (l *Logs) Handler() slog.Handler { return l.handler }

// Shutdown flushes what is queued until ctx ends. When ctx ends first, the
// pipeline is retired: the export in flight (and its retries) is cancelled
// and what is left is dropped and counted as shutdown. The batch worker is
// always stopped, also for a context that has already expired.
func (l *Logs) Shutdown(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { l.exp.retire(DropShutdown) })
	err := l.provider.Shutdown(ctx)
	stop()
	l.end(DropShutdown)
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
	l.end(DropReconfigure)
}

// end retires the exporter, stops the batch processor (the provider skips
// it for an expired context; its worker would otherwise stay alive) and
// counts what is still outstanding, once.
func (l *Logs) end(reason string) {
	l.exp.retire(reason)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	_ = l.proc.Shutdown(ctx) // the retired exporter drops at once
	cancel()
	c := l.count
	c.term.Lock()
	defer c.term.Unlock()
	if c.final {
		return
	}
	c.final = true
	if n := c.outstanding.Swap(0); n > 0 {
		c.drop(reason, int(n))
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
	status *statusRecorder
	dest   string // scheme and host, for error texts
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
	if reason, ok := e.retiredReason(); ok {
		e.c.settle(n, func() { e.c.drop(reason, n) })
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(e.pipe, cancel)
	defer stop()
	e.status.code.Store(0)
	err := e.Exporter.Export(ctx, recs)
	if reason, ok := e.retiredReason(); ok && err != nil {
		e.c.settle(n, func() { e.c.drop(reason, n) })
		return nil
	}
	if err != nil {
		safe := e.exportError(err)
		e.c.settle(n, func() {
			e.c.failed.Add(uint64(n))
			s := safe.Error()
			e.c.lastErr.Store(&s)
		})
		// The batch processor hands the error to the SDK's error handler,
		// which InstallGlobal routes to the rate-limited diagnostics.
		return safe
	}
	e.c.settle(n, func() {
		e.c.sent.Add(uint64(n))
		e.c.lastOK.Store(time.Now().UnixNano())
	})
	return nil
}

// ExportError is an export failure reduced to text that holds no
// collector-provided content: the destination as scheme and host, the
// transport error or the HTTP status code.
type ExportError struct{ text string }

func (e *ExportError) Error() string { return e.text }

// maxErrorText bounds the text of an export error.
const maxErrorText = 256

// exportError reduces err to a locally generated class: the HTTP status
// code, or the kind of transport failure, and the destination's scheme and
// host. No text of the error itself is kept (a malformed response, a
// header or a partial-success message can carry peer-provided text).
func (e *countingExporter) exportError(err error) *ExportError {
	return &ExportError{text: logx.CapText("export failed: "+errorClass(err, int(e.status.code.Load()))+" ("+e.dest+")", maxErrorText)}
}

func errorClass(err error, status int) string {
	var x509Unknown x509.UnknownAuthorityError
	var x509Host x509.HostnameError
	var x509Invalid x509.CertificateInvalidError
	var tlsAlert tls.AlertError
	var tlsRecord tls.RecordHeaderError
	var tlsVerify *tls.CertificateVerificationError
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.Is(err, errNotOTLP):
		return "the endpoint answered, but not as an OTLP collector (HTTP " + strconv.Itoa(status) + ")"
	case strings.Contains(strings.ToLower(err.Error()), "partial success"):
		return "the collector rejected some records (partial success)"
	case status != 0 && (status < 200 || status > 299):
		return "HTTP " + strconv.Itoa(status)
	case errors.As(err, &tlsVerify), errors.As(err, &x509Unknown), errors.As(err, &x509Host), errors.As(err, &x509Invalid):
		return "the server certificate was not accepted"
	case errors.As(err, &tlsAlert), errors.As(err, &tlsRecord):
		return "TLS handshake failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.As(err, &dnsErr):
		return "the host name could not be resolved"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "transport error"
}
