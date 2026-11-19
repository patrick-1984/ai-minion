package otel

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Levels of otel_metrics (design §4.2).
const (
	MetricsOff      = "off"
	MetricsBasic    = "basic"
	MetricsDetailed = "detailed"
)

// Fixed parameters of the metrics pipeline.
const (
	// DefaultMetricsInterval is the export interval;
	// OTEL_METRIC_EXPORT_INTERVAL (milliseconds) replaces it.
	DefaultMetricsInterval = 60 * time.Second
	// MaxMetricWorkers caps the Workers that get series of their own at the
	// detailed level; further Workers share one series with
	// otel.metric.overflow=true (the specification's overflow attribute).
	MaxMetricWorkers = 500
	// ScopeMetrics is the instrumentation scope of the traffic metrics.
	ScopeMetrics = "github.com/patrick-1984/ai-minion/traffic"
)

// CardinalityLimit is the series budget of each instrument: the SDK
// default (2000) would merge series below the 500-Worker cap (500 Workers
// times 3 stream kinds times 2 directions on the frame instruments, more on
// the op counter). It bounds the memory of the pipeline.
const CardinalityLimit = 100_000

// buildTimeout bounds the construction of the exporter, which reads the
// OTEL_EXPORTER_OTLP_*CERTIFICATE files of the environment itself before
// Minion's options replace them (a stalled mount must not hang a start or a
// settings change).
var buildTimeout = 5 * time.Second

// Attribute values of the traffic metrics.
const (
	DirReceive  = "receive"
	DirTransmit = "transmit"
)

// Metrics records the Worker traffic metrics of the Hub. Every recording
// method is cheap, safe for concurrent use and does nothing while the level
// is off; Configure switches level and destination live.
type Metrics struct {
	online atomic.Pointer[func() int64]

	mu       sync.Mutex // serialises Configure
	closed   atomic.Bool
	building atomic.Bool // a build is outstanding (buildBounded)
	cur      atomic.Pointer[meterSet]
	digest   string // of the configuration of the running pipeline
}

type meterSet struct {
	detailed bool
	mp       *sdkmetric.MeterProvider
	reg      metric.Registration

	connOpened, connClosed metric.Int64Counter
	wsIO, frameIO          metric.Int64Counter
	frameCount             metric.Int64Counter
	opCount, callCount     metric.Int64Counter
	admission, turnIngest  metric.Int64Counter
	connDuration, opDur    metric.Float64Histogram

	wmu     sync.Mutex
	workers map[string]bool
}

// NewMetrics returns metrics that are off.
func NewMetrics() *Metrics { return &Metrics{} }

// SetOnline sets the source of the minion.worker.online gauge.
func (m *Metrics) SetOnline(f func() int64) { m.online.Store(&f) }

// MetricsConfig is what Configure applies.
type MetricsConfig struct {
	// Level is off, basic or detailed.
	Level string
	// OTLP is the OpenTelemetry sink configuration whose MetricsURL,
	// headers and trust the exporter uses.
	OTLP config.OTLPConfig
}

// Configure applies c. A change of destination or credential builds a new
// pipeline and drops the old one at once (its unexported data is not sent
// with the new credential); a change between basic and detailed keeps the
// pipeline. Off shuts the pipeline down without a final export.
func (m *Metrics) Configure(ctx context.Context, c MetricsConfig, res *resource.Resource) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.cur.Load()
	if c.Level == MetricsOff || c.Level == "" || !c.OTLP.Enabled || m.closed.Load() {
		m.cur.Store(nil)
		m.digest = ""
		retire(old)
		return nil
	}
	// The level is part of the pipeline: after a change from detailed to
	// basic, a reused provider would keep exporting the per-Worker series
	// it aggregated, so any change builds a new one.
	digest := c.OTLP.Digest + "|" + c.Level
	if old != nil && digest == m.digest {
		return nil
	}
	next, err := m.buildBounded(ctx, c, res)
	if err != nil {
		m.cur.Store(nil)
		m.digest = ""
		retire(old)
		return err
	}
	if m.closed.Load() {
		retire(next)
		return nil
	}
	m.cur.Store(next)
	if m.closed.Load() && m.cur.CompareAndSwap(next, nil) {
		retire(next) // Shutdown ran between the check and the store
		return nil
	}
	m.digest = digest
	retire(old)
	return nil
}

// retire ends a pipeline at once, without a final export.
func retire(s *meterSet) {
	if s == nil {
		return
	}
	if s.reg != nil {
		_ = s.reg.Unregister()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.mp.Shutdown(ctx)
}

// Shutdown exports once more and stops, within ctx. It does not wait for a
// Configure in progress (which retires what it builds once it sees the
// shutdown).
func (m *Metrics) Shutdown(ctx context.Context) error {
	m.closed.Store(true)
	s := m.cur.Swap(nil)
	if s == nil {
		return nil
	}
	return s.mp.Shutdown(ctx)
}

// buildBounded runs build with buildTimeout; a build that is still blocked
// then is abandoned (its pipeline, if it ever completes, is shut down). At
// most one build is outstanding: while an abandoned one is stuck, a new
// attempt fails at once instead of leaving another goroutine behind.
func (m *Metrics) buildBounded(ctx context.Context, c MetricsConfig, res *resource.Resource) (*meterSet, error) {
	if !m.building.CompareAndSwap(false, true) {
		return nil, errors.New("an earlier attempt to build the OTLP metrics exporter is still blocked")
	}
	type result struct {
		s   *meterSet
		err error
	}
	const running, delivered, abandoned = 0, 1, 2
	var state atomic.Int32
	ch := make(chan result, 1)
	go func() {
		defer m.building.Store(false)
		s, err := m.build(ctx, c, res)
		if state.CompareAndSwap(running, delivered) {
			ch <- result{s, err}
			return
		}
		retire(s) // nobody waits for it any more
	}()
	t := time.NewTimer(buildTimeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-t.C:
		if state.CompareAndSwap(running, abandoned) {
			return nil, errors.New("the OTLP metrics exporter could not be built in time")
		}
		r := <-ch
		return r.s, r.err
	}
}

func (m *Metrics) build(ctx context.Context, c MetricsConfig, res *resource.Resource) (*meterSet, error) {
	if c.OTLP.MetricsURL == "" {
		return nil, errors.New("the OTLP metrics URL is not set")
	}
	var tlsCfg *tls.Config
	if strings.HasPrefix(c.OTLP.MetricsURL, "https://") {
		tlsCfg = c.OTLP.MetricsTLSConfig()
	}
	headers := c.OTLP.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	client, _ := exportClient(tlsCfg)
	exp, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(c.OTLP.MetricsURL),
		otlpmetrichttp.WithHeaders(headers),
		otlpmetrichttp.WithTLSClientConfig(tlsCfg),
		otlpmetrichttp.WithTimeout(ExportTimeout),
		otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression),
		otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig(retry)),
		otlpmetrichttp.WithHTTPClient(client),
	)
	if err != nil {
		return nil, errors.New("the OTLP metrics exporter could not be built")
	}
	opts := []sdkmetric.Option{sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
		sdkmetric.WithInterval(metricsInterval()), sdkmetric.WithTimeout(ExportTimeout))),
		sdkmetric.WithCardinalityLimit(CardinalityLimit)}
	if res != nil {
		opts = append(opts, sdkmetric.WithResource(res))
	}
	mp := sdkmetric.NewMeterProvider(opts...)
	meter := mp.Meter(ScopeMetrics)
	s := &meterSet{detailed: c.Level == MetricsDetailed, mp: mp, workers: map[string]bool{}}
	counter := func(name, unit, desc string) metric.Int64Counter {
		ctr, _ := meter.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(desc))
		return ctr
	}
	s.connOpened = counter("minion.worker.connection.opened", "{connection}", "Worker WebSocket connections accepted")
	s.connClosed = counter("minion.worker.connection.closed", "{connection}", "Worker WebSocket connections ended")
	s.wsIO = counter("minion.worker.ws.io", "By", "WebSocket message payload bytes (L1: sealed secure-channel records, handshake included)")
	s.frameIO = counter("minion.worker.frame.io", "By", "Application frame bytes (L3: 4-byte length and JSON body) on op, ca and ev streams")
	s.frameCount = counter("minion.worker.frame.count", "{frame}", "Application frames on op, ca and ev streams")
	s.opCount = counter("minion.worker.op.count", "{op}", "Ops the Hub sent to Workers")
	s.callCount = counter("minion.worker.call.count", "{call}", "Worker to Hub call stream requests")
	s.admission = counter("minion.worker.stream.admission", "{stream}", "Streams admitted to or refused from a lane")
	s.turnIngest = counter("minion.pipe.turn.ingest", "{turn}", "Pipe turns accepted, found busy or rejected")
	s.connDuration, _ = meter.Float64Histogram("minion.worker.connection.duration", metric.WithUnit("s"),
		metric.WithDescription("Lifetime of authenticated Worker connections (detailed level)"))
	s.opDur, _ = meter.Float64Histogram("minion.worker.op.duration", metric.WithUnit("s"),
		metric.WithDescription("Duration of ops sent to Workers (detailed level)"))
	gauge, _ := meter.Int64ObservableGauge("minion.worker.online", metric.WithUnit("{worker}"),
		metric.WithDescription("Workers online"))
	s.reg, _ = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		if f := m.online.Load(); f != nil {
			o.ObserveInt64(gauge, (*f)())
		}
		return nil
	}, gauge)
	return s, nil
}

// metricsInterval is OTEL_METRIC_EXPORT_INTERVAL (milliseconds) when it is a
// positive number, else DefaultMetricsInterval.
func metricsInterval() time.Duration {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("OTEL_METRIC_EXPORT_INTERVAL"))); err == nil && v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return DefaultMetricsInterval
}

// workerAttr is the Worker attribute of the detailed level: the id for the
// first MaxMetricWorkers Workers, the overflow marker for the others.
func (s *meterSet) workerAttr(id string) attribute.KeyValue {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.workers[id] || len(s.workers) < MaxMetricWorkers {
		s.workers[id] = true
		return attribute.String("minion.worker.id", id)
	}
	return attribute.Bool("otel.metric.overflow", true)
}

func dirOf(sent bool) string {
	if sent {
		return DirTransmit
	}
	return DirReceive
}

// boundOp keeps the op attribute within the op catalog.
func boundOp(op string) string {
	if _, ok := ops.Lookup(op); ok {
		return op
	}
	return "other"
}

// boundOutcome keeps the outcome attribute within the known outcomes and
// error codes.
func boundOutcome(o string) string {
	switch o {
	case "ok", string(ops.OutcomeDelivered), string(ops.OutcomePartial), string(ops.OutcomeFailedNotSent), string(ops.OutcomeUnknown),
		"accepted", "busy", "rejected":
		return o
	}
	if proto.Code(o).Known() {
		return o
	}
	return "other"
}

func add(ctr metric.Int64Counter, n int64, kv ...attribute.KeyValue) {
	ctr.Add(context.Background(), n, metric.WithAttributes(kv...))
}

// ConnOpened counts an accepted Worker WebSocket.
func (m *Metrics) ConnOpened() {
	if s := m.cur.Load(); s != nil {
		add(s.connOpened, 1)
	}
}

// ConnClosed counts the end of a connection; workerID is "" when the
// handshake did not complete. d is its lifetime (detailed level, for an
// authenticated connection).
func (m *Metrics) ConnClosed(workerID string, handshakeOK bool, d time.Duration) {
	s := m.cur.Load()
	if s == nil {
		return
	}
	end := "ended"
	if !handshakeOK {
		end = "handshake_failed"
	}
	add(s.connClosed, 1, attribute.String("minion.connection.end", end))
	if s.detailed && workerID != "" {
		s.connDuration.Record(context.Background(), d.Seconds(), metric.WithAttributes(s.workerAttr(workerID)))
	}
}

// WSBytes counts n WebSocket payload bytes (L1). Bytes before the handshake
// identified the Worker are unattributed (authenticated=false).
func (m *Metrics) WSBytes(workerID string, sent bool, n int) {
	s := m.cur.Load()
	if s == nil || n <= 0 {
		return
	}
	kv := []attribute.KeyValue{attribute.String("network.io.direction", dirOf(sent)), attribute.Bool("minion.connection.authenticated", workerID != "")}
	if s.detailed && workerID != "" {
		kv = append(kv, s.workerAttr(workerID))
	}
	add(s.wsIO, int64(n), kv...)
}

// Frame counts one application frame of size bytes (L3).
func (m *Metrics) Frame(workerID, kind string, sent bool, size int) {
	s := m.cur.Load()
	if s == nil {
		return
	}
	kv := []attribute.KeyValue{attribute.String("network.io.direction", dirOf(sent)), attribute.String("minion.stream.kind", kind)}
	if s.detailed && workerID != "" {
		kv = append(kv, s.workerAttr(workerID))
	}
	add(s.frameIO, int64(size), kv...)
	add(s.frameCount, 1, kv...)
}

// Op counts an op the Hub sent to a Worker.
func (m *Metrics) Op(workerID, op, outcome string, d time.Duration) {
	s := m.cur.Load()
	if s == nil {
		return
	}
	if s.detailed && workerID != "" {
		// Per Worker, the series are few: the ops the Hub sends to Workers
		// (the catalog's Worker scope) and an outcome class, so that
		// 500 Workers times every op times every class stay far below
		// CardinalityLimit whatever error codes come back.
		kv := []attribute.KeyValue{attribute.String("minion.op", boundWorkerOp(op)),
			attribute.String("minion.outcome", outcomeClass(outcome)), s.workerAttr(workerID)}
		s.opDur.Record(context.Background(), d.Seconds(), metric.WithAttributes(kv...))
		add(s.opCount, 1, kv...)
		return
	}
	add(s.opCount, 1, attribute.String("minion.op", boundOp(op)), attribute.String("minion.outcome", boundOutcome(outcome)))
}

// boundWorkerOp keeps the op attribute within the ops the Hub sends to a
// Worker.
func boundWorkerOp(op string) string {
	if o, ok := ops.Lookup(op); ok && o.Scope == ops.ScopeWorker {
		return op
	}
	return "other"
}

// outcomeClass folds an outcome or error code into ok, unknown or error.
func outcomeClass(o string) string {
	switch o {
	case "ok", string(ops.OutcomeDelivered), string(ops.OutcomePartial), "accepted":
		return "ok"
	case string(ops.OutcomeUnknown), string(proto.CodeConnectionLost):
		return "unknown"
	}
	return "error"
}

// Call counts a Worker->Hub call stream request.
func (m *Metrics) Call(op, outcome string) {
	if s := m.cur.Load(); s != nil {
		add(s.callCount, 1, attribute.String("minion.op", boundOp(op)), attribute.String("minion.outcome", boundOutcome(outcome)))
	}
}

// Admission counts a stream admitted to or refused from lane (call, event).
func (m *Metrics) Admission(lane string, admitted bool) {
	if s := m.cur.Load(); s != nil {
		r := "refused"
		if admitted {
			r = "admitted"
		}
		add(s.admission, 1, attribute.String("minion.lane", lane), attribute.String("minion.admission", r))
	}
}

// TurnIngest counts a pipe turn: accepted, busy or rejected.
func (m *Metrics) TurnIngest(result string) {
	if s := m.cur.Load(); s != nil {
		add(s.turnIngest, 1, attribute.String("minion.outcome", boundOutcome(result)))
	}
}
