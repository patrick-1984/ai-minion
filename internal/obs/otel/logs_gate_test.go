package otel

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/patrick-1984/ai-minion/internal/logx"
)

func newPipe(t *testing.T, url string, headers string) (*Logs, *slog.Logger, *syncBuf) {
	t.Helper()
	local := &syncBuf{}
	_, live, _ := logx.NewLiveLocal(logx.Options{Level: "warn", Format: logx.FormatText, Output: local})
	diag := NewDiag(live.Fanout.Local())
	diag.sync = true
	InstallGlobal(diag)
	panel := map[string]string{"otel": "on", "otel_endpoint": url}
	if headers != "" {
		panel["otel_headers"] = headers
	}
	cfg := resolveHub(t, nil, panel)
	logs, err := newLogs(context.Background(), cfg, nil, otlploghttp.RetryConfig{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	return logs, slog.New(logs.Handler()), local
}

// A collector that answers 307 never gets the batch or the headers replayed
// to another origin.
func TestRedirectIsNotFollowed(t *testing.T) {
	var other atomic.Int64
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other.Add(1) }))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/v1/logs", http.StatusTemporaryRedirect)
	}))
	defer a.Close()
	logs, log, _ := newPipe(t, a.URL, "X-API-Key: k3y")
	log.Info("x")
	_ = logs.Shutdown(context.Background())
	if other.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
	if s := logs.Stats(); s.Failed == 0 || !strings.Contains(s.LastError, "307") {
		t.Fatalf("stats = %+v", s)
	}
}

// Collector error bodies (and partial-success messages) never reach the
// status or the diagnostics.
func TestCollectorTextNeverReachesStatusOrDiagnostics(t *testing.T) {
	for _, h := range []http.HandlerFunc{
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("rejected key OPAQUE-CRED-123"))
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-protobuf")
			b, _ := proto.Marshal(&collogs.ExportLogsServiceResponse{PartialSuccess: &collogs.ExportLogsPartialSuccess{
				RejectedLogRecords: 1, ErrorMessage: "bad header OPAQUE-CRED-123"}})
			_, _ = w.Write(b)
		},
	} {
		srv := httptest.NewServer(h)
		logs, log, local := newPipe(t, srv.URL, "")
		log.Info("x")
		_ = logs.Shutdown(context.Background())
		srv.Close()
		s := logs.Stats()
		if s.LastError == "" || strings.Contains(s.LastError, "OPAQUE") || strings.Contains(local.String(), "OPAQUE") {
			t.Fatalf("status %q, local %q", s.LastError, local.String())
		}
	}
}

// Discard retires the pipeline at once: the export in flight is cancelled
// and nothing queued is sent later.
func TestDiscardStopsSending(t *testing.T) {
	var requests atomic.Int64
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer once.Do(func() { close(release) })
	logs, log, _ := newPipe(t, srv.URL, "")
	log.Info("first")
	eventually(t, func() bool { return requests.Load() == 1 })
	for range 100 {
		log.Info("queued under the old configuration")
	}
	start := time.Now()
	logs.Discard()
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("Discard took %s", d)
	}
	once.Do(func() { close(release) })
	time.Sleep(1500 * time.Millisecond)
	if n := requests.Load(); n != 1 {
		t.Fatalf("%d requests; the retired pipeline kept sending", n)
	}
	s := logs.Stats()
	if s.Queued != 0 || s.Dropped[DropReconfigure] != 101 || s.Sent != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

// A burst over the queue is counted as queue_full; Queued stays exact.
func TestQueueOverflowIsCounted(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	logs, log, _ := newPipe(t, srv.URL, "")
	for range 3000 {
		log.Info("burst")
	}
	s := logs.Stats()
	if s.Queued > QueueSize || s.Dropped[DropQueueFull] < 3000-QueueSize || s.Queued+s.Dropped[DropQueueFull] != 3000 {
		t.Fatalf("stats = %+v", s)
	}
	close(release)
	logs.Discard()
	if s := logs.Stats(); s.Queued != 0 {
		t.Fatalf("after discard: %+v", s)
	}
}

// A record with invalid UTF-8 does not make the collector reject the batch:
// the other records of the batch arrive.
func TestInvalidUTF8DoesNotDropTheBatch(t *testing.T) {
	rcv := newReceiver(t)
	logs, log, _ := newPipe(t, rcv.srv.URL, "")
	log.Info("path /a\xffb", "p", "/x\xfe", "bad\xffkey", 1)
	log.Info("neighbour")
	_ = logs.Shutdown(context.Background())
	bodies, _ := rcv.bodies()
	if len(bodies) != 2 || !strings.Contains(strings.Join(bodies, "\n"), "neighbour") {
		t.Fatalf("records = %q (stats %+v)", bodies, logs.Stats())
	}
}
