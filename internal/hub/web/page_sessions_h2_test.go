package web

// TLS/HTTP/2 regressions of the live dashboard stream (Story 5.4). The Hub
// serves HTTP/2 on its TLS port. There a client that stops reading its TCP
// socket blocks the connection's socket write, and net/http queues the stream
// deadline's RST_STREAM behind that write, so neither SetWriteDeadline nor
// Interrupt releases the handler. The Hub bounds it with
// http.HTTP2Config.WriteByteTimeout (internal/hub/server, newHTTPServer); these
// tests run the handler on a server configured the same way and show that the
// stalls end and that healthy long-lived streams are left alone.

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/testutil/stallh2"
)

// h2LiveServer serves the dashboard stream over TLS with HTTP/2. A zero
// writeByteTimeout leaves the connection write unbounded (the control case).
// wrap sees every request before the handler.
func h2LiveServer(t *testing.T, src LiveSource, writeByteTimeout time.Duration, tune func(*liveHandler), wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	h := newLiveHandler(func() LiveSource { return src }, 5*time.Millisecond, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.reconcile = time.Hour
	tune(h)
	reg := NewRegistry()
	if err := reg.RegisterRoute(Route{Pattern: "GET " + pathLiveDashboard, Handler: h}); err != nil {
		t.Fatal(err)
	}
	var handler http.Handler = newServer(t, reg)
	if wrap != nil {
		handler = wrap(handler)
	}
	ts := httptest.NewUnstartedServer(handler)
	ts.EnableHTTP2 = true
	if writeByteTimeout > 0 {
		ts.Config.HTTP2 = &http.HTTP2Config{WriteByteTimeout: writeByteTimeout}
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func h2Addr(ts *httptest.Server) string { return strings.TrimPrefix(ts.URL, "https://") }

var aliceHdr = map[string]string{"X-Test-Session": "alice:user"}

// h2Stall opens the stream on an HTTP/2 client with a huge window that reads
// only the response headers, then feeds big pictures until the handler is
// blocked in the socket write.
func h2Stall(t *testing.T, ts *httptest.Server, src *fakeSource) *stallh2.Client {
	t.Helper()
	c := stallh2.Dial(t, h2Addr(ts))
	c.Get(1, pathLiveDashboard, aliceHdr)
	c.ReadHeaders(1, 3*time.Second)
	waitFor(t, 3*time.Second, func() bool { return len(src.asked()) >= 2 })
	driveUntilBlocked(t, src)
	return c
}

// A client that stops reading must not hold the stream: the stream's write
// deadline cannot fire through the blocked socket write, the connection write
// timeout ends it. (net/http then closes the TLS connection, whose close_notify
// is itself bounded by crypto/tls at five seconds; the waits allow for that.)
func TestLiveStreamH2StalledClientIsCutAtTheWriteTimeout(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	ts := h2LiveServer(t, src, 500*time.Millisecond, func(h *liveHandler) { h.writeTimeout = 250 * time.Millisecond }, nil)
	h2Stall(t, ts, src)
	waitFor(t, 15*time.Second, func() bool { return feedCancelled(src) })
}

// When the feed ends (logout, act-as end, shutdown) behind a blocked write, the
// handler is released within the connection write timeout although the stream
// timeout is long: Interrupt alone cannot get past the blocked socket write.
func TestLiveStreamH2StalledClientIsCutAfterTheFeedEnds(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	ts := h2LiveServer(t, src, 1*time.Second, func(h *liveHandler) { h.writeTimeout = time.Hour }, nil)
	h2Stall(t, ts, src)
	if feedCancelled(src) {
		t.Fatal("the handler ended before the feed did: the stall was not reached")
	}
	close(src.changes)
	waitFor(t, 15*time.Second, func() bool { return feedCancelled(src) })
}

// The response headers of a new stream are written before any per-stream
// timeout exists. Behind a stalled connection that first flush blocks too; the
// connection write timeout releases it and closes the connection.
func TestLiveStreamH2InitialHeadersBehindAStalledConnection(t *testing.T) {
	d := Dashboard{Workers: []WorkerRow{wPrivate}}
	pad := strings.Repeat("x", 1024)
	for i := 0; i < 6000; i++ {
		d.Sessions = append(d.Sessions, SessionRow{WorkerID: "w1", Worker: "laptop", Name: fmt.Sprintf("s%d", i), Task: pad, Online: true})
	}
	src := newFakeSource(d, Dashboard{})
	var once sync.Once
	secondBegan := make(chan struct{})
	secondDone := make(chan struct{})
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("second") != "" {
				once.Do(func() { close(secondBegan) })
				defer close(secondDone)
			}
			next.ServeHTTP(w, r)
		})
	}
	// The stream timeout is long: only the connection write timeout can end the
	// second stream, whose response headers have no stream timeout at all.
	ts := h2LiveServer(t, src, 600*time.Millisecond, func(h *liveHandler) { h.writeTimeout = time.Hour }, wrap)
	c := stallh2.Dial(t, h2Addr(ts))
	c.Get(1, pathLiveDashboard, aliceHdr)
	c.ReadHeaders(1, 3*time.Second)
	// The first stream's 6 MiB picture now sits behind the unread socket.
	waitFor(t, 3*time.Second, func() bool { return len(src.asked()) >= 2 })
	c.Get(3, pathLiveDashboard+"?second=1", aliceHdr)
	select {
	case <-secondBegan:
	case <-time.After(3 * time.Second):
		t.Fatal("the second stream did not start")
	}
	select {
	case <-secondDone:
		t.Fatal("the second stream ended at once: its initial flush was not behind a stalled connection")
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-secondDone:
	case <-time.After(15 * time.Second):
		t.Fatal("the second stream's initial flush stayed blocked behind the stalled connection")
	}
	waitFor(t, 15*time.Second, func() bool { return feedCancelled(src) })
}

// The connection write timeout must not cut a healthy long-lived stream: it
// runs only while data waits to be written, so an idle period longer than the
// timeout (the feed is quiet) changes nothing, and the stream still delivers.
func TestLiveStreamH2HealthyIdleStreamSurvivesTheWriteTimeout(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	ts := h2LiveServer(t, src, 300*time.Millisecond, func(h *liveHandler) { h.heartbeat = 400 * time.Millisecond }, nil)
	c := stallh2.Dial(t, h2Addr(ts))
	c.Get(1, pathLiveDashboard, aliceHdr)
	if !c.Drain(3*time.Second, func(b []byte) bool { return bytes.Contains(b, []byte("laptop")) }) {
		t.Fatal("no first fragment")
	}
	// Idle for five times the write timeout, reading (heartbeats only).
	if c.Drain(1500*time.Millisecond, func(b []byte) bool { return bytes.Contains(b, []byte("fix-tests")) }) {
		t.Fatal("a fragment arrived without a change")
	}
	src.set(Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}})
	src.changes <- struct{}{}
	if !c.Drain(3*time.Second, func(b []byte) bool { return bytes.Contains(b, []byte("fix-tests")) }) {
		t.Fatal("the stream did not deliver after an idle period longer than the connection write timeout")
	}
}
