package traffic

import (
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/obs/otel"
	protocol "github.com/patrick-1984/ai-minion/internal/proto"
)

// sums collects the last export: instrument -> direction -> value.
type sums struct {
	mu   sync.Mutex
	last map[string]map[string]int64
}

func metricsServer(t *testing.T) (string, *sums) {
	s := &sums{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return
		}
		data, _ := io.ReadAll(zr)
		var m colmetrics.ExportMetricsServiceRequest
		if proto.Unmarshal(data, &m) != nil {
			return
		}
		out := map[string]map[string]int64{}
		for _, rm := range m.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, mt := range sm.Metrics {
					if mt.GetSum() == nil {
						continue
					}
					for _, dp := range mt.GetSum().DataPoints {
						dir := ""
						for _, kv := range dp.Attributes {
							if kv.Key == "network.io.direction" {
								dir = kv.Value.GetStringValue()
							}
						}
						if out[mt.Name] == nil {
							out[mt.Name] = map[string]int64{}
						}
						out[mt.Name][dir] += dp.GetAsInt()
					}
				}
			}
		}
		s.mu.Lock()
		s.last = out
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, s
}

func newMetrics(t *testing.T, url string) *otel.Metrics {
	t.Helper()
	res := &config.Resolver{Role: config.ScopeHub, Panel: config.MapSource{"otel": "on", "otel_endpoint": url},
		LookupEnv: func(string) (string, bool) { return "", false }}
	sinks, err := config.ResolveSinks(res, config.SinkFiles{})
	if err != nil {
		t.Fatal(err)
	}
	m := otel.NewMetrics()
	if err := m.Configure(context.Background(), otel.MetricsConfig{Level: otel.MetricsBasic, OTLP: sinks.OTLP}, nil); err != nil {
		t.Fatal(err)
	}
	return m
}

// L1 counts exactly the bytes a scripted peer sent and received over the
// wrapped connection.
func TestL1CountsExactPayloadBytes(t *testing.T) {
	url, got := metricsServer(t)
	m := newMetrics(t, url)
	a, b := net.Pipe()
	cc := NewCountingConn(a, m)
	go func() {
		_, _ = b.Write(make([]byte, 1234))
		buf := make([]byte, 4096)
		n := 0
		for n < 777 {
			k, err := b.Read(buf)
			if err != nil {
				return
			}
			n += k
		}
		_ = b.Close()
	}()
	buf := make([]byte, 1234)
	if _, err := io.ReadFull(cc, buf); err != nil {
		t.Fatal(err)
	}
	cc.Authenticated("w-1")
	if _, err := cc.Write(make([]byte, 777)); err != nil {
		t.Fatal(err)
	}
	cc.Closed(true)
	_ = m.Shutdown(context.Background())
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.last["minion.worker.ws.io"]["receive"] != 1234 || got.last["minion.worker.ws.io"]["transmit"] != 777 {
		t.Fatalf("ws.io = %v", got.last["minion.worker.ws.io"])
	}
}

// L3 equals the sum of the frame lengths plus 4 per frame, both directions.
func TestL3CountsFrameBytes(t *testing.T) {
	url, got := metricsServer(t)
	m := newMetrics(t, url)
	hub, worker := sessions(t, MetricsTap(m, "w-1"), nil)
	frames := []any{
		map[string]any{"type": "op.request", "op": "sessions_list", "request_id": "r"},
		map[string]any{"type": "op.progress", "data": map[string]any{"n": 1}},
	}
	var want int64
	for _, f := range frames {
		b, err := protocol.EncodeFrame(protocol.StreamFrameLimit, f)
		if err != nil {
			t.Fatal(err)
		}
		want += int64(len(b))
	}
	reply, _ := protocol.EncodeFrame(protocol.StreamFrameLimit, map[string]any{"type": "op.result", "result": map[string]any{}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		st, err := worker.Accept(context.Background())
		if err != nil {
			return
		}
		for range frames {
			if _, err := st.ReadFrame(); err != nil {
				return
			}
		}
		_ = st.WriteFrame(map[string]any{"type": "op.result", "result": map[string]any{}})
	}()
	st, err := hub.OpenOp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range frames {
		if err := st.WriteFrame(f); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	<-done
	_ = m.Shutdown(context.Background())
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.last["minion.worker.frame.io"]["transmit"] != want || got.last["minion.worker.frame.io"]["receive"] != int64(len(reply)) ||
		got.last["minion.worker.frame.count"]["transmit"] != 2 {
		t.Fatalf("frame.io %v count %v (want %d / %d)", got.last["minion.worker.frame.io"], got.last["minion.worker.frame.count"], want, len(reply))
	}
}
