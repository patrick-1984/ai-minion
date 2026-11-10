package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/web"
	"github.com/patrick-1984/ai-minion/internal/testutil/stallh2"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The Hub's HTTP server bounds HTTP/2 connection writes: a client that stops
// reading its TCP socket would otherwise hold every streaming handler (the
// event feed, the dashboard) for ever, because net/http queues a stream
// deadline's RST_STREAM behind the blocked socket write.
func TestHTTPServerHTTP2WriteByteTimeoutIsSet(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler(), discardLogger(), http2WriteByteTimeout)
	if srv.HTTP2 == nil || srv.HTTP2.WriteByteTimeout != http2WriteByteTimeout || http2WriteByteTimeout <= 0 {
		t.Fatalf("HTTP2 = %+v, want WriteByteTimeout %v", srv.HTTP2, http2WriteByteTimeout)
	}
	// Long enough for a slow healthy client, short enough to free a dead one.
	if http2WriteByteTimeout < 10*time.Second || http2WriteByteTimeout > 2*time.Minute {
		t.Errorf("http2WriteByteTimeout = %v, want between 10 s and 2 min", http2WriteByteTimeout)
	}
}

// The server has timeouts for the headers, for idle connections and for HTTP/2
// writes, and none that covers a whole connection or a whole request: a read or
// write timeout would kill the Worker's WebSocket and the event streams. The
// time a client may take to send a request body is a deadline of its own that
// only requests with a body get (internal/hub/httpbody).
func TestHTTPServerHasNoBlanketConnectionDeadline(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler(), discardLogger(), http2WriteByteTimeout)
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Fatalf("ReadTimeout = %v, WriteTimeout = %v: a blanket deadline kills long-lived channels", srv.ReadTimeout, srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v, IdleTimeout = %v", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
	if requestBodyTimeout < 5*time.Second || requestBodyTimeout > 2*time.Minute {
		t.Errorf("requestBodyTimeout = %v, want between 5 s and 2 min", requestBodyTimeout)
	}
}

// h2Hub serves handler over TLS with HTTP/2 on the Hub's own server
// configuration, with a short write-byte timeout.
func h2Hub(t *testing.T, handler http.Handler, writeByteTimeout time.Duration) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(handler)
	ts.Config = newHTTPServer(handler, discardLogger(), writeByteTimeout)
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func waitUntilTrue(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %v: %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Streaming handlers on the production SSE type, behind a client that reads
// the first response headers and then nothing: blocked event writes, a
// feed-closure interrupt, and the initial headers of a second stream all end
// within the connection write timeout (plus the five seconds that crypto/tls
// allows its close_notify).
func TestHubHTTP2StalledClientReleasesStreamingHandlers(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	var sent atomic.Int64
	var mu sync.Mutex
	done := map[string]chan struct{}{"/first": make(chan struct{}), "/second": make(chan struct{})}
	began := make(chan struct{})
	interrupt := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		d := done[r.URL.Path]
		mu.Unlock()
		defer close(d)
		if r.URL.Path == "/second" {
			close(began)
		}
		out, err := web.NewSSE(w) // the initial header flush
		if err != nil {
			return
		}
		// The stream timeout is long: only the connection timeout may end it.
		out.SetWriteTimeout(time.Hour)
		defer out.Finish()
		go func() { // the feed ends: Interrupt, as the dashboard does
			<-interrupt
			out.Interrupt()
		}()
		for {
			if out.Send(web.Message{Event: "x", Data: big}) != nil {
				return
			}
			sent.Add(1)
		}
	})
	ts := h2Hub(t, handler, 700*time.Millisecond)
	c := stallh2.Dial(t, strings.TrimPrefix(ts.URL, "https://"))
	c.Get(1, "/first", nil)
	c.ReadHeaders(1, 3*time.Second)

	// Wait until the first handler is blocked: no message goes out any more.
	var last int64 = -1
	stable := 0
	waitUntilTrue(t, 20*time.Second, "the first handler to block in a socket write", func() bool {
		now := sent.Load()
		if now > 0 && now == last {
			stable++
		} else {
			stable = 0
		}
		last = now
		return stable >= 40 // 200 ms without progress
	})
	select {
	case <-done["/first"]:
		t.Fatal("the first handler ended before it was released")
	default:
	}

	// A second stream starts behind the stalled connection; its response
	// headers have no stream timeout. The feed of the first one ends.
	c.Get(3, "/second", nil)
	select {
	case <-began:
	case <-time.After(3 * time.Second):
		t.Fatal("the second stream did not start")
	}
	close(interrupt) // Interrupt of the first stream cannot pass the blocked write
	for _, p := range []string{"/first", "/second"} {
		select {
		case <-done[p]:
		case <-time.After(15 * time.Second):
			t.Fatalf("the handler of %s stayed blocked behind the stalled connection", p)
		}
	}
}
