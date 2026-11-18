package otel

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
)

// rawServer answers every request with the given raw HTTP response.
func rawServer(t *testing.T, response string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				req, err := http.ReadRequest(r)
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body)
				_, _ = c.Write([]byte(response))
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

// Peer text in response headers (a malformed Location) or in a malformed
// status line never reaches the status or the diagnostics.
func TestPeerTextInHeadersAndStatusLineIsWithheld(t *testing.T) {
	for _, resp := range []string{
		"HTTP/1.1 307 Temporary Redirect\r\nLocation: %zzOPAQUE-CRED\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.1 401 OPAQUE-CRED reflected\r\nX-Debug: OPAQUE-CRED\r\nContent-Length: 0\r\n\r\n",
		"HTTP/1.1 abc OPAQUE-CRED\r\n\r\n",
	} {
		url := rawServer(t, resp)
		logs, log, local := newPipe(t, url, "")
		log.Info("x")
		_ = logs.Shutdown(context.Background())
		s := logs.Stats()
		if s.LastError == "" || strings.Contains(s.LastError, "OPAQUE") || strings.Contains(local.String(), "OPAQUE") {
			t.Fatalf("%q: status %q local %q", resp[:20], s.LastError, local.String())
		}
	}
}

// A 2xx answer that is not an OTLP response (a login page) is a failure, not
// a delivery.
func TestHTMLAnswerIsNotADelivery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>sign in</html>"))
	}))
	defer srv.Close()
	logs, log, _ := newPipe(t, srv.URL, "")
	log.Info("x")
	_ = logs.Shutdown(context.Background())
	if s := logs.Stats(); s.Sent != 0 || s.Failed != 1 || !strings.Contains(s.LastError, "not as an OTLP collector") {
		t.Fatalf("stats = %+v", s)
	}
}

// An already expired shutdown stops the batch worker and counts every record
// exactly once, also when an export was stalled.
func TestExpiredShutdownCountsOnceAndStopsTheWorker(t *testing.T) {
	var requests atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	logs, log, _ := newPipe(t, srv.URL, "")
	log.Info("in flight")
	eventually(t, func() bool { return requests.Load() == 1 })
	for range 50 {
		log.Info("queued")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = logs.Shutdown(ctx)
	close(release)
	time.Sleep(1500 * time.Millisecond)
	s := logs.Stats()
	var dropped uint64
	for _, n := range s.Dropped {
		dropped += n
	}
	if s.Queued != 0 || s.Sent+s.Failed+dropped != s.Emitted || s.Emitted != 51 {
		t.Fatalf("stats = %+v", s)
	}
	if requests.Load() != 1 {
		t.Fatalf("%d requests after shutdown", requests.Load())
	}
}

// An endpoint the exporter could not parse never falls back to the standard
// endpoint variable with Minion's headers.
func TestInvalidURLNeverReachesTheFallback(t *testing.T) {
	var hits atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer fallback.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", fallback.URL)
	_, err := NewLogs(context.Background(), config.OTLPConfig{Enabled: true, LogsURL: "http://[fe80::1%eth0]:4318/v1/logs",
		Headers: map[string]string{"Authorization": "Bearer t"}}, nil)
	if err == nil {
		t.Fatal("an unparsable URL was accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("the fallback was contacted")
	}
}

// Invalid UTF-8 in OTEL_RESOURCE_ATTRIBUTES does not stop every export.
func TestInvalidResourceUTF8StillExports(t *testing.T) {
	rcv := newReceiver(t)
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "site=%FF,team=ok")
	t.Setenv("OTEL_SERVICE_NAME", "svc\xff")
	cfg := resolveHub(t, nil, map[string]string{"otel": "on", "otel_endpoint": rcv.srv.URL})
	res, _ := NewResource(context.Background(), Process{Service: "minion-hub"})
	logs, err := NewLogs(context.Background(), cfg, res)
	if err != nil {
		t.Fatal(err)
	}
	newPipeLog(logs).Info("x")
	_ = logs.Shutdown(context.Background())
	bodies, attrs := rcv.bodies()
	if len(bodies) != 1 || attrs["team"] != "ok" {
		t.Fatalf("bodies %q attrs %v stats %+v", bodies, attrs, logs.Stats())
	}
}

func newPipeLog(l *Logs) *slog.Logger { return slog.New(l.Handler()) }
