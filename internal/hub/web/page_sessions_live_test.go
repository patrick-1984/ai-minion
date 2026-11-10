package web

// Real-transport regressions of the live dashboard stream (Story 5.4): every
// test below talks to the handler over a TCP connection through net/http, not
// through a recorder, and every wait is bounded.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
)

// liveServerWith is liveServer with the handler's timers set by tune.
func liveServerWith(t *testing.T, src LiveSource, tune func(*liveHandler)) *httptest.Server {
	t.Helper()
	h := newLiveHandler(func() LiveSource { return src }, 5*time.Millisecond, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.reconcile = time.Hour
	tune(h)
	reg := NewRegistry()
	if err := reg.RegisterRoute(Route{Pattern: "GET " + pathLiveDashboard, Handler: h}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newServer(t, reg))
	t.Cleanup(ts.Close)
	return ts
}

// userSource is a LiveSource whose picture depends on the signed-in user, so
// that two users' streams can be told apart. Its feed is shared by all callers.
type userSource struct {
	mu      sync.Mutex
	data    map[string]Dashboard // by user name
	changes chan struct{}
	calls   int
}

func newUserSource() *userSource {
	return &userSource{data: map[string]Dashboard{}, changes: make(chan struct{}, 8)}
}

func (u *userSource) set(user string, d Dashboard) {
	u.mu.Lock()
	u.data[user] = d
	u.mu.Unlock()
}

func (u *userSource) Snapshot(r *http.Request, _ bool) (Dashboard, error) {
	sess, _ := session.FromContext(r.Context())
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	return u.data[sess.Username], nil
}

func (u *userSource) Changes(context.Context, *http.Request, bool) (<-chan struct{}, error) {
	return u.changes, nil
}

// untilEvent reads events until pred accepts one; it fails after d.
func untilEvent(t *testing.T, rd *sseReader, d time.Duration, pred func(sseEvent) bool) sseEvent {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			t.Fatalf("no matching event within %v", d)
		}
		ev, ok := rd.nextWithin(left)
		if !ok {
			t.Fatal("the stream ended before a matching event")
		}
		if pred(ev) {
			return ev
		}
	}
}

func hasText(s string) func(sseEvent) bool {
	return func(ev sseEvent) bool { return ev.name == "dashboard" && strings.Contains(ev.data, s) }
}

// A feed that never pauses must not hold the render back: the batching window
// is fixed, not restarted by every event, and the keep-alive keeps flowing.
func TestLiveStreamRendersWhileEventsNeverPause(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	ts := liveServerWith(t, src, func(h *liveHandler) {
		h.debounce = 100 * time.Millisecond
		h.heartbeat = 150 * time.Millisecond
	})
	_, rd, _ := openStream(t, ts, pathLiveDashboard, "alice:user")
	rd.nextWithin(2 * time.Second)

	// Events faster than the window (the 100 ms screen polling of a busy
	// session), for a bounded time.
	ctx, stop := context.WithTimeout(context.Background(), 6*time.Second)
	defer stop()
	go func() {
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				select {
				case src.changes <- struct{}{}:
				default:
				}
			}
		}
	}()

	src.set(Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}})
	untilEvent(t, rd, 3*time.Second, hasText("fix-tests"))
	// A later change is rendered too, and the heartbeat is not starved.
	src.set(Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent, sShell}})
	untilEvent(t, rd, 3*time.Second, hasText(`data-session="ci"`))
	untilEvent(t, rd, 3*time.Second, func(ev sseEvent) bool { return ev.comment && ev.name == "" })
}

// Changes that no event announces (a Worker renamed, removed or reassigned
// while offline) reach the browser on the periodic reconciliation, in the
// scope of each user.
func TestLiveStreamReconcilesCatalogChangesWithoutEvents(t *testing.T) {
	laptop := WorkerRow{ID: "w1", Name: "laptop", Owner: "alice", Kind: "private"}
	src := newUserSource()
	src.set("alice", Dashboard{Workers: []WorkerRow{laptop}})
	src.set("bob", Dashboard{})
	ts := liveServerWith(t, src, func(h *liveHandler) { h.reconcile = 40 * time.Millisecond })
	_, alice, _ := openStream(t, ts, pathLiveDashboard, "alice:user")
	_, bob, _ := openStream(t, ts, pathLiveDashboard, "bob:user")
	first, _ := alice.nextWithin(2 * time.Second)
	if !strings.Contains(first.data, "laptop") {
		t.Fatalf("alice first = %+v", first)
	}
	if ev, _ := bob.nextWithin(2 * time.Second); strings.Contains(ev.data, "laptop") {
		t.Fatalf("bob sees alice's Worker: %+v", ev)
	}

	// Rename (offline): no event is sent, the stream still catches up.
	laptop.Name = "workstation"
	src.set("alice", Dashboard{Workers: []WorkerRow{laptop}})
	untilEvent(t, alice, 3*time.Second, hasText("workstation"))

	// Reassignment to bob: alice's row goes, bob's appears.
	laptop.Owner = "bob"
	src.set("alice", Dashboard{})
	src.set("bob", Dashboard{Workers: []WorkerRow{laptop}})
	untilEvent(t, alice, 3*time.Second, func(ev sseEvent) bool {
		return ev.name == "dashboard" && !strings.Contains(ev.data, "workstation") && strings.Contains(ev.data, "No workers connected")
	})
	untilEvent(t, bob, 3*time.Second, hasText("workstation"))

	// Removal: bob's row goes.
	src.set("bob", Dashboard{})
	untilEvent(t, bob, 3*time.Second, func(ev sseEvent) bool {
		return ev.name == "dashboard" && !strings.Contains(ev.data, "workstation")
	})
}

// rawConn opens a TCP connection to ts, with a small receive buffer so that
// a client that does not read fills the socket quickly.
func rawConn(t *testing.T, ts *httptest.Server) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", strings.TrimPrefix(ts.URL, "http://"), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4096)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func liveRequest(extra string) string {
	return "GET " + pathLiveDashboard + " HTTP/1.1\r\nHost: hub.test\r\nX-Test-Session: alice:user\r\n" + extra + "\r\n"
}

// bigDashboard is a picture of about 1 MiB that differs for every n.
func bigDashboard(n int) Dashboard {
	d := Dashboard{Workers: []WorkerRow{wPrivate}}
	pad := strings.Repeat("x", 200)
	for i := 0; i < 3000; i++ {
		d.Sessions = append(d.Sessions, SessionRow{WorkerID: "w1", Worker: "laptop", Name: fmt.Sprintf("s%d", i),
			Kind: "shell", State: "idle", Task: fmt.Sprintf("round %d %s", n, pad), Online: true})
	}
	return d
}

// stallStream opens the stream on a client that never reads, then feeds big
// pictures until the handler is blocked in a write. It returns once blocked.
func stallStream(t *testing.T, ts *httptest.Server, src *fakeSource) {
	t.Helper()
	c := rawConn(t, ts)
	if _, err := io.WriteString(c, liveRequest("")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return len(src.asked()) >= 2 }) // Changes + first snapshot
	driveUntilBlocked(t, src)
}

// driveUntilBlocked feeds big pictures to the stream that src serves until the
// handler no longer takes them: it is blocked in a write.
func driveUntilBlocked(t *testing.T, src *fakeSource) {
	t.Helper()
	quiet := 0
	for n := 1; n <= 80; n++ {
		before := len(src.asked())
		src.set(bigDashboard(n))
		src.changes <- struct{}{}
		if waitForOK(600*time.Millisecond, func() bool { return len(src.asked()) > before }) {
			quiet = 0
			continue
		}
		if quiet++; quiet == 2 {
			return // two silent rounds in a row: the handler cannot write
		}
		src.changes <- struct{}{}
	}
	t.Fatal("the handler never blocked: the socket buffers did not fill")
}

func waitForOK(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	if !waitForOK(d, cond) {
		t.Fatalf("condition not met within %v", d)
	}
}

func feedCancelled(src *fakeSource) bool {
	src.mu.Lock()
	defer src.mu.Unlock()
	return src.changeCtx != nil && src.changeCtx.Err() != nil
}

// A client that stops reading must not hold the stream for ever: the write
// is bounded and the handler ends.
func TestLiveStreamStalledClientIsCutAtTheWriteTimeout(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	ts := liveServerWith(t, src, func(h *liveHandler) { h.writeTimeout = 400 * time.Millisecond })
	stallStream(t, ts, src)
	waitFor(t, 5*time.Second, func() bool { return feedCancelled(src) })
}

// When the feed behind the stream ends (logout, act-as expiry, shutdown) while
// a write is blocked, the write is cut at once even though the write timeout
// is long: the handler cannot see the end of the feed while it is blocked.
func TestLiveStreamStalledClientIsCutWhenTheFeedEnds(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	ts := liveServerWith(t, src, func(h *liveHandler) { h.writeTimeout = time.Hour })
	stallStream(t, ts, src)
	if feedCancelled(src) {
		t.Fatal("the handler ended before the feed did: the stall was not reached")
	}
	close(src.changes)
	waitFor(t, 5*time.Second, func() bool { return feedCancelled(src) })
}

// A GET that announces a body and never sends it must not block the start of
// the stream: the body is bounded on the original request, before the feed is
// subscribed.
func TestLiveStreamAnnouncedButWithheldBody(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	ts := liveServerWith(t, src, func(h *liveHandler) { h.bodyTimeout = 250 * time.Millisecond })
	c := rawConn(t, ts)
	if _, err := io.WriteString(c, liveRequest("Content-Length: 1\r\n")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no answer to a GET with a withheld body: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !resp.Close {
		t.Errorf("status %d close %v, want 400 and a closing connection", resp.StatusCode, resp.Close)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("answered after %v", time.Since(start))
	}
	if got := src.asked(); len(got) != 0 {
		t.Errorf("the feed was subscribed (%v) before the body was bounded", got)
	}
}

// A body that does arrive is read and discarded, and the stream starts.
func TestLiveStreamAnnouncedBodyThatArrivesIsAccepted(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	ts := liveServerWith(t, src, func(h *liveHandler) { h.bodyTimeout = 2 * time.Second })
	c := rawConn(t, ts)
	if _, err := io.WriteString(c, liveRequest("Content-Length: 1\r\n")+"x"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	// No resp.Body.Close(): closing a chunked stream body would read to its end.
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	rd := &sseReader{t: t, br: bufio.NewReader(resp.Body)}
	untilEvent(t, rd, 3*time.Second, hasText("laptop"))
}
