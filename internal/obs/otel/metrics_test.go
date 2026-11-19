package otel

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

// metricsReceiver is an in-process OTLP/HTTP metrics receiver.
type metricsReceiver struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
	reqs  []*colmetrics.ExportMetricsServiceRequest
}

func newMetricsReceiver(t *testing.T) *metricsReceiver {
	r := &metricsReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body io.Reader = req.Body
		if req.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(req.Body)
			if err != nil {
				w.WriteHeader(400)
				return
			}
			body = zr
		}
		data, _ := io.ReadAll(body)
		var m colmetrics.ExportMetricsServiceRequest
		if err := proto.Unmarshal(data, &m); err != nil {
			w.WriteHeader(400)
			return
		}
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		r.reqs = append(r.reqs, &m)
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// point is one data point: the instrument, its attributes and its value.
type point struct {
	name  string
	attrs map[string]string
	value float64
}

func attrMap(kvs []*commonpb.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		switch v := kv.Value.Value.(type) {
		case *commonpb.AnyValue_StringValue:
			m[kv.Key] = v.StringValue
		case *commonpb.AnyValue_BoolValue:
			m[kv.Key] = strconv.FormatBool(v.BoolValue)
		case *commonpb.AnyValue_IntValue:
			m[kv.Key] = strconv.FormatInt(v.IntValue, 10)
		}
	}
	return m
}

// points returns the data points of the last export.
func (r *metricsReceiver) points() []point {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		return nil
	}
	var out []point
	for _, rm := range r.reqs[len(r.reqs)-1].ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if s := m.GetSum(); s != nil {
					for _, dp := range s.DataPoints {
						out = append(out, point{m.Name, attrMap(dp.Attributes), float64(dp.GetAsInt())})
					}
				}
				if g := m.GetGauge(); g != nil {
					for _, dp := range g.DataPoints {
						out = append(out, point{m.Name, attrMap(dp.Attributes), float64(dp.GetAsInt())})
					}
				}
				if h := m.GetHistogram(); h != nil {
					for _, dp := range h.DataPoints {
						out = append(out, point{m.Name, attrMap(dp.Attributes), float64(dp.Count)})
					}
				}
			}
		}
	}
	return out
}

func metricsConfig(t *testing.T, url, level string) MetricsConfig {
	return MetricsConfig{Level: level, OTLP: resolveHub(t, nil, map[string]string{"otel": "on", "otel_endpoint": url, "otel_headers": "X-Key: k"})}
}

func record(m *Metrics) {
	m.ConnOpened()
	m.WSBytes("", false, 100) // handshake, unattributed
	m.WSBytes("w-1", true, 40)
	m.Frame("w-1", "op", true, 64)
	m.Op("w-1", "sessions_list", "ok", 20*time.Millisecond)
	m.Op("w-1", "made_up_op", "weird outcome", time.Millisecond)
	m.Call("pipe_send", "delivered")
	m.Admission("call", true)
	m.TurnIngest("busy")
	m.ConnClosed("w-1", true, time.Second)
}

func TestMetricsBasicAndDetailed(t *testing.T) {
	rcv := newMetricsReceiver(t)
	m := NewMetrics()
	m.SetOnline(func() int64 { return 3 })
	if err := m.Configure(context.Background(), metricsConfig(t, rcv.srv.URL, MetricsBasic), nil); err != nil {
		t.Fatal(err)
	}
	record(m)
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range rcv.points() {
		names[p.name] = true
		if _, ok := p.attrs["minion.worker.id"]; ok {
			t.Fatalf("basic carries a Worker id: %+v", p)
		}
		if p.attrs["minion.op"] == "made_up_op" || p.attrs["minion.outcome"] == "weird outcome" {
			t.Fatalf("unbounded attribute: %+v", p)
		}
		if p.name == "minion.worker.online" && p.value != 3 {
			t.Fatalf("online = %v", p.value)
		}
	}
	for _, n := range []string{"minion.worker.connection.opened", "minion.worker.connection.closed", "minion.worker.online", "minion.worker.ws.io",
		"minion.worker.frame.io", "minion.worker.frame.count", "minion.worker.op.count", "minion.worker.call.count",
		"minion.worker.stream.admission", "minion.pipe.turn.ingest"} {
		if !names[n] {
			t.Errorf("basic lacks %s (got %v)", n, names)
		}
	}
	if names["minion.worker.op.duration"] || names["minion.worker.connection.duration"] {
		t.Error("basic has the detailed histograms")
	}
	if rcv.paths[0] != "/v1/metrics" {
		t.Fatalf("path %q", rcv.paths[0])
	}

	rcv2 := newMetricsReceiver(t)
	d := NewMetrics()
	if err := d.Configure(context.Background(), metricsConfig(t, rcv2.srv.URL, MetricsDetailed), nil); err != nil {
		t.Fatal(err)
	}
	record(d)
	_ = d.Shutdown(context.Background())
	got := map[string]bool{}
	for _, p := range rcv2.points() {
		got[p.name] = true
		if p.name == "minion.worker.ws.io" && p.attrs["minion.connection.authenticated"] == "false" && p.attrs["minion.worker.id"] != "" {
			t.Fatalf("unauthenticated bytes attributed: %+v", p)
		}
		if p.name == "minion.worker.frame.io" && p.attrs["minion.worker.id"] != "w-1" {
			t.Fatalf("detailed frame.io = %+v", p)
		}
	}
	if !got["minion.worker.op.duration"] || !got["minion.worker.connection.duration"] {
		t.Fatalf("detailed lacks the histograms: %v", got)
	}
}

// Beyond MaxMetricWorkers Workers, the further ones share one overflow series.
func TestMetricsWorkerOverflow(t *testing.T) {
	rcv := newMetricsReceiver(t)
	m := NewMetrics()
	if err := m.Configure(context.Background(), metricsConfig(t, rcv.srv.URL, MetricsDetailed), nil); err != nil {
		t.Fatal(err)
	}
	for i := range MaxMetricWorkers + 20 {
		m.Frame("w-"+strconv.Itoa(i), "ev", false, 10)
	}
	_ = m.Shutdown(context.Background())
	workers, overflow := 0, 0.0
	for _, p := range rcv.points() {
		if p.name != "minion.worker.frame.count" {
			continue
		}
		if p.attrs["otel.metric.overflow"] == "true" {
			overflow += p.value
		} else if p.attrs["minion.worker.id"] != "" {
			workers++
		}
	}
	if workers != MaxMetricWorkers || overflow != 20 {
		t.Fatalf("workers %d overflow %v", workers, overflow)
	}
}

// Off records nothing and exports nothing; a level switch keeps the
// pipeline, a destination change builds a new one.
func TestMetricsOffAndSwitches(t *testing.T) {
	rcv := newMetricsReceiver(t)
	m := NewMetrics()
	record(m) // off: no pipeline, nothing happens
	cfg := metricsConfig(t, rcv.srv.URL, MetricsBasic)
	if err := m.Configure(context.Background(), cfg, nil); err != nil {
		t.Fatal(err)
	}
	first := m.cur.Load()
	cfg.Level = MetricsDetailed
	_ = m.Configure(context.Background(), cfg, nil)
	if m.cur.Load().mp != first.mp || !m.cur.Load().detailed {
		t.Fatal("a level switch rebuilt the pipeline")
	}
	other := newMetricsReceiver(t)
	_ = m.Configure(context.Background(), metricsConfig(t, other.srv.URL, MetricsDetailed), nil)
	if m.cur.Load().mp == first.mp {
		t.Fatal("a new destination kept the old pipeline")
	}
	_ = m.Configure(context.Background(), MetricsConfig{Level: MetricsOff}, nil)
	if m.cur.Load() != nil {
		t.Fatal("off kept a pipeline")
	}
	if len(rcv.points()) != 0 {
		t.Fatal("a retired pipeline exported")
	}
}
