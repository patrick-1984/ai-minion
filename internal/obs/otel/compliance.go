package otel

import (
	"context"
	"crypto/tls"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Budgets of the compliance queue (design §5.8). A request of 256 records or
// 4 MiB (estimated, uncompressed) stays far below a collector's default
// request limit of 20 MiB; the queue holds at most 4096 records and 16 MiB.
const (
	ComplianceQueueRecords   = 4096
	ComplianceQueueBytes     = 16 << 20
	ComplianceRequestRecords = 256
	ComplianceRequestBytes   = 4 << 20
	complianceInterval       = time.Second
	// recordOverhead is added to the size estimate of every record (names,
	// timestamps, framing).
	recordOverhead = 128
)

// Further reasons a compliance record is not delivered, as the status reports
// them (queue_full, reconfigure and shutdown are the log pipeline's).
const DropNoExporter = "no_exporter"

// ComplianceOptions are the budgets and the interval of a compliance
// pipeline. The zero value of a field means the constant above; tests shrink
// them.
type ComplianceOptions struct {
	QueueRecords, QueueBytes, RequestRecords, RequestBytes int
	Interval                                               time.Duration
}

func (o ComplianceOptions) fill() ComplianceOptions {
	if o.QueueRecords <= 0 {
		o.QueueRecords = ComplianceQueueRecords
	}
	if o.QueueBytes <= 0 {
		o.QueueBytes = ComplianceQueueBytes
	}
	if o.RequestRecords <= 0 {
		o.RequestRecords = ComplianceRequestRecords
	}
	if o.RequestBytes <= 0 {
		o.RequestBytes = ComplianceRequestBytes
	}
	if o.Interval <= 0 {
		o.Interval = complianceInterval
	}
	return o
}

// Compliance is the compliance export: its own LoggerProvider, exporter,
// credentials, trust and queue, independent of the log sinks and of their
// levels. Records are emitted with the Logs API (an event name, not a slog
// message). It is built once from a validated configuration; a new
// configuration builds a new one and discards this.
type Compliance struct {
	provider *sdklog.LoggerProvider
	logger   otellog.Logger
	proc     *budgetProcessor
	exp      *countingExporter
	count    *counters
	dest     string
	opts     ComplianceOptions
}

// NewCompliance builds the pipeline of cfg for the process p. As for the log
// pipeline, the exporter gets every option explicitly (it never reads an
// OTEL_EXPORTER_OTLP_* variable), refuses redirects and keeps collector text
// out of every error.
func NewCompliance(ctx context.Context, cfg config.ComplianceConfig, p Process) (*Compliance, error) {
	return newCompliance(ctx, cfg, p, ComplianceOptions{}, retry)
}

func newCompliance(ctx context.Context, cfg config.ComplianceConfig, p Process, opts ComplianceOptions, rc otlploghttp.RetryConfig) (*Compliance, error) {
	if u, err := url.Parse(cfg.LogsURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("the compliance logs URL is not valid")
	}
	opts = opts.fill()
	headers := cfg.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	var tlsCfg *tls.Config
	if strings.HasPrefix(cfg.LogsURL, "https://") {
		tlsCfg = cfg.TLSConfig()
	}
	client, status := exportClient(tlsCfg)
	exp, err := otlploghttp.New(ctx,
		otlploghttp.WithEndpointURL(cfg.LogsURL),
		otlploghttp.WithHeaders(headers),
		otlploghttp.WithTLSClientConfig(tlsCfg),
		otlploghttp.WithTimeout(ExportTimeout),
		otlploghttp.WithCompression(otlploghttp.GzipCompression),
		otlploghttp.WithRetry(rc),
		otlploghttp.WithHTTPClient(client),
	)
	if err != nil {
		return nil, err
	}
	res, _ := NewResource(ctx, p)
	c := &counters{}
	pctx, cancel := context.WithCancel(context.Background())
	dest := logx.SafeURL(cfg.LogsURL)
	ce := &countingExporter{Exporter: exp, c: c, pipe: pctx, cancel: cancel, status: status, dest: dest}
	bp := newBudgetProcessor(ce, c, opts)
	lopts := []sdklog.LoggerProviderOption{sdklog.WithProcessor(bp)}
	if res != nil {
		lopts = append(lopts, sdklog.WithResource(res))
	}
	lp := sdklog.NewLoggerProvider(lopts...)
	cp := &Compliance{provider: lp, logger: lp.Logger(ScopeName), proc: bp, exp: ce, count: c, dest: dest, opts: opts}
	bp.emitGap = cp.emitGap
	bp.start()
	return cp, nil
}

// Emit queues a record. It never blocks and never fails; a record that does
// not fit the budgets is dropped and counted.
func (c *Compliance) Emit(r compliance.Record) {
	c.logger.Emit(context.Background(), toLogRecord(r))
}

func toLogRecord(r compliance.Record) otellog.Record {
	var rec otellog.Record
	rec.SetEventName(r.Event)
	rec.SetTimestamp(r.Time)
	rec.SetObservedTimestamp(time.Now())
	rec.SetSeverity(otellog.SeverityInfo)
	rec.SetSeverityText("INFO")
	if r.Body != "" {
		rec.SetBody(attribute.StringValue(r.Body))
	}
	kvs := make([]attribute.KeyValue, 0, len(r.Attrs))
	for _, a := range r.Attrs {
		switch v := a.Value.(type) {
		case string:
			kvs = append(kvs, attribute.String(a.Key, v))
		case int64:
			kvs = append(kvs, attribute.Int64(a.Key, v))
		case int:
			kvs = append(kvs, attribute.Int(a.Key, v))
		case bool:
			kvs = append(kvs, attribute.Bool(a.Key, v))
		case []string:
			kvs = append(kvs, attribute.StringSlice(a.Key, v))
		}
	}
	rec.AddAttributes(kvs...)
	return rec
}

// emitGap tells the collector what was lost (design §5.8): how many records
// were dropped, of which events, between which times.
func (c *Compliance) emitGap(g gapInfo) {
	events := make([]string, 0, len(g.byEvent))
	for e, n := range g.byEvent {
		events = append(events, e+"="+strconv.FormatUint(n, 10))
	}
	sort.Strings(events)
	c.Emit(compliance.Record{Event: compliance.EventGap, Time: time.Now(), Attrs: []compliance.Attr{
		{Key: "minion.dropped", Value: int64(g.n)},
		{Key: "minion.dropped.by_event", Value: strings.Join(events, ",")},
		{Key: "minion.dropped.reason", Value: DropQueueFull},
		{Key: "minion.dropped.first_ts", Value: g.first.UTC().Format(time.RFC3339Nano)},
		{Key: "minion.dropped.last_ts", Value: g.last.UTC().Format(time.RFC3339Nano)},
	}})
}

// Close flushes what is queued until ctx ends. When ctx ends first the
// pipeline is retired: the export in flight is cancelled and the rest is
// dropped and counted as shutdown.
func (c *Compliance) Close(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { c.exp.retire(DropShutdown) })
	c.proc.drain(ctx)
	err := c.provider.Shutdown(ctx)
	stop()
	c.end(DropShutdown)
	return err
}

// Discard ends the pipeline at once: records queued under a revoked
// destination or credential are never sent, with the new configuration or
// later with the old one.
func (c *Compliance) Discard() {
	c.exp.retire(DropReconfigure)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.provider.Shutdown(ctx)
	c.end(DropReconfigure)
}

func (c *Compliance) end(reason string) {
	c.exp.retire(reason)
	c.proc.stop()
	cnt := c.count
	cnt.term.Lock()
	defer cnt.term.Unlock()
	if cnt.final {
		return
	}
	cnt.final = true
	if n := cnt.outstanding.Swap(0); n > 0 {
		cnt.drop(reason, int(n))
	}
}

// ComplianceStats is the accounting of a compliance pipeline. For every
// record handed to Emit: Emitted = Sent + Failed + Queued + (sum of Dropped
// except export_failed, which is Failed).
type ComplianceStats struct {
	Emitted, Sent, Failed, Queued uint64
	QueuedBytes                   uint64
	Dropped                       map[string]uint64
	LastSuccess                   time.Time
	LastError                     string
	// DroppedByEvent counts the records dropped for a full queue, by event.
	DroppedByEvent map[string]uint64
	// MaxRequestBytes and MaxRequestRecords are the largest request built.
	MaxRequestBytes, MaxRequestRecords int
}

// Stats returns the counters.
func (c *Compliance) Stats() ComplianceStats {
	cnt := c.count
	s := ComplianceStats{Emitted: cnt.emitted.Load(), Sent: cnt.sent.Load(), Failed: cnt.failed.Load(), Dropped: map[string]uint64{}}
	if n := cnt.outstanding.Load(); n > 0 {
		s.Queued = uint64(n)
	}
	s.QueuedBytes = uint64(c.proc.queuedBytes())
	for reason, n := range map[string]uint64{DropQueueFull: cnt.queueFull.Load(), DropReconfigure: cnt.reconfigure.Load(), DropShutdown: cnt.shutdown.Load(), DropExportFailed: cnt.failed.Load()} {
		if n > 0 {
			s.Dropped[reason] = n
		}
	}
	if ns := cnt.lastOK.Load(); ns != 0 {
		s.LastSuccess = time.Unix(0, ns)
	}
	if p := cnt.lastErr.Load(); p != nil {
		s.LastError = *p
	}
	s.DroppedByEvent, s.MaxRequestBytes, s.MaxRequestRecords = c.proc.accounting()
	return s
}

// Status is the state of the export for the Hub's diagnostics. It holds no
// credential: the destination is scheme and host.
func (c *Compliance) Status() ops.LogSinkStatus {
	s := c.Stats()
	st := ops.LogSinkStatus{Name: "compliance", Enabled: true, Destination: c.dest, Queued: s.Queued, Sent: s.Sent,
		LastError: s.LastError, Dropped: s.Dropped}
	if !s.LastSuccess.IsZero() {
		t := ops.Timestamp(s.LastSuccess)
		st.LastSuccess = &t
	}
	return st
}

// --- the budget processor ----------------------------------------------------

type gapInfo struct {
	n       uint64
	byEvent map[string]uint64
	first   time.Time
	last    time.Time
}

type queuedRecord struct {
	rec  sdklog.Record
	size int
}

// budgetProcessor is the sdklog.Processor of the compliance queue. It holds
// at most opts.QueueRecords records and opts.QueueBytes estimated bytes; when
// full it drops the NEW record (what is queued keeps its order) and counts it
// by event name. When space returns it emits one minion.compliance.gap record.
// A request holds at most opts.RequestRecords records and opts.RequestBytes.
type budgetProcessor struct {
	exp  *countingExporter
	c    *counters
	opts ComplianceOptions

	mu     sync.Mutex
	q      []queuedRecord
	qBytes int
	closed bool
	gap    gapInfo
	byEv   map[string]uint64
	maxReq int
	maxRec int

	emitGap func(gapInfo)
	wake    chan struct{}
	quit    chan struct{}
	done    chan struct{}
	once    sync.Once
	// exportMu serialises the exports: the exporter is not used concurrently.
	exportMu sync.Mutex
}

func newBudgetProcessor(exp *countingExporter, c *counters, o ComplianceOptions) *budgetProcessor {
	return &budgetProcessor{exp: exp, c: c, opts: o, byEv: map[string]uint64{},
		wake: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{})}
}

func (p *budgetProcessor) start() { go p.run() }

func (p *budgetProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func estimate(r *sdklog.Record) int {
	n := recordOverhead + len(r.EventName())
	if b := r.Body(); b.Type() == attribute.STRING {
		n += len(b.AsString())
	}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		n += len(kv.Key) + 16
		if kv.Value.Type() == attribute.STRING {
			n += len(kv.Value.AsString())
		} else {
			n += len(kv.Value.String())
		}
		return true
	})
	return n
}

func (p *budgetProcessor) OnEmit(_ context.Context, r *sdklog.Record) error {
	p.c.emitted.Add(1)
	ev := r.EventName()
	size := estimate(r)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.c.shutdown.Add(1)
		return nil
	}
	gapRecord := ev == compliance.EventGap
	if !gapRecord && (len(p.q) >= p.opts.QueueRecords || p.qBytes+size > p.opts.QueueBytes) {
		p.c.queueFull.Add(1)
		now := time.Now()
		if p.gap.n == 0 {
			p.gap = gapInfo{byEvent: map[string]uint64{}, first: now}
		}
		p.gap.n++
		p.gap.byEvent[ev]++
		p.gap.last = now
		p.byEv[ev]++
		p.mu.Unlock()
		return nil
	}
	p.c.outstanding.Add(1)
	p.q = append(p.q, queuedRecord{rec: r.Clone(), size: size})
	p.qBytes += size
	full := len(p.q) >= p.opts.RequestRecords
	p.mu.Unlock()
	if full {
		p.signal()
	}
	return nil
}

func (p *budgetProcessor) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *budgetProcessor) queuedBytes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.qBytes
}

func (p *budgetProcessor) accounting() (map[string]uint64, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]uint64, len(p.byEv))
	for k, v := range p.byEv {
		out[k] = v
	}
	return out, p.maxReq, p.maxRec
}

// take removes the next request from the queue: at most RequestRecords
// records and RequestBytes estimated bytes, at least one record.
func (p *budgetProcessor) take() []sdklog.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.q) == 0 {
		return nil
	}
	n, bytes := 0, 0
	for n < len(p.q) && n < p.opts.RequestRecords {
		if n > 0 && bytes+p.q[n].size > p.opts.RequestBytes {
			break
		}
		bytes += p.q[n].size
		n++
	}
	batch := make([]sdklog.Record, n)
	for i := 0; i < n; i++ {
		batch[i] = p.q[i].rec
	}
	p.q = append(p.q[:0:0], p.q[n:]...)
	p.qBytes -= bytes
	if bytes > p.maxReq {
		p.maxReq = bytes
	}
	if n > p.maxRec {
		p.maxRec = n
	}
	return batch
}

// pendingGap returns the gap to report once the queue is at most half full.
func (p *budgetProcessor) pendingGap() (gapInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gap.n == 0 || p.closed || len(p.q) > p.opts.QueueRecords/2 || p.qBytes > p.opts.QueueBytes/2 {
		return gapInfo{}, false
	}
	g := p.gap
	p.gap = gapInfo{}
	return g, true
}

func (p *budgetProcessor) run() {
	defer close(p.done)
	t := time.NewTicker(p.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-p.quit:
			return
		case <-p.wake:
		case <-t.C:
		}
		p.flushOnce()
	}
}

// flushOnce exports every queued request and then reports a gap if space
// returned.
func (p *budgetProcessor) flushOnce() {
	for {
		select {
		case <-p.quit:
			return
		default:
		}
		batch := p.take()
		if len(batch) == 0 {
			break
		}
		p.export(context.Background(), batch)
	}
	if g, ok := p.pendingGap(); ok && p.emitGap != nil {
		p.emitGap(g)
		p.signal()
	}
}

// drain exports what is queued until ctx ends or nothing is left.
func (p *budgetProcessor) drain(ctx context.Context) {
	for ctx.Err() == nil {
		batch := p.take()
		if len(batch) == 0 {
			return
		}
		p.export(ctx, batch)
	}
}

func (p *budgetProcessor) export(ctx context.Context, batch []sdklog.Record) {
	p.exportMu.Lock()
	defer p.exportMu.Unlock()
	if err := p.exp.Export(ctx, batch); err != nil {
		otel.Handle(err)
	}
}

func (p *budgetProcessor) stop() {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.quit)
	})
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
	}
}

func (p *budgetProcessor) Shutdown(ctx context.Context) error {
	p.drain(ctx)
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return ctx.Err()
}

func (p *budgetProcessor) ForceFlush(ctx context.Context) error {
	p.drain(ctx)
	return ctx.Err()
}

var _ sdklog.Processor = (*budgetProcessor)(nil)
