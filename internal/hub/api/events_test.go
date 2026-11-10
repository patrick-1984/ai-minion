package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// ---------------------------------------------------------------------------
// Fakes

const (
	evAlice = "u-alice"
	evBob   = "u-bob"
	evRoot  = "u-root"

	wAlice = "w-alice"
	wBob   = "w-bob"
	wRoot  = "w-root"
)

// evWorld is the users and Workers behind the authorizer and the directory.
type evWorld struct {
	mu     sync.Mutex
	users  map[string]authz.UserState
	owners map[string]string // worker id -> owner
	names  map[string]string // worker id -> name
}

func newEvWorld() *evWorld {
	return &evWorld{
		users: map[string]authz.UserState{
			evAlice: {Role: authz.RoleUser, Active: true},
			evBob:   {Role: authz.RoleUser, Active: true},
			evRoot:  {Role: authz.RoleAdmin, Active: true},
		},
		owners: map[string]string{wAlice: evAlice, wBob: evBob, wRoot: evRoot},
		names:  map[string]string{wAlice: "pi-1", wBob: "box", wRoot: "nuc"},
	}
}

func (w *evWorld) User(_ context.Context, id string) (authz.UserState, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	u, ok := w.users[id]
	if !ok {
		return authz.UserState{}, authz.ErrNotFound
	}
	return u, nil
}

func (w *evWorld) WorkerOwner(_ context.Context, id string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	o, ok := w.owners[id]
	if !ok {
		return "", authz.ErrNotFound
	}
	return o, nil
}

func (w *evWorld) Workers(context.Context) ([]workers.WorkerRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []workers.WorkerRecord
	for id, owner := range w.owners {
		out = append(out, workers.WorkerRecord{ID: id, Name: w.names[id], OwnerID: owner, Kind: "private"})
	}
	return out, nil
}

func (w *evWorld) set(f func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f()
}

// evAuth is an Authn with credentials the test can revoke.
type evAuth struct {
	mu    sync.Mutex
	creds map[string]Identity
	calls int
}

func newEvAuth() *evAuth { return &evAuth{creds: map[string]Identity{}} }

func (a *evAuth) grant(cred string, p authz.Principal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.creds[cred] = Identity{Principal: p, Actor: p.UserID, Via: ViaSession}
}

func (a *evAuth) revoke(cred string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.creds, cred)
}

func (a *evAuth) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *evAuth) Authenticate(r *http.Request) (Identity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	id, ok := a.creds[r.Header.Get("X-Cred")]
	if !ok {
		return Identity{}, unauthenticated("sign in first")
	}
	return id, nil
}

// evScope is an authz.Scope that grants read on the listed resources.
type evScope struct {
	workers  map[string]bool
	sessions map[[2]string]bool
}

func (s evScope) Actions(res authz.ResolvedResource) []ops.Action {
	switch {
	case res.Kind == authz.ResourceWorker && s.workers[res.WorkerID]:
		return []ops.Action{ops.ActionRead}
	case res.Kind == authz.ResourceSession && (s.workers[res.WorkerID] || s.sessions[[2]string{res.WorkerID, res.SessionName}]):
		return []ops.Action{ops.ActionRead}
	}
	return nil
}

// evClock fires timers when the test advances it.
type evClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []evTimer
}

type evTimer struct {
	at time.Time
	ch chan time.Time
}

func newEvClock() *evClock { return &evClock{now: time.Unix(1_800_000_000, 0)} }

func (c *evClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, evTimer{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *evClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

func (c *evClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	rest := c.timers[:0]
	for _, t := range c.timers {
		if !t.at.After(c.now) {
			t.ch <- c.now
		} else {
			rest = append(rest, t)
		}
	}
	c.timers = rest
}

// rig wires a handler set on a world, an event hub and a fake clock.
type evRig struct {
	t      *testing.T
	world  *evWorld
	auth   *evAuth
	az     *authz.Authorizer
	hub    *workers.EventHub
	clock  *evClock
	lister *SessionLister
	srv    *httptest.Server
	events *EventsHandler
}

func newEvRig(t *testing.T, mutate func(*EventsConfig)) *evRig {
	t.Helper()
	world := newEvWorld()
	r := &evRig{t: t, world: world, auth: newEvAuth(), hub: workers.NewEventHub(workers.EventHubOptions{}), clock: newEvClock()}
	r.az = authz.New(world, world)
	r.lister = &SessionLister{Directory: world, State: r.hub, Authz: r.az, OwnerName: func(_ context.Context, id string) string {
		return map[string]string{evAlice: "alice", evBob: "bob", evRoot: "root"}[id]
	}}
	cfg := EventsConfig{Auth: r.auth, Authz: r.az, Bus: r.hub, Lister: r.lister, Owners: world, Clock: r.clock,
		Log: testLogger(&strings.Builder{})}
	if mutate != nil {
		mutate(&cfg)
	}
	var err error
	r.events, err = NewEventsHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := NewSessionsHandler(r.auth, r.lister)
	if err != nil {
		t.Fatal(err)
	}
	sh.BodyTimeout = cfg.BodyTimeout
	mux := http.NewServeMux()
	mux.Handle("/api/v1/sessions", sh)
	mux.Handle("/api/v1/events", r.events)
	r.srv = httptest.NewServer(mux)
	t.Cleanup(func() { r.events.Close(); r.srv.Close() })
	r.auth.grant("alice", authz.NewSessionPrincipal(evAlice))
	r.auth.grant("bob", authz.NewSessionPrincipal(evBob))
	r.auth.grant("root", authz.NewSessionPrincipal(evRoot))
	return r
}

// feed connects a Worker to the hub and loads its sessions.
func (r *evRig) feed(workerID string, sessions ...workers.SessionState) (*workers.Link, *workers.Feed) {
	r.t.Helper()
	l := r.hub.Connect(workerID, r.world.names[workerID])
	f, err := l.NewFeed()
	if err != nil {
		r.t.Fatal(err)
	}
	if err := f.Snapshot(0, sessions); err != nil {
		r.t.Fatal(err)
	}
	return l, f
}

func ss(name, kind, state, task string) workers.SessionState {
	return workers.SessionState{Name: name, Kind: kind, State: state, Task: task}
}

func (r *evRig) get(cred, path string) (int, []byte) {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+path, nil)
	if cred != "" {
		req.Header.Set("X-Cred", cred)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (r *evRig) sessions(cred, query string) []SessionInfo {
	r.t.Helper()
	code, body := r.get(cred, "/api/v1/sessions"+query)
	if code != 200 {
		r.t.Fatalf("%s: %d %s", query, code, body)
	}
	var res SessionsResult
	if err := json.Unmarshal(body, &res); err != nil {
		r.t.Fatal(err)
	}
	return res.Sessions
}

func sessionNames(list []SessionInfo) string {
	var n []string
	for _, s := range list {
		n = append(n, s.Worker+":"+s.Session)
	}
	return strings.Join(n, ",")
}

// ---------------------------------------------------------------------------
// GET /api/v1/sessions

func TestSessionsListsTheCallersSessionsWithAllFields(t *testing.T) {
	r := newEvRig(t, nil)
	r.feed(wAlice, ss("minion-1", "agent:claude", "busy", "fix the build"), ss("minion-2", "shell", "stable", ""))
	r.feed(wBob, ss("bobs", "shell", "stable", ""))

	got := r.sessions("alice", "")
	if sessionNames(got) != "pi-1:minion-1,pi-1:minion-2" {
		t.Fatalf("alice sees %s", sessionNames(got))
	}
	want := SessionInfo{WorkerID: wAlice, Worker: "pi-1", OwnerID: evAlice, Owner: "alice", Session: "minion-1",
		Kind: "agent:claude", State: "busy", Task: "fix the build", Online: true}
	if got[0] != want {
		t.Fatalf("entry = %+v, want %+v", got[0], want)
	}
	// bob never sees alice's sessions, not even with all=true.
	if n := sessionNames(r.sessions("bob", "?all=true")); n != "box:bobs" {
		t.Fatalf("bob sees %s", n)
	}
	// An empty answer is an empty array, not null.
	r.auth.grant("carol", authz.NewSessionPrincipal(evAlice))
	_, body := r.get("bob", "/api/v1/sessions?worker=pi-1")
	if !strings.Contains(string(body), `"sessions":[]`) {
		t.Fatalf("body = %s", body)
	}
}

func TestSessionsAdminDefaultIsMine(t *testing.T) {
	r := newEvRig(t, nil)
	r.feed(wAlice, ss("a", "shell", "stable", ""))
	r.feed(wRoot, ss("r", "shell", "stable", ""))
	if n := sessionNames(r.sessions("root", "")); n != "nuc:r" {
		t.Fatalf("default filter: %s", n)
	}
	if n := sessionNames(r.sessions("root", "?all=true")); n != "nuc:r,pi-1:a" {
		t.Fatalf("all=true: %s", n)
	}
	if n := sessionNames(r.sessions("root", "?all=false")); n != "nuc:r" {
		t.Fatalf("all=false: %s", n)
	}
}

func TestSessionsOfflineWorkersKeepTheirLastState(t *testing.T) {
	r := newEvRig(t, nil)
	l, _ := r.feed(wAlice, ss("a", "shell", "stable", ""))
	r.feed(wRoot, ss("r", "shell", "stable", ""))
	l.Disconnect()

	got := r.sessions("root", "?all=true")
	byName := map[string]bool{}
	for _, s := range got {
		byName[s.Session] = s.Online
	}
	if len(got) != 2 || byName["a"] || !byName["r"] {
		t.Fatalf("%+v", got)
	}
	if n := sessionNames(r.sessions("root", "?all=true&online=false")); n != "pi-1:a" {
		t.Fatalf("online=false: %s", n)
	}
	if n := sessionNames(r.sessions("root", "?all=true&online=true")); n != "nuc:r" {
		t.Fatalf("online=true: %s", n)
	}
}

func TestSessionsFilters(t *testing.T) {
	r := newEvRig(t, nil)
	r.feed(wAlice, ss("c", "agent:claude", "busy", ""), ss("x", "agent:codex", "stable", ""), ss("sh", "shell", "stable", ""))
	r.feed(wRoot, ss("r", "agent:claude", "dialog", ""))
	for query, want := range map[string]string{
		"?all=true&kind=agent":        "nuc:r,pi-1:c,pi-1:x",
		"?all=true&kind=agent:codex":  "pi-1:x",
		"?all=true&kind=shell":        "pi-1:sh",
		"?all=true&state=stable":      "pi-1:sh,pi-1:x",
		"?all=true&state=dialog":      "nuc:r",
		"?all=true&worker=nuc":        "nuc:r",
		"?all=true&worker=" + wAlice:  "pi-1:c,pi-1:sh,pi-1:x",
		"?all=true&worker=nope":       "",
		"?kind=agent&state=busy":      "",
		"?all=true&future_param=1&k=": "nuc:r,pi-1:c,pi-1:sh,pi-1:x",
	} {
		if got := sessionNames(r.sessions("root", query)); got != want {
			t.Errorf("%s: got %q, want %q", query, got, want)
		}
	}
}

func TestSessionsAreFilteredByKeyScope(t *testing.T) {
	r := newEvRig(t, nil)
	r.feed(wAlice, ss("a1", "shell", "stable", ""), ss("a2", "shell", "stable", ""))
	r.feed(wRoot, ss("r1", "shell", "stable", ""), ss("r2", "shell", "stable", ""))
	// A key of the admin scoped to one session of one Worker and a whole other one.
	r.auth.grant("key", authz.NewAPIKeyPrincipal(evRoot, evScope{
		workers:  map[string]bool{wAlice: true},
		sessions: map[[2]string]bool{{wRoot, "r2"}: true},
	}, ""))
	if n := sessionNames(r.sessions("key", "?all=true")); n != "nuc:r2,pi-1:a1,pi-1:a2" {
		t.Fatalf("scoped key sees %s", n)
	}
	// Bob's key has a scope for alice's Worker, but bob may not read it.
	r.auth.grant("bobkey", authz.NewAPIKeyPrincipal(evBob, evScope{workers: map[string]bool{wAlice: true}}, ""))
	if n := sessionNames(r.sessions("bobkey", "?all=true")); n != "" {
		t.Fatalf("bob's key sees %s", n)
	}
}

func TestSessionsExcludeRemovedAndUnknownWorkers(t *testing.T) {
	r := newEvRig(t, nil)
	r.feed(wAlice, ss("a", "shell", "stable", ""))
	r.world.set(func() { delete(r.world.owners, wAlice) }) // removed from the directory
	if n := sessionNames(r.sessions("root", "?all=true")); n != "" {
		t.Fatalf("removed Worker listed: %s", n)
	}
	// A Worker in the directory with no event stream yet has no sessions.
	if n := sessionNames(r.sessions("bob", "")); n != "" {
		t.Fatalf("%s", n)
	}
}

func TestSessionsErrors(t *testing.T) {
	r := newEvRig(t, nil)
	if code, body := r.get("", "/api/v1/sessions"); code != 401 || !strings.Contains(string(body), `"code":"unauthenticated"`) {
		t.Fatalf("anonymous: %d %s", code, body)
	}
	for _, q := range []string{"?all=maybe", "?online=2", "?worker=" + strings.Repeat("x", 300)} {
		if code, body := r.get("alice", "/api/v1/sessions"+q); code != 400 || !strings.Contains(string(body), `"invalid_argument"`) {
			t.Fatalf("%s: %d %s", q, code, body)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/api/v1/sessions", nil)
	req.Header.Set("X-Cred", "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET" {
		t.Fatalf("POST: %d", resp.StatusCode)
	}
	// A deactivated user gets nothing.
	r.feed(wAlice, ss("a", "shell", "stable", ""))
	r.world.set(func() { r.world.users[evAlice] = authz.UserState{Role: authz.RoleUser, Active: false} })
	if n := sessionNames(r.sessions("alice", "")); n != "" {
		t.Fatalf("deactivated user sees %s", n)
	}
	if _, err := NewSessionsHandler(nil, r.lister); err == nil {
		t.Fatal("handler without authenticator")
	}
	if _, err := NewSessionsHandler(r.auth, &SessionLister{}); err == nil {
		t.Fatal("handler with an empty lister")
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/events

type sseMsg struct {
	event, data string
	comment     bool
}

// sseClient reads an event stream.
type sseClient struct {
	t      *testing.T
	resp   *http.Response
	msgs   chan sseMsg
	cancel context.CancelFunc
}

func (r *evRig) open(cred, query string) *sseClient {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, r.srv.URL+"/api/v1/events"+query, nil)
	req.Header.Set("X-Cred", cred)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		r.t.Fatal(err)
	}
	c := &sseClient{t: r.t, resp: resp, msgs: make(chan sseMsg, 256), cancel: cancel}
	r.t.Cleanup(func() { cancel(); resp.Body.Close() })
	if resp.StatusCode != 200 {
		return c
	}
	go func() {
		defer close(c.msgs)
		sc := bufio.NewScanner(resp.Body)
		var cur sseMsg
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.event != "" || cur.data != "" || cur.comment {
					c.msgs <- cur
				}
				cur = sseMsg{}
			case strings.HasPrefix(line, ":"):
				cur.comment = true
			case strings.HasPrefix(line, "event: "):
				cur.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data += strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return c
}

// next returns the next message that is not a heartbeat comment.
func (c *sseClient) next() (sseMsg, bool) {
	c.t.Helper()
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				return sseMsg{}, false
			}
			if m.comment && m.event == "" {
				continue
			}
			return m, true
		case <-time.After(5 * time.Second):
			c.t.Fatal("no SSE message")
		}
	}
}

func (c *sseClient) expect(event string) sseMsg {
	c.t.Helper()
	m, ok := c.next()
	if !ok || m.event != event {
		c.t.Fatalf("got %+v (open %v), want event %q", m, ok, event)
	}
	return m
}

// none asserts that nothing but heartbeats arrives for a short while.
func (c *sseClient) none() {
	c.t.Helper()
	select {
	case m, ok := <-c.msgs:
		if ok && (!m.comment || m.event != "") {
			c.t.Fatalf("unexpected message %+v", m)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

func (c *sseClient) closed() {
	c.t.Helper()
	for {
		select {
		case _, ok := <-c.msgs:
			if !ok {
				return
			}
		case <-time.After(5 * time.Second):
			c.t.Fatal("the stream stayed open")
		}
	}
}

func (r *evRig) waitTimers(n int) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.clock.pending() < n {
		if time.Now().After(deadline) {
			r.t.Fatalf("handler armed %d timers, want %d", r.clock.pending(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func decodeInfo(t *testing.T, m sseMsg) SessionsResult {
	t.Helper()
	var res SessionsResult
	if err := json.Unmarshal([]byte(m.data), &res); err != nil {
		t.Fatalf("%q: %v", m.data, err)
	}
	return res
}

func decodeEvent(t *testing.T, m sseMsg) workers.Event {
	t.Helper()
	var ev workers.Event
	if err := json.Unmarshal([]byte(m.data), &ev); err != nil {
		t.Fatalf("%q: %v", m.data, err)
	}
	return ev
}

func TestEventsStartWithASnapshotAndDeliverOnlyReadableEvents(t *testing.T) {
	r := newEvRig(t, nil)
	_, fa := r.feed(wAlice, ss("a1", "agent:claude", "stable", "t"))
	_, fb := r.feed(wBob, ss("b1", "shell", "stable", ""))

	c := r.open("alice", "")
	if c.resp.StatusCode != 200 || !strings.HasPrefix(c.resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %v", c.resp.StatusCode, c.resp.Header)
	}
	snap := decodeInfo(t, c.expect("snapshot"))
	if len(snap.Sessions) != 1 || snap.Sessions[0].Session != "a1" || snap.Sessions[0].Worker != "pi-1" || !snap.Sessions[0].Online {
		t.Fatalf("snapshot = %+v", snap)
	}
	root := r.open("root", "?all=true")
	if got := decodeInfo(t, root.expect("snapshot")); len(got.Sessions) != 2 {
		t.Fatalf("root snapshot = %+v", got)
	}
	bob := r.open("bob", "")
	bob.expect("snapshot")

	// An event of bob's Worker reaches bob and the admin, not alice.
	if err := fb.Apply(workers.Change{Seq: 1, Event: workers.EventStateChanged, HasTask: true, Session: ss("b1", "shell", "busy", "")}); err != nil {
		t.Fatal(err)
	}
	if ev := decodeEvent(t, bob.expect("state_changed")); ev.WorkerID != wBob || ev.State != "busy" || ev.Session != "b1" {
		t.Fatalf("%+v", ev)
	}
	root.expect("state_changed")
	c.none()

	// An event of alice's Worker reaches alice and the admin, not bob.
	if err := fa.Apply(workers.Change{Seq: 1, Event: workers.EventStateChanged, HasTask: true, Session: ss("a1", "agent:claude", "busy", "t")}); err != nil {
		t.Fatal(err)
	}
	ev := decodeEvent(t, c.expect("state_changed"))
	if ev.WorkerID != wAlice || ev.Worker != "pi-1" || ev.Session != "a1" || ev.State != "busy" || ev.Task == nil || *ev.Task != "t" {
		t.Fatalf("%+v", ev)
	}
	root.expect("state_changed")
	bob.none()

	// Worker-level events follow the same rule.
	fa2 := r.hub.Connect(wAlice, "pi-1")
	_ = fa2
	c.expect("worker_online")
	root.expect("worker_online")
	bob.none()
}

func TestEventsAdminDefaultIsMine(t *testing.T) {
	r := newEvRig(t, nil)
	_, fa := r.feed(wAlice, ss("a", "shell", "stable", ""))
	_, fr := r.feed(wRoot, ss("r", "shell", "stable", ""))
	root := r.open("root", "")
	if got := decodeInfo(t, root.expect("snapshot")); len(got.Sessions) != 1 || got.Sessions[0].Session != "r" {
		t.Fatalf("%+v", got)
	}
	_ = fa.Apply(workers.Change{Seq: 1, Event: workers.EventStateChanged, HasTask: true, Session: ss("a", "shell", "busy", "")})
	_ = fr.Apply(workers.Change{Seq: 1, Event: workers.EventStateChanged, HasTask: true, Session: ss("r", "shell", "busy", "")})
	if ev := decodeEvent(t, root.expect("state_changed")); ev.WorkerID != wRoot {
		t.Fatalf("the mine filter let %s through", ev.WorkerID)
	}
	root.none()

	// ?worker narrows an all=true stream to one Worker.
	one := r.open("root", "?all=true&worker=pi-1")
	if got := decodeInfo(t, one.expect("snapshot")); len(got.Sessions) != 1 || got.Sessions[0].Session != "a" {
		t.Fatalf("%+v", got)
	}
	_ = fr.Apply(workers.Change{Seq: 2, Event: workers.EventStateChanged, HasTask: true, Session: ss("r", "shell", "stable", "")})
	_ = fa.Apply(workers.Change{Seq: 2, Event: workers.EventStateChanged, HasTask: true, Session: ss("a", "shell", "stable", "")})
	if ev := decodeEvent(t, one.expect("state_changed")); ev.WorkerID != wAlice {
		t.Fatalf("%+v", ev)
	}
	one.none()
}

func TestEventsScopedKeyReceivesOnlyItsSessions(t *testing.T) {
	r := newEvRig(t, nil)
	_, fr := r.feed(wRoot, ss("r1", "shell", "stable", ""), ss("r2", "shell", "stable", ""))
	r.auth.grant("key", authz.NewAPIKeyPrincipal(evRoot, evScope{sessions: map[[2]string]bool{{wRoot, "r2"}: true}}, ""))
	c := r.open("key", "?all=true")
	if got := decodeInfo(t, c.expect("snapshot")); len(got.Sessions) != 1 || got.Sessions[0].Session != "r2" {
		t.Fatalf("%+v", got)
	}
	_ = fr.Apply(workers.Change{Seq: 1, Event: workers.EventStateChanged, HasTask: true, Session: ss("r1", "shell", "busy", "")})
	_ = fr.Apply(workers.Change{Seq: 2, Event: workers.EventStateChanged, HasTask: true, Session: ss("r2", "shell", "busy", "")})
	if ev := decodeEvent(t, c.expect("state_changed")); ev.Session != "r2" {
		t.Fatalf("%+v", ev)
	}
	// The Worker itself is not readable by this key, but one of its sessions
	// is: a Worker-level event is still sent.
	r.hub.Connect(wRoot, "nuc")
	c.expect("worker_online")
	c.none()
}

func TestEventsEndWhenAccessIsLost(t *testing.T) {
	r := newEvRig(t, nil)
	r.feed(wAlice, ss("a", "shell", "stable", ""))
	c := r.open("alice", "")
	c.expect("snapshot")
	r.waitTimers(2)
	callsBefore := r.auth.count()

	// Within the interval nothing is re-checked; a heartbeat may fire.
	r.world.set(func() { r.world.users[evAlice] = authz.UserState{Role: authz.RoleUser, Active: false} })
	r.clock.advance(15 * time.Second)
	r.waitTimers(2) // the heartbeat re-armed
	c.none()
	if r.auth.count() != callsBefore {
		t.Fatal("credentials were re-checked before the interval")
	}

	// At 30 s the deactivated user loses the stream.
	r.clock.advance(15 * time.Second)
	m := c.expect("access_lost")
	if !strings.Contains(m.data, `"code":"forbidden"`) {
		t.Fatalf("data = %s", m.data)
	}
	c.closed()
	waitFor(t, "the stream slot", func() bool { return r.events.Streams() == 0 })
}

func TestEventsEndWhenTheCredentialIsRevoked(t *testing.T) {
	r := newEvRig(t, nil)
	r.auth.grant("key", authz.NewAPIKeyPrincipal(evAlice, evScope{workers: map[string]bool{wAlice: true}}, ""))
	c := r.open("key", "")
	c.expect("snapshot")
	r.waitTimers(2)
	r.auth.revoke("key")
	r.clock.advance(30 * time.Second)
	m := c.expect("access_lost")
	if !strings.Contains(m.data, `"code":"unauthenticated"`) {
		t.Fatalf("data = %s", m.data)
	}
	c.closed()
}

func TestEventsEndWhenTheCredentialChangesUser(t *testing.T) {
	r := newEvRig(t, nil)
	c := r.open("alice", "")
	c.expect("snapshot")
	r.waitTimers(2)
	r.auth.grant("alice", authz.NewSessionPrincipal(evBob)) // same cookie, another user
	r.clock.advance(30 * time.Second)
	if m := c.expect("access_lost"); !strings.Contains(m.data, "credentials changed") {
		t.Fatalf("%s", m.data)
	}
	c.closed()
}

func TestEventsReauthorisationDropsStalePermissions(t *testing.T) {
	r := newEvRig(t, nil)
	_, fa := r.feed(wAlice, ss("a", "shell", "stable", ""))
	_, fr := r.feed(wRoot, ss("r", "shell", "stable", ""))
	c := r.open("alice", "")
	c.expect("snapshot")
	r.waitTimers(2)

	_ = fa.Apply(workers.Change{Seq: 1, Event: workers.EventStateChanged, HasTask: true, Session: ss("a", "shell", "busy", "")})
	c.expect("state_changed")

	// The Worker is reassigned to bob. The stream stays open (alice is still a
	// valid user) but, at the next re-check, alice stops receiving its events.
	r.world.set(func() { r.world.owners[wAlice] = evBob })
	r.clock.advance(30 * time.Second)
	r.waitTimers(2)
	_ = fa.Apply(workers.Change{Seq: 2, Event: workers.EventStateChanged, HasTask: true, Session: ss("a", "shell", "stable", "")})
	c.none()
	// ...and it is still alive: heartbeats and later events of her own work.
	r.world.set(func() { r.world.owners[wRoot] = evAlice })
	r.clock.advance(30 * time.Second)
	r.waitTimers(2)
	_ = fr.Apply(workers.Change{Seq: 1, Event: workers.EventStateChanged, HasTask: true, Session: ss("r", "shell", "busy", "")})
	if ev := decodeEvent(t, c.expect("state_changed")); ev.WorkerID != wRoot {
		t.Fatalf("%+v", ev)
	}
}

func TestEventsHeartbeat(t *testing.T) {
	r := newEvRig(t, nil)
	c := r.open("alice", "")
	c.expect("snapshot")
	r.waitTimers(2)
	r.clock.advance(15 * time.Second)
	select {
	case m := <-c.msgs:
		if !m.comment {
			t.Fatalf("got %+v, want a comment", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat")
	}
}

func TestEventsHubShutdownEndsStreams(t *testing.T) {
	r := newEvRig(t, nil)
	c := r.open("alice", "")
	c.expect("snapshot")
	r.events.Close()
	if m := c.expect("reset"); !strings.Contains(m.data, "shutdown") {
		t.Fatalf("%+v", m)
	}
	c.closed()
}

func TestEventsOpenErrors(t *testing.T) {
	r := newEvRig(t, func(c *EventsConfig) { c.MaxStreamsPerUser = 1; c.MaxStreams = 2 })
	if code, _ := r.get("", "/api/v1/events"); code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
	if code, _ := r.get("alice", "/api/v1/events?all=maybe"); code != 400 {
		t.Fatalf("bad query: %d", code)
	}
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/api/v1/events", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 405 {
		t.Fatalf("POST: %v %v", resp, err)
	}
	// A user that is already deactivated is refused before the stream starts.
	r.world.set(func() { r.world.users[evBob] = authz.UserState{Role: authz.RoleUser, Active: false} })
	if code, body := r.get("bob", "/api/v1/events"); code != 403 {
		t.Fatalf("deactivated: %d %s", code, body)
	}

	a := r.open("alice", "")
	a.expect("snapshot")
	code, body := r.get("alice", "/api/v1/events")
	if code != 429 || !strings.Contains(string(body), "rate_limited") {
		t.Fatalf("per-user cap: %d %s", code, body)
	}
	r.open("root", "").expect("snapshot")
	r.auth.grant("carol", authz.NewSessionPrincipal(evRoot))
	if code, _ := r.get("carol", "/api/v1/events"); code != 429 { // root's cap of 1
		t.Fatalf("second root stream: %d", code)
	}
	// Closing a stream frees its slot.
	a.cancel()
	waitFor(t, "the slot", func() bool { return r.events.Streams() == 1 })
	b := r.open("alice", "")
	b.expect("snapshot")

	if _, err := NewEventsHandler(EventsConfig{}); err == nil {
		t.Fatal("handler without configuration")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// gateWriter is a ResponseWriter whose writes block until the gate opens, so
// that a stream's queue can overflow.
type gateWriter struct {
	hdr  http.Header
	gate chan struct{}
	mu   sync.Mutex
	buf  strings.Builder
}

func (g *gateWriter) Header() http.Header { return g.hdr }
func (g *gateWriter) WriteHeader(int)     {}
func (g *gateWriter) Flush()              {}
func (g *gateWriter) Write(p []byte) (int, error) {
	<-g.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.Write(p)
}

func (g *gateWriter) text() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}

func TestEventsSlowConsumerIsReset(t *testing.T) {
	r := newEvRig(t, func(c *EventsConfig) { c.Buffer = 2 })
	_, fa := r.feed(wAlice, ss("a", "shell", "stable", ""))
	gw := &gateWriter{hdr: http.Header{}, gate: make(chan struct{})}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("X-Cred", "alice")
	done := make(chan struct{})
	go func() { defer close(done); r.events.ServeHTTP(gw, req) }()
	waitFor(t, "the subscription", func() bool { return r.events.Streams() == 1 })
	// The writer is stuck on its first write (the retry hint); publish more
	// than the queue holds.
	waitFor(t, "the handler to reach the writer", func() bool {
		for i := uint64(1); ; i++ {
			_ = fa.Apply(workers.Change{Seq: i, Event: workers.EventScreenChanged, Session: workers.SessionState{Name: "a", ScreenHash: strings.Repeat("h", int(i))}})
			if i > 6 {
				return true
			}
		}
	})
	close(gw.gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow stream stayed open")
	}
	if out := gw.text(); !strings.Contains(out, "event: reset") || !strings.Contains(out, "slow_consumer") {
		t.Fatalf("output: %s", out)
	}
}

// ---------------------------------------------------------------------------
// Integration: a Worker's ev stream reaches an SSE client.

func TestSSEClientReceivesStateChangedFromATestWorker(t *testing.T) {
	r := newEvRig(t, nil)
	ing, err := workers.NewIngest(workers.IngestOptions{Hub: r.hub})
	if err != nil {
		t.Fatal(err)
	}
	reg := workers.NewRegistry()
	reg.OnAdd(ing.Attach)
	hubSide, workerSide := net.Pipe()
	hs, err := transport.NewSession(hubSide, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := transport.NewSession(workerSide, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hs.Close(); _ = ws.Close() })

	c := r.open("alice", "")
	c.expect("snapshot")
	reg.Add(workers.Online{WorkerID: wAlice, Name: "pi-1", OwnerID: evAlice, Ops: []string{ops.OpSessionsList}}, hs)
	c.expect("worker_online")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev, err := ws.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ev.WriteFrame(map[string]any{"type": proto.TypeEventsSnap, "seq": 0, "sessions": []map[string]any{
		{"name": "minion-1", "kind": "agent:claude", "state": "stable", "task": "", "screen_hash": "h0"}}}); err != nil {
		t.Fatal(err)
	}
	c.expect("worker_resynced")
	// The aggregated list answers from the ingested state.
	if n := sessionNames(r.sessions("alice", "")); n != "pi-1:minion-1" {
		t.Fatalf("list = %s", n)
	}
	if err := ev.WriteFrame(map[string]any{"type": proto.TypeEvent, "seq": 1, "event": "state_changed", "session": "minion-1",
		"kind": "agent:claude", "state": "busy", "task": "go"}); err != nil {
		t.Fatal(err)
	}
	got := decodeEvent(t, c.expect("state_changed"))
	if got.WorkerID != wAlice || got.Session != "minion-1" || got.State != "busy" || got.Task == nil || *got.Task != "go" {
		t.Fatalf("%+v", got)
	}
	if l := r.sessions("alice", ""); len(l) != 1 || l[0].State != "busy" || !l[0].Online {
		t.Fatalf("%+v", l)
	}

	// The Worker reconnects: its new snapshot replaces the state and the
	// subscriber hears worker_resynced.
	_ = ws.Close()
	c.expect("worker_offline")
	if l := r.sessions("alice", ""); len(l) != 1 || l[0].Online {
		t.Fatalf("offline list: %+v", l)
	}
	h2, w2 := net.Pipe()
	hs2, _ := transport.NewSession(h2, transport.RoleHub, transport.SessionOptions{})
	ws2, _ := transport.NewSession(w2, transport.RoleWorker, transport.SessionOptions{})
	t.Cleanup(func() { _ = hs2.Close(); _ = ws2.Close() })
	reg.Add(workers.Online{WorkerID: wAlice, Name: "pi-1", OwnerID: evAlice}, hs2)
	c.expect("worker_online")
	ev2, err := ws2.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = ev2.WriteFrame(map[string]any{"type": proto.TypeEventsSnap, "seq": 0, "sessions": []map[string]any{
		{"name": "minion-9", "kind": "shell", "state": "stable", "task": ""}}})
	c.expect("worker_resynced")
	if n := sessionNames(r.sessions("alice", "")); n != "pi-1:minion-9" {
		t.Fatalf("after the resync: %s", n)
	}
}

// ---------------------------------------------------------------------------
// Visibility around snapshots and reassignment

// A scoped subscriber that hears worker_online before the Worker's first
// snapshot is denied (the Worker holds nothing yet); the worker_resynced that
// brings the sessions must not be suppressed by that earlier denial.
func TestEventsScopedSubscriberHearsTheResyncOfAWorkerThatConnectsLater(t *testing.T) {
	r := newEvRig(t, nil)
	r.auth.grant("key", authz.NewAPIKeyPrincipal(evRoot, evScope{sessions: map[[2]string]bool{{wRoot, "r2"}: true}}, ""))
	c := r.open("key", "?all=true")
	if got := decodeInfo(t, c.expect("snapshot")); len(got.Sessions) != 0 {
		t.Fatalf("%+v", got)
	}
	l := r.hub.Connect(wRoot, "nuc") // no sessions known yet: nothing readable
	c.none()
	f, err := l.NewFeed()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Snapshot(0, []workers.SessionState{ss("r1", "shell", "stable", ""), ss("r2", "shell", "stable", "")}); err != nil {
		t.Fatal(err)
	}
	if ev := decodeEvent(t, c.expect("worker_resynced")); ev.WorkerID != wRoot {
		t.Fatalf("%+v", ev)
	}
}

// When a replacement snapshot no longer holds the only session a scoped
// subscriber could read, it still hears worker_resynced once, so that it
// drops the session; later resyncs with nothing readable stay quiet.
func TestEventsResyncTellsAScopedSubscriberThatItsLastSessionIsGone(t *testing.T) {
	r := newEvRig(t, nil)
	l, _ := r.feed(wRoot, ss("r1", "shell", "stable", ""), ss("r2", "shell", "stable", ""))
	r.auth.grant("key", authz.NewAPIKeyPrincipal(evRoot, evScope{sessions: map[[2]string]bool{{wRoot, "r2"}: true}}, ""))
	c := r.open("key", "?all=true")
	if got := decodeInfo(t, c.expect("snapshot")); len(got.Sessions) != 1 || got.Sessions[0].Session != "r2" {
		t.Fatalf("%+v", got)
	}
	resync := func(sessions ...workers.SessionState) {
		t.Helper()
		f, err := l.NewFeed()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Snapshot(0, sessions); err != nil {
			t.Fatal(err)
		}
	}
	resync(ss("r1", "shell", "stable", "")) // r2 was deleted or unshared
	c.expect("worker_resynced")
	resync(ss("r1", "shell", "busy", ""))
	c.none()
}

// A stream narrowed to one Worker ends when that Worker is no longer the
// caller's to read; an unfiltered stream only stops hearing about it (see
// TestEventsReauthorisationDropsStalePermissions).
func TestEventsEndWhenTheSelectedWorkerIsReassigned(t *testing.T) {
	r := newEvRig(t, nil)
	r.feed(wAlice, ss("a", "shell", "stable", ""))
	c := r.open("alice", "?worker=pi-1")
	c.expect("snapshot")
	r.waitTimers(2)
	r.world.set(func() { r.world.owners[wAlice] = evBob })
	r.clock.advance(30 * time.Second)
	if m := c.expect("access_lost"); !strings.Contains(m.data, `"code":"forbidden"`) {
		t.Fatalf("data = %s", m.data)
	}
	c.closed()
	waitFor(t, "the stream slot", func() bool { return r.events.Streams() == 0 })
}

// ---------------------------------------------------------------------------
// Re-authorisation runs while a write is blocked

// deadlineWriter is a ResponseWriter whose writes block, once block is set,
// until the write deadline passes (a client that stopped reading).
type deadlineWriter struct {
	hdr      http.Header
	block    atomic.Bool
	blocked  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	deadline time.Time
	buf      strings.Builder
}

func newDeadlineWriter() *deadlineWriter {
	return &deadlineWriter{hdr: http.Header{}, blocked: make(chan struct{})}
}

func (d *deadlineWriter) Header() http.Header { return d.hdr }
func (d *deadlineWriter) WriteHeader(int)     {}
func (d *deadlineWriter) Flush()              {}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadline = t
	return nil
}

func (d *deadlineWriter) expired() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.deadline.IsZero() && !time.Now().Before(d.deadline)
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if d.block.Load() {
		d.once.Do(func() { close(d.blocked) })
		for !d.expired() {
			time.Sleep(5 * time.Millisecond)
		}
		d.block.Store(false) // the client "recovers": later writes go through
		return 0, os.ErrDeadlineExceeded
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.Write(p)
}

func (d *deadlineWriter) text() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.String()
}

// The re-check does not wait for a blocked write: a revoked credential ends
// the stream at the interval and cuts the write, long before the write
// timeout. (The last event cannot pass a connection whose write failed.)
func TestEventsReauthorisationInterruptsABlockedWrite(t *testing.T) {
	r := newEvRig(t, func(c *EventsConfig) { c.WriteTimeout = time.Minute })
	_, fa := r.feed(wAlice, ss("a", "shell", "stable", ""))
	dw := newDeadlineWriter()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	req.Header.Set("X-Cred", "alice")
	done := make(chan struct{})
	go func() { defer close(done); r.events.ServeHTTP(dw, req) }()
	waitFor(t, "the snapshot", func() bool { return strings.Contains(dw.text(), "event: snapshot") })
	r.waitTimers(2)

	dw.block.Store(true)
	_ = fa.Apply(workers.Change{Seq: 1, Event: workers.EventStateChanged, HasTask: true, Session: ss("a", "shell", "busy", "")})
	select {
	case <-dw.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the write did not block")
	}
	r.auth.revoke("alice")
	r.clock.advance(30 * time.Second)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the revoked stream stayed behind its blocked write (the write timeout is a minute)")
	}
}

// ---------------------------------------------------------------------------
// Announced request bodies

// rawGet sends a GET that announces a body of n bytes and sends none, and
// returns the response, which must come within the body deadline.
func (r *evRig) rawGetWithMissingBody(path string) *http.Response {
	r.t.Helper()
	conn, err := net.Dial("tcp", r.srv.Listener.Addr().String())
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, "GET "+path+" HTTP/1.1\r\nHost: hub\r\nX-Cred: alice\r\nContent-Length: 1000\r\n\r\n"); err != nil {
		r.t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		r.t.Fatalf("GET %s that announces a body and sends none was not answered in time: %v", path, err)
	}
	r.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestRoutesBoundAnAnnouncedBodyThatNeverArrives(t *testing.T) {
	for _, path := range []string{SessionsPath, EventsPath} {
		t.Run(path, func(t *testing.T) {
			r := newEvRig(t, func(c *EventsConfig) { c.BodyTimeout = 200 * time.Millisecond })
			began := time.Now()
			resp := r.rawGetWithMissingBody(path)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if time.Since(began) < 150*time.Millisecond {
				t.Fatal("answered before the body deadline")
			}
			if !resp.Close {
				t.Error("the connection stays open after an unread body")
			}
		})
	}
}

// A body that does arrive is dropped and the request is served as usual: the
// list answers and the feed streams.
func TestRoutesServeAGetWithASmallBody(t *testing.T) {
	r := newEvRig(t, nil)
	r.feed(wAlice, ss("a", "shell", "stable", ""))
	for _, path := range []string{SessionsPath, EventsPath} {
		req, _ := http.NewRequest(http.MethodGet, r.srv.URL+path, strings.NewReader("{}"))
		req.Header.Set("X-Cred", "alice")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
		line, _ := bufio.NewReader(resp.Body).ReadString('\n')
		if path == EventsPath && !strings.HasPrefix(line, "retry:") {
			t.Fatalf("%s did not stream: %q", path, line)
		}
		if path == SessionsPath && !strings.Contains(line, `"session":"a"`) {
			t.Fatalf("%s = %q", path, line)
		}
		resp.Body.Close()
	}
}

// A body that is a huge announcement is refused, not read for ever.
func TestRoutesRefuseAnOversizedBody(t *testing.T) {
	r := newEvRig(t, nil)
	for _, path := range []string{SessionsPath, EventsPath} {
		req, _ := http.NewRequest(http.MethodGet, r.srv.URL+path, io.LimitReader(zeroReader{}, MaxBodyBytes+10))
		req.Header.Set("X-Cred", "alice")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s = %d, want 413", path, resp.StatusCode)
		}
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// rdWriter records the read deadlines set through the ResponseController.
type rdWriter struct {
	*gateWriter
	mu    sync.Mutex
	reads []time.Time
}

func (w *rdWriter) SetReadDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reads = append(w.reads, t)
	return nil
}

func (w *rdWriter) lastRead() (time.Time, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.reads) == 0 {
		return time.Time{}, 0
	}
	return w.reads[len(w.reads)-1], len(w.reads)
}

// No read deadline stays on a request once its body is read, so none cuts a
// live event stream.
func TestRoutesLiftTheBodyDeadline(t *testing.T) {
	r := newEvRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, path := range []string{SessionsPath, EventsPath} {
		gw := &gateWriter{hdr: http.Header{}, gate: make(chan struct{})}
		close(gw.gate)
		w := &rdWriter{gateWriter: gw}
		req := httptest.NewRequest(http.MethodGet, path, strings.NewReader("{}")).WithContext(ctx)
		req.Header.Set("X-Cred", "alice")
		done := make(chan struct{})
		go func() {
			defer close(done)
			if path == SessionsPath {
				sh, _ := NewSessionsHandler(r.auth, r.lister)
				sh.ServeHTTP(w, req)
				return
			}
			r.events.ServeHTTP(w, req)
		}()
		if path == EventsPath {
			waitFor(t, "the stream", func() bool { return strings.Contains(gw.text(), "event: snapshot") })
			cancel()
		}
		<-done
		if last, n := w.lastRead(); n != 2 || !last.IsZero() {
			t.Fatalf("%s: read deadlines = %d, last %v; want the deadline set and then lifted", path, n, last)
		}
	}
}

// ---------------------------------------------------------------------------
// pipe_changed: the metadata-only wake-up of the pipe conversation of a session.

func TestEventsPipeChangedIsMetadataOnlyAndFollowsTheReadFilter(t *testing.T) {
	r := newEvRig(t, nil)
	r.feed(wAlice, ss("a", "agent:claude", "stable", "SECRET-TASK"))
	r.feed(wBob, ss("b", "shell", "stable", ""))
	alice, bob, root := r.open("alice", ""), r.open("bob", ""), r.open("root", "")
	for _, c := range []*sseClient{alice, bob, root} {
		c.expect("snapshot")
	}
	r.hub.PublishPipeChanged(wAlice, "a")
	m := alice.expect("pipe_changed")
	var got map[string]any
	if err := json.Unmarshal([]byte(m.data), &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "pipe_changed" || got["worker_id"] != wAlice || got["session"] != "a" {
		t.Fatalf("event = %v", got)
	}
	for k, v := range got {
		switch k {
		case "type", "worker_id", "session", "time":
		case "worker":
			if v != "" {
				t.Errorf("the wake-up names the Worker: %v", v)
			}
		default:
			t.Errorf("the wake-up carries %q", k)
		}
	}
	if strings.Contains(m.data, "SECRET-TASK") || strings.Contains(m.data, "pi-1") {
		t.Errorf("the wake-up carries session data: %s", m.data)
	}
	// Another person and an admin without ?all=true get no wake-up of a session that is not theirs, and nothing names it.
	bob.none()
	root.none()
	// An event without a session or without a Worker is not published at all.
	r.hub.PublishPipeChanged(wAlice, "")
	r.hub.PublishPipeChanged("", "a")
	alice.none()
	// ?all=true lifts the filter, as for every event.
	all := r.open("root", "?all=true")
	all.expect("snapshot")
	r.hub.PublishPipeChanged(wAlice, "a")
	all.expect("pipe_changed")
	alice.expect("pipe_changed")
}

// The decision is made at the delivery, from the current state: an event that was queued before the session was taken away
// from the share, or before the Worker changed owner, is dropped, and the stream goes on.
func TestEventsPipeChangedQueuedBeforeARevocationIsDropped(t *testing.T) {
	for _, revoke := range []struct{ name string }{{"unshare"}, {"reassign"}} {
		t.Run(revoke.name, func(t *testing.T) {
			r2 := newEvRig(t, func(c *EventsConfig) { c.Buffer = 8 })
			_, fa := r2.feed(wAlice, ss("a", "shell", "stable", ""), ss("keep", "shell", "stable", ""))
			gw := &gateWriter{hdr: http.Header{}, gate: make(chan struct{})}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
			req.Header.Set("X-Cred", "alice")
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); r2.events.ServeHTTP(gw, req.WithContext(ctx)) }()
			waitFor(t, "the subscription", func() bool { return r2.events.Streams() == 1 })
			// The writer is held on its first write: these events are queued for the stream.
			r2.hub.PublishPipeChanged(wAlice, "a")
			r2.hub.PublishPipeChanged(wAlice, "keep")
			switch revoke.name {
			case "unshare":
				r2.hub.ApplyShare(wAlice, []string{"a"})
			case "reassign":
				r2.world.set(func() { r2.world.owners[wAlice] = evBob })
			}
			close(gw.gate)
			// A later event proves the queue was handled up to here.
			_ = fa.Apply(workers.Change{Seq: 1, Event: workers.EventStateChanged, HasTask: true, Session: ss("keep", "shell", "busy", "")})
			waitFor(t, "the queue to be handled", func() bool {
				return revoke.name == "reassign" || strings.Contains(gw.text(), "state_changed")
			})
			time.Sleep(100 * time.Millisecond)
			cancel()
			<-done
			out := gw.text()
			const aEvent = `"worker":"","session":"a"`
			if revoke.name == "unshare" {
				if strings.Contains(out, aEvent) {
					t.Errorf("a wake-up queued before the unshare was delivered:\n%s", out)
				}
				if !strings.Contains(out, `"session":"keep"`) {
					t.Errorf("the wake-up of the session that stays shared was lost:\n%s", out)
				}
			} else if strings.Contains(out, "event: pipe_changed") {
				t.Errorf("a wake-up queued before the Worker changed owner was delivered:\n%s", out)
			}
		})
	}
}
