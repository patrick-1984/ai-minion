package otel

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/logx"
)

type gotRequest struct {
	path    string
	headers http.Header
	req     *collogs.ExportLogsServiceRequest
}

// receiver is an in-process OTLP/HTTP logs receiver (protobuf, gzip).
type receiver struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []gotRequest
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{}
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
		var m collogs.ExportLogsServiceRequest
		if err := proto.Unmarshal(data, &m); err != nil {
			w.WriteHeader(400)
			return
		}
		r.mu.Lock()
		r.got = append(r.got, gotRequest{path: req.URL.Path, headers: req.Header.Clone(), req: &m})
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&collogs.ExportLogsServiceResponse{})
		_, _ = w.Write(out)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) requests() []gotRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]gotRequest(nil), r.got...)
}

// bodies returns every record body with its severity text and the resource
// attributes of the first request.
func (r *receiver) bodies() (bodies []string, res map[string]string) {
	res = map[string]string{}
	for _, g := range r.requests() {
		for _, rl := range g.req.ResourceLogs {
			for _, kv := range rl.GetResource().GetAttributes() {
				res[kv.Key] = kv.Value.GetStringValue()
			}
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					var attrs []string
					for _, kv := range lr.Attributes {
						attrs = append(attrs, kv.Key+"="+kv.Value.GetStringValue())
					}
					bodies = append(bodies, lr.SeverityText+" "+lr.Body.GetStringValue()+" "+strings.Join(attrs, ","))
				}
			}
		}
	}
	return bodies, res
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func resolveHub(t *testing.T, env, panel map[string]string) config.OTLPConfig {
	t.Helper()
	res := &config.Resolver{Role: config.ScopeHub, Panel: config.MapSource(panel), LookupEnv: func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}}
	s, err := config.ResolveSinks(res, config.SinkFiles{})
	if err != nil {
		t.Fatal(err)
	}
	return s.OTLP
}

func TestLogsReachTheCollectorAtTheSinkLevel(t *testing.T) {
	rcv := newReceiver(t)
	var local bytes.Buffer
	log, live, err := logx.NewLiveLocal(logx.Options{Level: "debug", Format: logx.FormatText, Output: &local})
	if err != nil {
		t.Fatal(err)
	}
	diag := NewDiag(live.Fanout.Local())
	InstallGlobal(diag)
	cfg := resolveHub(t, nil, map[string]string{"otel": "on", "otel_endpoint": rcv.srv.URL + "/base/", "otel_level": "warn",
		"otel_headers": "X-Scope-OrgID: tenant-1"})
	res, err := NewResource(context.Background(), Process{Service: "minion-hub", Version: "v0.14.0", InstanceID: "hub-1"})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := NewLogs(context.Background(), cfg, res)
	if err != nil {
		t.Fatal(err)
	}
	lvl, _ := logx.ParseLevel(cfg.Level)
	live.Fanout.Set(logx.SlotOTel, logs.Handler(), lvl)
	log = log.With("component", "test")
	log.Info("below the sink level")
	log.Warn("worker lost", "worker_id", "w-1", "token", "s3cr3t")
	log.Error("store failed", "error", "open https://u:p@db/x?k=v")
	logx.HumanNotice(context.Background(), log, slog.LevelError, "generated password", "password", "pw-1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := logs.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	bodies, resAttrs := rcv.bodies()
	joined := strings.Join(bodies, "\n")
	if len(bodies) != 2 || !strings.Contains(joined, "WARN worker lost") || !strings.Contains(joined, "ERROR store failed") {
		t.Fatalf("records = %q", bodies)
	}
	for _, leak := range []string{"s3cr3t", "u:p", "k=v", "pw-1", "generated password", "below the sink"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("leak %q in %q", leak, bodies)
		}
	}
	if !strings.Contains(joined, "component=test") {
		t.Fatalf("bound attribute lost: %q", bodies)
	}
	if resAttrs["service.name"] != "minion-hub" || resAttrs["service.version"] != "v0.14.0" || resAttrs["service.instance.id"] != "hub-1" || resAttrs["host.name"] == "" {
		t.Fatalf("resource = %v", resAttrs)
	}
	reqs := rcv.requests()
	if reqs[0].path != "/base/v1/logs" || reqs[0].headers.Get("X-Scope-OrgID") != "tenant-1" {
		t.Fatalf("path %q headers %v", reqs[0].path, reqs[0].headers)
	}
	if s := logs.Stats(); s.Sent != 2 || s.Emitted != 2 || s.LastSuccess.IsZero() {
		t.Fatalf("stats = %+v", s)
	}
	if !strings.Contains(local.String(), "generated password") {
		t.Fatal("the console lost the human notice")
	}
}

func TestExactSignalEndpointIsUsedAsIs(t *testing.T) {
	rcv := newReceiver(t)
	cfg := resolveHub(t, map[string]string{config.EnvOTLPLogsEndpoint: rcv.srv.URL + "/custom/ingest"}, map[string]string{"otel": "on"})
	logs, err := NewLogs(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	slog.New(logs.Handler()).Info("x")
	_ = logs.Shutdown(context.Background())
	if r := rcv.requests(); len(r) != 1 || r[0].path != "/custom/ingest" {
		t.Fatalf("requests = %+v", r)
	}
}

// The exporter never parses OTEL_EXPORTER_OTLP_HEADERS: its value (with a
// token and an invalid pair) reaches neither the collector nor any output.
func TestHeaderEnvNeverLeaks(t *testing.T) {
	rcv := newReceiver(t)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer s3cr3t,bad")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS", "Authorization=Bearer s3cr3t,bad")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "s3cr3t")
	var local bytes.Buffer
	_, live, _ := logx.NewLiveLocal(logx.Options{Level: "debug", Format: logx.FormatText, Output: &local})
	diag := NewDiag(live.Fanout.Local())
	InstallGlobal(diag)
	env := map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer s3cr3t,bad"}
	res := &config.Resolver{Role: config.ScopeHub, Panel: config.MapSource{"otel": "on", "otel_endpoint": rcv.srv.URL},
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok }}
	sinks, err := config.ResolveSinks(res, config.SinkFiles{})
	if err != nil {
		t.Fatal(err)
	}
	logs, err := NewLogs(context.Background(), sinks.OTLP, nil)
	if err != nil {
		t.Fatal(err)
	}
	slog.New(logs.Handler()).Info("hello")
	_ = logs.Shutdown(context.Background())
	r := rcv.requests()
	if len(r) != 1 {
		t.Fatalf("requests = %d", len(r))
	}
	for k, vs := range r[0].headers {
		if strings.Contains(strings.Join(vs, ","), "s3cr3t") {
			t.Fatalf("header %s carries the env token", k)
		}
	}
	all := local.String() + strings.Join(sinks.Warnings, "\n")
	if strings.Contains(all, "s3cr3t") {
		t.Fatalf("output leaks the env token: %s", all)
	}
	if !strings.Contains(strings.Join(sinks.Warnings, "\n"), "OTEL_EXPORTER_OTLP_HEADERS is ignored") {
		t.Fatalf("warnings = %q", sinks.Warnings)
	}
}

// A dead endpoint: logging stays non-blocking, and the local diagnostics
// show one line per minute (with the suppressed count later), never a
// record of a remote slot.
func TestDeadEndpointNeverBlocks(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	var local syncBuf
	log, live, _ := logx.NewLiveLocal(logx.Options{Level: "warn", Format: logx.FormatText, Output: &local})
	diag := NewDiag(live.Fanout.Local())
	InstallGlobal(diag)
	cfg := resolveHub(t, nil, map[string]string{"otel": "on", "otel_endpoint": "http://" + addr})
	logs, err := newLogs(context.Background(), cfg, nil, otlploghttp.RetryConfig{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	live.Fanout.Set(logx.SlotOTel, logs.Handler(), slog.LevelInfo)
	start := time.Now()
	for range 5000 {
		log.Info("x", "n", 1)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("logging blocked for %s", d)
	}
	eventually(t, func() bool { return logs.Stats().Failed > 0 })
	time.Sleep(1500 * time.Millisecond) // more failed exports
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	start = time.Now()
	_ = logs.Shutdown(ctx)
	cancel()
	if time.Since(start) > 3*time.Second {
		t.Fatal("shutdown overran its deadline")
	}
	eventually(t, func() bool { return strings.Contains(local.String(), "class=sdk") })
	// One line per (sink, class) within the minute (the diagnostics write
	// asynchronously).
	lines := strings.Split(strings.TrimSpace(local.String()), "\n")
	seen := map[string]bool{}
	for _, l := range lines {
		if !strings.Contains(l, `msg="log export problem"`) {
			t.Fatalf("unexpected local line: %s", l)
		}
		class := l[strings.Index(l, "class="):]
		class = class[:strings.Index(class, " error=")]
		if seen[class] {
			t.Fatalf("class %s reported twice within a minute:\n%s", class, local.String())
		}
		seen[class] = true
	}
	if !seen["class=sdk"] || strings.Contains(local.String(), "/v1/logs") {
		t.Fatalf("diagnostics = %s", local.String())
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestDiagRateLimit(t *testing.T) {
	var buf syncBuf
	d := NewDiag(slog.NewTextHandler(&buf, nil))
	now := time.Unix(1000, 0)
	d.now = func() time.Time { return now }
	d.sync = true
	err := io.ErrUnexpectedEOF
	d.Report("syslog", "connect", err)
	d.Report("syslog", "connect", err)
	d.Report("syslog", "write", err)
	now = now.Add(30 * time.Second)
	d.Report("syslog", "connect", err)
	now = now.Add(31 * time.Second)
	d.Report("syslog", "connect", err)
	out := buf.String()
	if strings.Count(out, "class=connect") != 2 || strings.Count(out, "class=write") != 1 || !strings.Contains(out, "suppressed=2") {
		t.Fatalf("diag = %s", out)
	}
}
