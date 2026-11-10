package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// fakeSource is a LiveSource with a settable picture and a change channel.
type fakeSource struct {
	mu        sync.Mutex
	mine, all Dashboard
	snapErr   error
	changeErr error
	askedAll  []bool // the "all" flag of every Snapshot and Changes call
	changes   chan struct{}
	changeCtx context.Context
}

func newFakeSource(mine, all Dashboard) *fakeSource {
	return &fakeSource{mine: mine, all: all, changes: make(chan struct{}, 8)}
}

func (f *fakeSource) Snapshot(_ *http.Request, all bool) (Dashboard, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.askedAll = append(f.askedAll, all)
	if f.snapErr != nil {
		return Dashboard{}, f.snapErr
	}
	if all {
		return f.all, nil
	}
	return f.mine, nil
}

func (f *fakeSource) Changes(ctx context.Context, _ *http.Request, all bool) (<-chan struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.askedAll = append(f.askedAll, all)
	f.changeCtx = ctx
	if f.changeErr != nil {
		return nil, f.changeErr
	}
	return f.changes, nil
}

func (f *fakeSource) set(mine Dashboard) {
	f.mu.Lock()
	f.mine = mine
	f.mu.Unlock()
}

func (f *fakeSource) asked() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.askedAll...)
}

var (
	wPrivate = WorkerRow{ID: "w1", Name: "laptop", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true, Version: "1.0"}
	wInfra   = WorkerRow{ID: "w2", Name: "buildbox", Owner: "bob", Kind: ops.WorkerKindInfrastructure, Online: false}
	sAgent   = SessionRow{WorkerID: "w1", Worker: "laptop", Owner: "alice", Name: "fix-tests", Kind: "agent:claude", State: "busy", Task: "fix the flaky test", Online: true}
	sShell   = SessionRow{WorkerID: "w2", Worker: "buildbox", Owner: "bob", Name: "ci", Kind: "shell", State: "idle", Online: false}
)

func dashboardServer(t *testing.T, src LiveSource) *Server {
	t.Helper()
	reg := NewRegistry()
	get := func() LiveSource { return src }
	if err := registerDashboard(reg, get, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	return newServer(t, reg)
}

func page(t *testing.T, h http.Handler, path, user string) (int, string) {
	t.Helper()
	rec := get(t, h, path, "X-Test-Session", user)
	return rec.Code, rec.Body.String()
}

// AC 1: a signed-in user sees their Workers (name, online, kind badge,
// version) and sessions (name, kind, state, task), and the page connects an
// SSE stream that swaps the dashboard fragment.
func TestDashboardUserListsWorkersAndSessions(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}, Dashboard{})
	s := dashboardServer(t, src)
	code, body := page(t, s, "/", "alice:user")
	if code != http.StatusOK {
		t.Fatalf("GET / = %d\n%s", code, body)
	}
	mustHave(t, body,
		`<th scope="row"><a href="/workers/w1" data-control="name" data-worker-link>laptop</a></th>`,
		`<span class="badge" data-online="true">online</span>`,
		`<span class="badge plain" data-kind="private">private</span>`,
		`<td data-label="Protocol">1.0</td>`,
		`<th scope="row"><a href="/s/laptop/fix-tests" data-control="name" data-inspect data-inspect-label="fix-tests on laptop">fix-tests</a></th>`,
		`<td data-label="Kind">agent:claude</td>`,
		`<span class="badge" data-state="busy">Working</span>`,
		`<td data-label="Task">fix the flaky test</td>`,
		`<div id="dashboard" class="stack" data-live-list sse-connect="/live/dashboard" sse-swap="dashboard" hx-swap="innerHTML"`,
		`<title>Control center - Minion</title>`)
	// A user has no filter and no owner column.
	mustNotHave(t, body, `class="scope"`, `>Owner<`, `>All users<`)
	if got := src.asked(); len(got) != 1 || got[0] {
		t.Fatalf("source asked with all=%v, want one call with false", got)
	}
}

// R48: a user cannot widen the list, whatever the query says.
func TestDashboardUserCannotAskForAll(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{Workers: []WorkerRow{wPrivate, wInfra}})
	s := dashboardServer(t, src)
	_, body := page(t, s, "/?scope=all", "alice:user")
	mustNotHave(t, body, "buildbox", `class="scope"`)
	mustHave(t, body, `sse-connect="/live/dashboard"`)
	if got := src.asked(); len(got) != 1 || got[0] {
		t.Fatalf("source asked with all=%v, want false", got)
	}
}

func TestDashboardShowsOfflineWorkerAndSession(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wInfra}, Sessions: []SessionRow{sShell}}, Dashboard{})
	s := dashboardServer(t, src)
	_, body := page(t, s, "/", "alice:user")
	mustHave(t, body,
		`<span class="badge muted" data-online="false">offline</span>`,
		`<span class="badge plain" data-kind="infrastructure">infrastructure</span>`,
		`<td data-label="Protocol"><span class="muted">-</span></td>`)
	// The session of an offline Worker is marked, and shows no task.
	mustHave(t, body, `<td data-label="Worker"><a href="/workers/w2" data-control="worker" data-worker-link>buildbox</a> <span class="badge muted" data-online="false">offline</span></td>`)
}

func TestDashboardEmptyState(t *testing.T) {
	s := dashboardServer(t, newFakeSource(Dashboard{}, Dashboard{}))
	_, body := page(t, s, "/", "alice:user")
	mustHave(t, body, `No workers connected`, `No sessions yet.`, `colspan="6"`, `<a class="btn" href="/workers/add">`)
}

// Values are escaped (AD-14): a hostile task or name stays text.
func TestDashboardEscapesValues(t *testing.T) {
	bad := SessionRow{WorkerID: "w1", Worker: `<b>w</b>`, Name: `"><img src=x>`, Kind: "shell", State: `x" onclick="y`,
		Task: `<script>alert(1)</script>`, Online: true}
	s := dashboardServer(t, newFakeSource(Dashboard{Sessions: []SessionRow{bad}}, Dashboard{}))
	_, body := page(t, s, "/", "alice:user")
	mustNotHave(t, body, `<script>alert`, `<img src=x>`, `<b>w</b>`, `onclick="y`)
	mustHave(t, body, `&lt;script&gt;alert(1)&lt;/script&gt;`)
}

// AC 2: an admin sees the switch, defaulting to "mine" without an owner column.
func TestDashboardAdminDefaultsToMine(t *testing.T) {
	src := newFakeSource(
		Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}},
		Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sShell}})
	s := dashboardServer(t, src)
	_, body := page(t, s, "/", "alice:admin")
	mustHave(t, body,
		`<nav aria-label="Whose workers and sessions" class="scope" id="scope-switch">`,
		`<a href="/" aria-current="true">Mine</a>`,
		`<a href="/?scope=all">All users</a>`,
		`sse-connect="/live/dashboard"`)
	mustNotHave(t, body, `>Owner<`, "buildbox")
	if got := src.asked(); len(got) != 1 || got[0] {
		t.Fatalf("source asked with all=%v, want false", got)
	}
}

// AC 2: "all users" adds the owner column and asks the source for everything;
// the live stream carries the same scope.
func TestDashboardAdminAllUsersShowsOwner(t *testing.T) {
	src := newFakeSource(Dashboard{},
		Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sShell}})
	s := dashboardServer(t, src)
	_, body := page(t, s, "/?scope=all", "alice:admin")
	mustHave(t, body,
		`<a href="/">Mine</a>`,
		`<a href="/?scope=all" aria-current="true">All users</a>`,
		`sse-connect="/live/dashboard?scope=all"`,
		`<th scope="col">Owner</th>`,
		`<td data-label="Owner">alice</td>`, `<td data-label="Owner">bob</td>`)
	if strings.Count(body, `<th scope="col">Owner</th>`) != 2 {
		t.Errorf("want an Owner column in both tables:\n%s", body)
	}
	if got := src.asked(); len(got) != 1 || !got[0] {
		t.Fatalf("source asked with all=%v, want true", got)
	}
}

func TestDashboardEmptyAllUsersColspan(t *testing.T) {
	s := dashboardServer(t, newFakeSource(Dashboard{}, Dashboard{}))
	_, body := page(t, s, "/?scope=all", "alice:admin")
	mustHave(t, body, `colspan="7"`)
}

// While an admin acts as another user the API sees that user, so the switch is
// not offered and the list stays "mine".
func TestDashboardActAsHasNoSwitch(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{Workers: []WorkerRow{wPrivate, wInfra}})
	layer := actAsSessions{target: "bob", expires: actAsNow.Add(time.Hour)}
	reg := NewRegistry()
	if err := registerDashboard(reg, func() LiveSource { return src }, nil); err != nil {
		t.Fatal(err)
	}
	s := actAsServer(t, layer, reg)
	body := get(t, s, "/?scope=all").Body.String()
	mustNotHave(t, body, `class="scope"`, "buildbox", `>Owner<`)
	if got := src.asked(); len(got) != 1 || got[0] {
		t.Fatalf("source asked with all=%v, want false", got)
	}
}

func TestDashboardNeedsSignIn(t *testing.T) {
	s := dashboardServer(t, newFakeSource(Dashboard{}, Dashboard{}))
	for _, p := range []string{"/", pathLiveDashboard} {
		rec := get(t, s, p)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" && !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("GET %s anonymous = %d %q, want a redirect to /login", p, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestDashboardStatuses(t *testing.T) {
	t.Run("not connected", func(t *testing.T) {
		reg := NewRegistry()
		if err := registerDashboard(reg, func() LiveSource { return nil }, nil); err != nil {
			t.Fatal(err)
		}
		s := newServer(t, reg)
		code, body := page(t, s, "/", "alice:user")
		if code != http.StatusServiceUnavailable {
			t.Fatalf("GET / = %d, want 503\n%s", code, body)
		}
		mustHave(t, body, "not connected")
		if c, _ := page(t, s, pathLiveDashboard, "alice:user"); c != http.StatusServiceUnavailable {
			t.Errorf("GET live = %d, want 503", c)
		}
	})
	t.Run("refused", func(t *testing.T) {
		src := newFakeSource(Dashboard{}, Dashboard{})
		src.snapErr = &OpError{Status: http.StatusForbidden, Code: "forbidden", Message: "no"}
		code, _ := page(t, dashboardServer(t, src), "/", "alice:user")
		if code != http.StatusNotFound {
			t.Errorf("a refused snapshot = %d, want 404", code)
		}
	})
	t.Run("failed", func(t *testing.T) {
		src := newFakeSource(Dashboard{}, Dashboard{})
		src.snapErr = errors.New("database is locked: secret detail")
		code, body := page(t, dashboardServer(t, src), "/", "alice:user")
		if code != http.StatusInternalServerError {
			t.Errorf("a failed snapshot = %d, want 500", code)
		}
		mustNotHave(t, body, "secret detail")
	})
}

func TestDashboardRegistration(t *testing.T) {
	reg := NewRegistry()
	if err := registerDashboard(reg, currentLiveSource, nil); err != nil {
		t.Fatal(err)
	}
	if err := registerDashboard(reg, currentLiveSource, nil); err == nil {
		t.Fatal("registering the home page twice must fail")
	}
	snap := reg.snapshot()
	if len(snap.pages) != 4 || snap.pages[0].Pattern != "GET /{$}" || snap.pages[0].Template != "sessions.html" ||
		snap.pages[1].Pattern != "GET "+pathWorkers || snap.pages[2].Pattern != "GET "+pathSessions ||
		snap.pages[3].Pattern != "GET "+pathWorkerPattern || snap.pages[3].Template != "worker.html" || !snap.pages[3].Inspector {
		t.Errorf("pages = %+v", snap.pages)
	}
	if len(snap.routes) != 4 || snap.routes[0].Pattern != "GET "+pathLiveDashboard ||
		snap.routes[1].Pattern != "GET "+pathLiveWorkers || snap.routes[2].Pattern != "GET "+pathLiveSessions ||
		snap.routes[3].Pattern != "GET "+pathLiveWorkerPattern {
		t.Errorf("routes = %+v", snap.routes)
	}
	// Workspace entries for every signed-in user, none of them admin-only.
	if len(snap.nav) != 3 || snap.nav[0].Path != "/" || snap.nav[0].Label != "Control center" ||
		snap.nav[1].Path != pathWorkers || snap.nav[2].Path != pathSessions {
		t.Errorf("nav = %+v", snap.nav)
	}
	for _, n := range snap.nav {
		if len(n.Roles) != 0 {
			t.Errorf("nav entry %+v is limited to roles", n)
		}
	}
	// The package registers the page for the real Hub.
	found := false
	for _, p := range DefaultRegistry.snapshot().pages {
		found = found || p.Pattern == "GET /{$}"
	}
	if !found {
		t.Error("the home page is not in DefaultRegistry")
	}
}

func TestCurrentLiveSourcePrecedence(t *testing.T) {
	SetLiveSource(nil)
	SetAdminOps(nil)
	t.Cleanup(func() { SetLiveSource(nil); SetAdminOps(nil) })
	if currentLiveSource() != nil {
		t.Fatal("nothing connected must give nil")
	}
	SetAdminOps(fakeCaller{})
	if currentLiveSource() != nil {
		t.Fatal("an OpCaller that is not a HandlerCaller gives no source")
	}
	SetAdminOps(NewHandlerCaller(http.NotFoundHandler()))
	if _, ok := currentLiveSource().(*handlerSource); !ok {
		t.Fatalf("source = %T, want the in-process source", currentLiveSource())
	}
	fs := newFakeSource(Dashboard{}, Dashboard{})
	SetLiveSource(fs)
	if currentLiveSource() != LiveSource(fs) {
		t.Fatal("an explicit source wins")
	}
}

type fakeCaller struct{}

func (fakeCaller) CallOp(*http.Request, string, any, any) error { return nil }

// ---- the live stream ----

// sseReader reads events from a stream body.
type sseReader struct {
	t  *testing.T
	br *bufio.Reader
}

type sseEvent struct {
	name, data string
	comment    bool
}

func (s *sseReader) next() (sseEvent, error) {
	var ev sseEvent
	var data []string
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			return ev, err
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if ev.name != "" || len(data) > 0 || ev.comment {
				ev.data = strings.Join(data, "\n")
				return ev, nil
			}
		case strings.HasPrefix(line, ":"):
			ev.comment = true
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
}

// nextWithin returns the next event or fails after d.
func (s *sseReader) nextWithin(d time.Duration) (sseEvent, bool) {
	s.t.Helper()
	type res struct {
		ev  sseEvent
		err error
	}
	ch := make(chan res, 1)
	go func() { ev, err := s.next(); ch <- res{ev, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			return sseEvent{}, false
		}
		return r.ev, true
	case <-time.After(d):
		s.t.Fatalf("no event within %v", d)
		return sseEvent{}, false
	}
}

func liveServer(t *testing.T, src LiveSource, debounce, heartbeat time.Duration) *httptest.Server {
	t.Helper()
	reg := NewRegistry()
	err := reg.RegisterRoute(Route{Pattern: "GET " + pathLiveDashboard,
		Handler: newLiveHandler(func() LiveSource { return src }, debounce, heartbeat, slog.New(slog.NewTextHandler(io.Discard, nil)))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newServer(t, reg))
	t.Cleanup(ts.Close)
	return ts
}

func openStream(t *testing.T, ts *httptest.Server, path, user string) (*http.Response, *sseReader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
	req.Header.Set("X-Test-Session", user)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	return resp, &sseReader{t: t, br: bufio.NewReader(resp.Body)}, cancel
}

// AC 1 (live): the stream sends the fragment on connect and after a change.
func TestLiveStreamSendsFragmentOnConnectAndOnChange(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	ts := liveServer(t, src, 5*time.Millisecond, time.Hour)
	resp, rd, _ := openStream(t, ts, pathLiveDashboard, "alice:user")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ev, _ := rd.nextWithin(2 * time.Second)
	if ev.name != "dashboard" || !strings.Contains(ev.data, `<th scope="row"><a href="/workers/w1" data-control="name" data-worker-link>laptop</a></th>`) || strings.Contains(ev.data, "fix-tests") {
		t.Fatalf("first event = %+v", ev)
	}
	src.set(Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}})
	src.changes <- struct{}{}
	ev, _ = rd.nextWithin(2 * time.Second)
	if ev.name != "dashboard" || !strings.Contains(ev.data, "fix-tests") || !strings.Contains(ev.data, "fix the flaky test") {
		t.Fatalf("second event = %+v", ev)
	}
}

// A change that the lists do not show sends nothing; a burst renders once.
func TestLiveStreamSkipsUnchangedAndMergesBursts(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	ts := liveServer(t, src, 60*time.Millisecond, time.Hour)
	_, rd, _ := openStream(t, ts, pathLiveDashboard, "alice:user")
	rd.nextWithin(2 * time.Second)
	for i := 0; i < 3; i++ { // changes with the same picture
		src.changes <- struct{}{}
	}
	src.set(Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}})
	deadline := time.After(2 * time.Second)
	for len(src.asked()) < 3 { // connect snapshot + changes + the merged render
		select {
		case <-deadline:
			t.Fatalf("calls: %v", src.asked())
		case <-time.After(5 * time.Millisecond):
		}
	}
	src.changes <- struct{}{}
	ev, _ := rd.nextWithin(2 * time.Second)
	if !strings.Contains(ev.data, "fix-tests") {
		t.Fatalf("event = %+v", ev)
	}
	snaps := len(src.asked()) - 1 // minus the Changes call
	if snaps > 4 {
		t.Errorf("%d snapshots for 4 change signals: bursts are not merged", snaps)
	}
}

func TestLiveStreamUnchangedPictureSendsNoSecondEvent(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	ts := liveServer(t, src, time.Millisecond, 40*time.Millisecond)
	_, rd, _ := openStream(t, ts, pathLiveDashboard, "alice:user")
	rd.nextWithin(2 * time.Second)
	src.changes <- struct{}{}
	// Next thing on the wire is the heartbeat, not a repeated fragment.
	ev, _ := rd.nextWithin(2 * time.Second)
	if !ev.comment || ev.name != "" {
		t.Fatalf("event = %+v, want only a keep-alive comment", ev)
	}
}

func TestLiveStreamScope(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}},
		Dashboard{Workers: []WorkerRow{wPrivate, wInfra}})
	ts := liveServer(t, src, time.Millisecond, time.Hour)

	_, rd, cancel := openStream(t, ts, pathLiveDashboard+"?scope=all", "alice:admin")
	ev, _ := rd.nextWithin(2 * time.Second)
	if !strings.Contains(ev.data, "buildbox") || !strings.Contains(ev.data, "<th scope=\"col\">Owner</th>") {
		t.Fatalf("admin all = %+v", ev)
	}
	cancel()

	_, rd, cancel = openStream(t, ts, pathLiveDashboard+"?scope=all", "alice:user")
	ev, _ = rd.nextWithin(2 * time.Second)
	if strings.Contains(ev.data, "buildbox") || strings.Contains(ev.data, ">Owner<") {
		t.Fatalf("user all = %+v", ev)
	}
	cancel()

	got := src.asked() // changes+snapshot for the admin (true,true), then the user (false,false)
	want := []bool{true, true, false, false}
	if len(got) < 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Errorf("asked = %v, want %v", got, want)
	}
}

func TestLiveStreamEndsWhenFeedEnds(t *testing.T) {
	src := newFakeSource(Dashboard{}, Dashboard{})
	ts := liveServer(t, src, time.Millisecond, time.Hour)
	_, rd, _ := openStream(t, ts, pathLiveDashboard, "alice:user")
	rd.nextWithin(2 * time.Second)
	close(src.changes)
	if ev, ok := rd.nextWithin(2 * time.Second); ok {
		t.Fatalf("got %+v after the feed ended, want the stream to end", ev)
	}
}

func TestLiveStreamCancelsFeedWhenClientLeaves(t *testing.T) {
	src := newFakeSource(Dashboard{}, Dashboard{})
	ts := liveServer(t, src, time.Millisecond, time.Hour)
	_, rd, cancel := openStream(t, ts, pathLiveDashboard, "alice:user")
	rd.nextWithin(2 * time.Second)
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		src.mu.Lock()
		ctx := src.changeCtx
		src.mu.Unlock()
		if ctx != nil && ctx.Err() != nil {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the feed context was not cancelled after the client left")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestLiveStreamFeedErrors(t *testing.T) {
	src := newFakeSource(Dashboard{}, Dashboard{})
	src.changeErr = &OpError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "too many open event streams"}
	ts := liveServer(t, src, time.Millisecond, time.Hour)
	resp, _, _ := openStream(t, ts, pathLiveDashboard, "alice:user")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
	src.changeErr = errors.New("boom: internal detail")
	resp, _, _ = openStream(t, ts, pathLiveDashboard, "alice:user")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "internal detail") {
		t.Errorf("status = %d body %q, want 502 without detail", resp.StatusCode, body)
	}
}

func TestLiveStreamEndsOnSnapshotFailure(t *testing.T) {
	src := newFakeSource(Dashboard{}, Dashboard{})
	src.snapErr = errors.New("db down")
	ts := liveServer(t, src, time.Millisecond, time.Hour)
	_, rd, _ := openStream(t, ts, pathLiveDashboard, "alice:user")
	if ev, ok := rd.nextWithin(2 * time.Second); ok && ev.name == "dashboard" {
		t.Fatalf("got a dashboard event %+v on a failed snapshot", ev)
	}
}

// ---- the in-process source against a fake REST API ----

type fakeAPI struct {
	mu       sync.Mutex
	requests []string
	cookies  []string
	workers  ops.WorkersListResult
	sessions string
	events   func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()
	note := func(r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		f.cookies = append(f.cookies, r.Header.Get("Cookie"))
	}
	mux.HandleFunc("POST "+ops.RESTPrefix+ops.OpWorkersList, func(w http.ResponseWriter, r *http.Request) {
		note(r)
		var a ops.WorkersListArgs
		_ = json.NewDecoder(r.Body).Decode(&a)
		f.mu.Lock()
		f.requests = append(f.requests, "workers_list all="+map[bool]string{true: "true", false: "false"}[a.All])
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(f.workers)
	})
	mux.HandleFunc("GET "+apiSessionsPath, func(w http.ResponseWriter, r *http.Request) {
		note(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, f.sessions)
	})
	mux.HandleFunc("GET "+apiEventsPath, func(w http.ResponseWriter, r *http.Request) {
		note(r)
		f.events(w, r)
	})
	return mux
}

func (f *fakeAPI) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func sessionContext(ctx context.Context, user, role string) context.Context {
	return session.WithSession(ctx, &session.Session{ID: "s1", UserID: "u-" + user, Username: user,
		Role: authz.Role(role), CSRFToken: "csrf-" + user})
}

func sourceRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Cookie", "minion_session=abc")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	return req.WithContext(sessionContext(req.Context(), "alice", "admin"))
}

func TestHandlerSourceSnapshotMergesWorkersAndSessions(t *testing.T) {
	api := &fakeAPI{
		workers: ops.WorkersListResult{Workers: []ops.WorkerInfo{
			{ID: "w1", Name: "laptop", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true, ProtoVersion: "1.0"},
			{ID: "w2", Name: "buildbox", Owner: "bob", Kind: ops.WorkerKindInfrastructure},
		}},
		sessions: `{"sessions":[{"worker_id":"w1","worker":"laptop","owner":"alice","session":"fix-tests","kind":"agent:claude","state":"busy","task":"t","online":true,"screen_hash":"h"},
		{"worker_id":"w2","worker":"buildbox","owner":"bob","session":"ci","kind":"shell","state":"idle","online":false}]}`,
	}
	src := NewHandlerSource(NewHandlerCaller(api.handler()))
	d, err := src.Snapshot(sourceRequest(t), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Workers) != 2 || !reflect.DeepEqual(d.Workers[0], WorkerRow{ID: "w1", Name: "laptop", Owner: "alice", Kind: "private", Online: true, Version: "1.0"}) ||
		d.Workers[1].Online || d.Workers[1].Kind != "infrastructure" {
		t.Errorf("workers = %+v", d.Workers)
	}
	if len(d.Sessions) != 2 || d.Sessions[0] != (SessionRow{WorkerID: "w1", Worker: "laptop", Owner: "alice", Name: "fix-tests", Kind: "agent:claude", State: "busy", Task: "t", Online: true}) ||
		d.Sessions[1].Online || d.Sessions[1].Name != "ci" {
		t.Errorf("sessions = %+v", d.Sessions)
	}
	log := strings.Join(api.log(), "\n")
	mustHave(t, log, "workers_list all=true", "GET /api/v1/sessions?all=true")
	for _, c := range api.cookies {
		if c != "minion_session=abc" {
			t.Errorf("cookie forwarded as %q", c)
		}
	}

	api.requests = nil
	if _, err := src.Snapshot(sourceRequest(t), false); err != nil {
		t.Fatal(err)
	}
	log = strings.Join(api.log(), "\n")
	mustHave(t, log, "workers_list all=false", "GET /api/v1/sessions")
	mustNotHave(t, log, "all=true")
}

func TestHandlerSourceSnapshotErrors(t *testing.T) {
	api := &fakeAPI{sessions: `{"error":{"code":"forbidden","message":"nope"}}`}
	denied := http.NewServeMux()
	denied.Handle("/", api.handler())
	denied.HandleFunc("GET "+apiSessionsPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, api.sessions)
	})
	_, err := NewHandlerSource(NewHandlerCaller(denied)).Snapshot(sourceRequest(t), false)
	var oe *OpError
	if !errors.As(err, &oe) || oe.Status != http.StatusForbidden || oe.Code != "forbidden" {
		t.Fatalf("err = %v, want a 403 OpError", err)
	}

	bad := &fakeAPI{sessions: `not json`}
	if _, err := NewHandlerSource(NewHandlerCaller(bad.handler())).Snapshot(sourceRequest(t), false); err == nil {
		t.Fatal("a malformed session list must fail")
	}

	// A Worker list failure comes first.
	none := http.NewServeMux()
	none.HandleFunc("POST "+ops.RESTPrefix+ops.OpWorkersList, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthenticated","message":"sign in"}}`)
	})
	_, err = NewHandlerSource(NewHandlerCaller(none)).Snapshot(sourceRequest(t), false)
	if !errors.As(err, &oe) || oe.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want 401", err)
	}
}

func eventFeed(frames ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_ = http.NewResponseController(w).Flush()
		for _, f := range frames {
			_, _ = io.WriteString(w, f)
			_ = http.NewResponseController(w).Flush()
		}
		<-r.Context().Done()
	}
}

func recvWithin(t *testing.T, ch <-chan struct{}, d time.Duration) (open, got bool) {
	t.Helper()
	select {
	case _, ok := <-ch:
		return ok, ok
	case <-time.After(d):
		return true, false
	}
}

func TestHandlerSourceChangesSignalsEvents(t *testing.T) {
	long := `data: ` + strings.Repeat("x", 20000) + "\n"
	api := &fakeAPI{events: eventFeed(
		"retry: 3000\n\n",
		"event: snapshot\n"+long+"\n",
		": keep-alive\n\n",
	)}
	src := NewHandlerSource(NewHandlerCaller(api.handler()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := src.Changes(ctx, sourceRequest(t), true)
	if err != nil {
		t.Fatal(err)
	}
	// The snapshot event (with a long data line) is one signal; comments and
	// retry lines are none.
	if _, got := recvWithin(t, ch, 2*time.Second); !got {
		t.Fatal("no signal for the snapshot event")
	}
	if _, got := recvWithin(t, ch, 100*time.Millisecond); got {
		t.Fatal("a comment or retry line caused a signal")
	}
	mustHave(t, strings.Join(api.log(), "\n"), "GET /api/v1/events?all=true")
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the channel stayed open after cancel")
		}
	}
}

func TestHandlerSourceChangesEndsOnResetAndAccessLost(t *testing.T) {
	for _, name := range []string{"reset", "access_lost"} {
		api := &fakeAPI{events: eventFeed("event: state_changed\ndata: {}\n\n", "event: "+name+"\ndata: {}\n\n")}
		src := NewHandlerSource(NewHandlerCaller(api.handler()))
		ch, err := src.Changes(context.Background(), sourceRequest(t), false)
		if err != nil {
			t.Fatal(err)
		}
		var sawOpen, sawClose bool
		deadline := time.After(2 * time.Second)
		for !sawClose {
			select {
			case _, ok := <-ch:
				if ok {
					sawOpen = true
				} else {
					sawClose = true
				}
			case <-deadline:
				t.Fatalf("%s: the channel did not close", name)
			}
		}
		if !sawOpen {
			t.Errorf("%s: no signal for the state change before the end", name)
		}
	}
}

func TestHandlerSourceChangesRefusal(t *testing.T) {
	api := &fakeAPI{events: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limited","message":"too many open event streams"}}`)
	}}
	_, err := NewHandlerSource(NewHandlerCaller(api.handler())).Changes(context.Background(), sourceRequest(t), false)
	var oe *OpError
	if !errors.As(err, &oe) || oe.Status != http.StatusTooManyRequests || oe.Code != "rate_limited" {
		t.Fatalf("err = %v, want a 429 OpError", err)
	}
}

func TestHandlerSourceChangesHandlerPanicEndsFeed(t *testing.T) {
	api := &fakeAPI{events: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic("boom")
	}}
	ch, err := NewHandlerSource(NewHandlerCaller(api.handler())).Changes(context.Background(), sourceRequest(t), false)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("the feed did not end after a handler panic")
	}
}

func TestFollowEvents(t *testing.T) {
	var n int
	input := "event: a\ndata: 1\n\n: c\n\nevent: b\r\ndata: " + strings.Repeat("y", 9000) + "\r\n\r\nevent: reset\ndata: {}\n\nevent: c\n\n"
	followEvents(strings.NewReader(input), func() { n++ })
	if n != 2 {
		t.Fatalf("signals = %d, want 2 (a, b; reset ends the stream)", n)
	}
}

// A Hub without the Worker list or the session list (for example one
// assembled for another feature) still gets a home page, with a reason.
func TestHandlerSourceSnapshotDegradesWhenNotServed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+ops.RESTPrefix+ops.OpWorkersList, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = io.WriteString(w, `{"error":{"code":"unsupported_op","message":"the operation is not available on this Hub"}}`)
	})
	mux.Handle("/", http.NotFoundHandler()) // no /api/v1/sessions either
	d, err := NewHandlerSource(NewHandlerCaller(mux)).Snapshot(sourceRequest(t), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Workers) != 0 || len(d.Sessions) != 0 || d.Notice != noticeNotServed {
		t.Fatalf("dashboard = %+v", d)
	}
	s := dashboardServer(t, newFakeSource(d, Dashboard{}))
	_, body := page(t, s, "/", "alice:user")
	mustHave(t, body, `<p class="notice notice-info" role="status">This Hub does not serve the live Worker and session lists.</p>`, "No workers connected.")
}
