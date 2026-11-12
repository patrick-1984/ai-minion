//go:build e2e && unix

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/server"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// Story 4.15, composed: the default Hub (HTTPS, REST dispatcher, authz, audit,
// the route, events, share and pipe modules, the web pages) with a REAL Worker:
// the real connector over the real handshake, op server, executor, event
// publisher and policy over a real tmux server on a private socket. Nothing of
// the share path is faked: share_get and share_set go from the REST caller to
// the Worker and back, and snapshot, capture, the session list and the SSE feed
// are read from the Hub afterwards.

const secretText = "SECRET-PRIVATE-SCREEN-TEXT"

// shareHub is a started default Hub (the composition a Hub process runs, from
// a copy of server.DefaultRegistry plus a module that reads services back).
type shareHub struct {
	t      *testing.T
	st     *store.Store
	wsv    *workers.Service
	mgr    *session.Manager
	events *workers.EventHub
	addr   string
	client *http.Client
	admin  map[string]string
}

func asSession(token, csrf string) map[string]string {
	return map[string]string{"Cookie": session.CookieName + "=" + token, session.CSRFHeader: csrf}
}

func asKey(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startShareHub(t *testing.T) *shareHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "share-e2e-password-1")
	h := &shareHub{t: t}
	reg := server.DefaultRegistry.Clone()
	if err := reg.Register("grab-share", 101, func(_ context.Context, env *server.Env) error {
		var err error
		if h.st, err = server.Require[*store.Store](env); err != nil {
			return err
		}
		if h.wsv, err = server.Require[*workers.Service](env); err != nil {
			return err
		}
		if h.mgr, err = server.Require[*session.Manager](env); err != nil {
			return err
		}
		h.events, err = server.Require[*workers.EventHub](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	hubidtest.Provision(t, dataDir) // protocol 3.0: an established, certified Hub identity
	cfg := &server.Config{DataDir: dataDir, Listen: "127.0.0.1:0", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := server.New(cfg, reg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	select {
	case <-srv.Started():
	case err := <-done:
		cancel()
		t.Fatalf("the Hub did not start: %v", err)
	case <-time.After(20 * time.Second):
		cancel()
		t.Fatal("the Hub did not start")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v after shutdown", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("the Hub did not stop")
		}
	})
	h.addr = srv.Addr()
	h.client = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test Hub
	root, err := h.st.UserByIdentifier(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	is, err := h.mgr.Create(context.Background(), root, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h.admin = asSession(is.Token, is.CSRFToken)
	return h
}

func (h *shareHub) do(method, path string, body any, headers map[string]string) (int, string) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://"+h.addr+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// postForm posts a form like the browser's htmx does.
func (h *shareHub) postForm(path, form string, headers map[string]string) (int, string) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", "https://"+h.addr+path, strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

type shareE2E struct {
	t       *testing.T
	ph      *shareHub
	wid     string
	tmux    *tmuxtest.Server
	policy  *agent.Policy
	wstore  *wstore.Store
	owner   map[string]string // the owner's browser session
	foreign map[string]string
	ownerID string
	targets map[string]string
}

func (e *shareE2E) user(name string) (id string, sess map[string]string) {
	e.t.Helper()
	id = store.NewID()
	if _, err := e.ph.st.DB().Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, ?, 1, 1)`, id, name); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.ph.st.DB().Exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'user', 'local')`, id); err != nil {
		e.t.Fatal(err)
	}
	row, err := e.ph.st.UserByIdentifier(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	is, err := e.ph.mgr.Create(context.Background(), row, time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	return id, asSession(is.Token, is.CSRFToken)
}

func startShareE2E(t *testing.T, mut func(p *agent.Policy)) *shareE2E {
	t.Helper()
	e := &shareE2E{t: t, targets: map[string]string{}}
	e.ph = startShareHub(t)
	e.ownerID, e.owner = e.user("olivia")
	_, e.foreign = e.user("fred")

	// The Worker: state, key, enrollment, tmux, executor, op server, events.
	dir := t.TempDir()
	st, err := wstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.wstore = st
	if err := st.SaveShare(wstore.ShareSelection{Mode: wstore.ShareAll}); err != nil {
		t.Fatal(err)
	}
	enr := agent.NewEnrollments(st, dir)
	key, err := enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := enr.Commit(context.Background(), agent.Enrollment{Pin: identity.Pin{Pub: e.ph.wsv.Identity().Public()}}); err != nil {
		t.Fatal(err)
	}
	e.wid = store.NewID()
	if err := e.ph.st.InsertWorker(context.Background(), nil, store.WorkerRow{ID: e.wid, Pub: key.Public(), Name: "pi", OwnerID: e.ownerID,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	e.tmux = tmuxtest.New(t)
	for _, n := range []string{"alpha", "beta"} {
		e.targets[n] = e.tmux.NewSession(n, "cat")
	}
	host, err := tmux.New(e.tmux.Socket())
	if err != nil {
		t.Fatal(err)
	}
	e.policy = agent.NewPolicy(st)
	if mut != nil {
		mut(e.policy)
	}
	ex, err := agent.NewExecutor(agent.Config{Host: host, Classifier: agentdetect.New(), State: st, Policy: e.policy, LockWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := agent.NewOpServer(agent.OpServerConfig{Executor: ex, Policy: e.policy})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := agent.NewHostObserver(agent.HostObserverConfig{Host: host, Classifier: agentdetect.New(), Policy: e.policy})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := agent.NewEventPublisher(agent.EventConfig{Observer: obs, Interval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := agent.NewConnector(agent.ConnectorConfig{
		HubURL: "https://" + e.ph.addr, Name: "pi", Ops: ex.Ops, Key: enr.LoadKey, Enrollment: enr, Policy: e.policy,
		Dialer:   agent.NewHubDialer(),
		Backoff:  &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		IdlePoll: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn.OnLink("op-server", srv.Serve)
	conn.OnLink("events", pub.Serve)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = srv.Stop(context.Background())
	})
	waitFor(t, "the Worker online", func() bool { return e.ph.wsv.Registry().IsOnline(e.wid) })
	waitFor(t, "both sessions in the Hub", func() bool {
		ws, ok := e.ph.events.Sessions(e.wid)
		return ok && len(ws.Sessions) == 2
	})
	return e
}

func (e *shareE2E) rest(sess map[string]string, op string, body map[string]any) (int, string) {
	e.t.Helper()
	code, out := e.ph.do("POST", "/api/v1/ops/"+op, body, sess)
	return code, out
}

func (e *shareE2E) get(sess map[string]string, path string) (int, string) {
	e.t.Helper()
	code, out := e.ph.do("GET", path, nil, sess)
	return code, out
}

func (e *shareE2E) shareGet(sess map[string]string) (int, ops.ShareResult, string) {
	e.t.Helper()
	code, body := e.rest(sess, ops.OpShareGet, map[string]any{"worker_id": e.wid})
	var res ops.ShareResult
	if code == 200 {
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			e.t.Fatalf("share_get answer %s: %v", body, err)
		}
	}
	return code, res, body
}

func (e *shareE2E) shareSet(sess map[string]string, mode string, names ...string) (int, ops.ShareResult, string) {
	e.t.Helper()
	b := map[string]any{"worker_id": e.wid, "mode": mode}
	if len(names) > 0 {
		b["names"] = names
	}
	code, body := e.rest(sess, ops.OpShareSet, b)
	var res ops.ShareResult
	if code == 200 {
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			e.t.Fatalf("share_set answer %s: %v", body, err)
		}
	}
	return code, res, body
}

func (e *shareE2E) read(sess map[string]string, op, session string) (int, string) {
	e.t.Helper()
	return e.rest(sess, op, map[string]any{"target": map[string]any{"worker_id": e.wid, "session_name": session}})
}

// sse is an open SSE feed of the Hub.
type sseFeed struct {
	mu     sync.Mutex
	events [][2]string
	cancel context.CancelFunc
}

func (e *shareE2E) openSSE(sess map[string]string) *sseFeed {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+e.ph.addr+api.EventsPath, nil)
	for k, v := range sess {
		req.Header.Set(k, v)
	}
	resp, err := e.ph.client.Do(req)
	if err != nil {
		cancel()
		e.t.Fatal(err)
	}
	f := &sseFeed{cancel: cancel}
	e.t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		var cur [2]string
		for sc.Scan() {
			switch line := sc.Text(); {
			case line == "":
				if cur[0] != "" {
					f.mu.Lock()
					f.events = append(f.events, cur)
					f.mu.Unlock()
				}
				cur = [2]string{}
			case strings.HasPrefix(line, "event: "):
				cur[0] = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur[1] = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return f
}

func (f *sseFeed) all() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.events...)
}

// about returns the events (type and data) that name the session.
func (f *sseFeed) about(session string) [][2]string {
	var out [][2]string
	for _, ev := range f.all() {
		if strings.Contains(ev[1], `"session":"`+session+`"`) {
			out = append(out, ev)
		}
	}
	return out
}

func TestShareE2EAuthorizationMatrixAndNamesOnly(t *testing.T) {
	e := startShareE2E(t, nil)
	admin := e.ph.admin

	// The owner and an admin may ask; a foreign user may not even learn the Worker exists.
	for who, sess := range map[string]map[string]string{"owner": e.owner, "admin": admin} {
		code, res, body := e.shareGet(sess)
		if code != 200 || res.Rule.Mode != "all" || len(res.Unshared) != 0 {
			t.Fatalf("%s share_get = %d %s", who, code, body)
		}
	}
	if code, _, body := e.shareGet(e.foreign); code != 404 {
		t.Fatalf("a foreign user's share_get = %d %s, want 404", code, body)
	}
	if code, _, body := e.shareSet(e.foreign, "none"); code != 404 {
		t.Fatalf("a foreign user's share_set = %d %s, want 404", code, body)
	}
	if code, res, _ := e.shareGet(e.owner); code != 200 || res.Rule.Mode != "all" {
		t.Fatalf("a foreign share_set changed the rule: %+v", res)
	}

	// API keys: operate on the Worker is needed; read alone is forbidden; a key of another Worker is not found.
	mint := func(actions ...string) map[string]string {
		code, body := e.rest(e.owner, ops.OpAPIKeyCreate, map[string]any{
			"name": "k-" + strings.Join(actions, "-"), "actions": actions,
			"resources": []map[string]any{{"kind": "worker", "worker_id": e.wid}},
		})
		var res ops.APIKeyCreateResult
		if code != 200 || json.Unmarshal([]byte(body), &res) != nil {
			t.Fatalf("apikey_create = %d %s", code, body)
		}
		return asKey(res.Key)
	}
	operateKey, readKey := mint("operate"), mint("read")
	if code, _, body := e.shareGet(operateKey); code != 200 {
		t.Fatalf("operate key share_get = %d %s", code, body)
	}
	if code, _, body := e.shareGet(readKey); code != 403 {
		t.Fatalf("read key share_get = %d %s, want 403", code, body)
	}
	if code, _, body := e.shareSet(readKey, "none"); code != 403 {
		t.Fatalf("read key share_set = %d %s, want 403", code, body)
	}
	if code, res, body := e.shareSet(operateKey, "select", "alpha"); code != 200 || res.Rule.Mode != "select" {
		t.Fatalf("operate key share_set = %d %s", code, body)
	}

	// Names only: the answer holds the rule and the names, whatever the sessions show.
	e.tmux.SendKeys(e.targets["beta"], secretText, "Enter")
	e.tmux.WaitFor(e.targets["beta"], secretText, 5*time.Second)
	code, res, body := e.shareGet(e.owner)
	if code != 200 || res.Rule.Mode != "select" || len(res.Unshared) != 1 || res.Unshared[0] != "beta" {
		t.Fatalf("share_get after select = %d %s", code, body)
	}
	for _, leak := range []string{secretText, "screen", "pane", "agent", "task"} {
		if strings.Contains(body, leak) {
			t.Fatalf("share_get exposed %q: %s", leak, body)
		}
	}

	// The audit log recorded the changes, and the refused attempt of the foreign user.
	code, aud := e.get(admin, "/api/v1/audit?limit=50")
	if code != 200 || !strings.Contains(aud, `"share_set"`) || !strings.Contains(aud, `"denied"`) {
		t.Fatalf("audit = %d %s", code, aud)
	}
}

func TestShareE2EUnsharedSessionNeverLeaksAfterShareSet(t *testing.T) {
	e := startShareE2E(t, nil)
	feed := e.openSSE(e.owner)
	waitFor(t, "the feed's snapshot", func() bool { return len(feed.all()) > 0 })
	e.tmux.SendKeys(e.targets["beta"], "before-unshare", "Enter")
	e.tmux.WaitFor(e.targets["beta"], "before-unshare", 5*time.Second)

	// While shared, beta is readable.
	if code, body := e.read(e.owner, ops.OpSessionSnapshot, "beta"); code != 200 || !strings.Contains(body, "before-unshare") {
		t.Fatalf("snapshot of a shared session = %d %s", code, body)
	}
	if code, body := e.read(e.owner, ops.OpSessionCapture, "beta"); code != 200 || !strings.Contains(body, "before-unshare") {
		t.Fatalf("capture of a shared session = %d %s", code, body)
	}

	// The Hub keeps pipe history of beta (a reply of the agent), which is readable while it is shared.
	turnID, _ := pipe.NewTurnID(time.Now())
	if _, err := e.ph.st.Pipe().PutTurn(context.Background(), pipe.Turn{TurnID: turnID,
		Peer: pipe.Peer{WorkerID: e.wid, SessionName: "beta"}, Text: "PIPE-HISTORY-OF-BETA", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pipeRead := func() (int, string) {
		return e.rest(e.owner, ops.OpPipeRead, map[string]any{"target": map[string]any{"worker_id": e.wid, "session_name": "beta"}})
	}
	if code, body := pipeRead(); code != 200 || !strings.Contains(body, "PIPE-HISTORY-OF-BETA") {
		t.Fatalf("pipe_read of a shared session = %d %s", code, body)
	}

	code, res, body := e.shareSet(e.owner, "select", "alpha")
	if code != 200 || len(res.Unshared) != 1 || res.Unshared[0] != "beta" {
		t.Fatalf("share_set = %d %s", code, body)
	}

	// From the moment share_set has returned, every read of beta is refused...
	for _, op := range []string{ops.OpSessionSnapshot, ops.OpSessionCapture} {
		code, body := e.read(e.owner, op, "beta")
		if code != 403 || !strings.Contains(body, "not_shared") || strings.Contains(body, "before-unshare") {
			t.Fatalf("%s of an unshared session = %d %s", op, code, body)
		}
		if code, body := e.read(e.ph.admin, op, "beta"); code != 403 {
			t.Fatalf("an admin's %s of an unshared session = %d %s", op, code, body)
		}
	}
	// ...the history the Hub kept is not served either, to the owner or an admin...
	for who, sess := range map[string]map[string]string{"owner": e.owner, "admin": e.ph.admin} {
		code, body := e.rest(sess, ops.OpPipeRead, map[string]any{"target": map[string]any{"worker_id": e.wid, "session_name": "beta"}})
		if code != 403 || !strings.Contains(body, "not_shared") || strings.Contains(body, "PIPE-HISTORY-OF-BETA") {
			t.Fatalf("%s pipe_read of an unshared session = %d %s", who, code, body)
		}
	}
	// ...the session list has no beta, not even by name, and the Hub's own state dropped it...
	code, list := e.get(e.owner, api.SessionsPath)
	if code != 200 || strings.Contains(list, "beta") || !strings.Contains(list, "alpha") {
		t.Fatalf("session list = %d %s", code, list)
	}
	if ws, _ := e.ph.events.Sessions(e.wid); len(ws.Sessions) != 1 || ws.Sessions[0].Name != "alpha" {
		t.Fatalf("the Hub still knows %+v", ws.Sessions)
	}
	// ...and a session that was open before sees it removed, and nothing more of it.
	waitFor(t, "session_removed for beta on the open feed", func() bool {
		for _, ev := range feed.about("beta") {
			if ev[0] == "session_removed" {
				return true
			}
		}
		return false
	})
	seenAtRemoval := len(feed.about("beta"))

	// New content in beta and in alpha: only alpha's change is reported.
	e.tmux.SendKeys(e.targets["beta"], secretText, "Enter")
	e.tmux.SendKeys(e.targets["alpha"], "alpha-visible", "Enter")
	waitFor(t, "an event about alpha's new screen", func() bool {
		for _, ev := range feed.about("alpha") {
			if ev[0] == "screen_changed" {
				return true
			}
		}
		return false
	})
	time.Sleep(200 * time.Millisecond) // a few polls of the Worker; the bound is the alpha event above
	if n := len(feed.about("beta")); n != seenAtRemoval {
		t.Fatalf("events about the unshared session went on: %v", feed.about("beta"))
	}
	for _, ev := range feed.all() {
		if strings.Contains(ev[1], secretText) {
			t.Fatalf("new content of an unshared session reached the feed: %v", ev)
		}
	}
	// A new feed does not know beta either.
	fresh := e.openSSE(e.owner)
	waitFor(t, "the new feed's snapshot", func() bool { return len(fresh.all()) > 0 })
	if len(fresh.about("beta")) != 0 || strings.Contains(fresh.all()[0][1], "beta") {
		t.Fatalf("a new feed names the unshared session: %v", fresh.all())
	}

	// Sharing it again restores reads, and share_get no longer lists it.
	if code, res, body := e.shareSet(e.owner, "all"); code != 200 || len(res.Unshared) != 0 {
		t.Fatalf("share_set all = %d %s", code, body)
	}
	if code, body := pipeRead(); code != 200 || !strings.Contains(body, "PIPE-HISTORY-OF-BETA") {
		t.Fatalf("pipe_read after sharing again = %d %s", code, body)
	}
	if code, body := e.read(e.owner, ops.OpSessionSnapshot, "beta"); code != 200 {
		t.Fatalf("snapshot after sharing again = %d %s", code, body)
	}
	waitFor(t, "beta back in the Hub", func() bool {
		ws, _ := e.ph.events.Sessions(e.wid)
		return len(ws.Sessions) == 2
	})
}

func TestShareE2EWorkerPoliciesStillWin(t *testing.T) {
	t.Run("share lock", func(t *testing.T) {
		e := startShareE2E(t, func(p *agent.Policy) { p.SetShareLock(true) })
		code, _, body := e.shareSet(e.owner, "none")
		if code != 403 || !strings.Contains(body, "share_lock") {
			t.Fatalf("share_set on a locked Worker = %d %s", code, body)
		}
		if code, _, body := e.shareSet(e.ph.admin, "none"); code != 403 {
			t.Fatalf("an admin's share_set on a locked Worker = %d %s", code, body)
		}
		code, res, body := e.shareGet(e.owner)
		if code != 200 || !res.Locked || res.Rule.Mode != "all" {
			t.Fatalf("share_get on a locked Worker = %d %s", code, body)
		}
		if sel, _ := e.wstore.LoadShare(); sel.Mode != wstore.ShareAll {
			t.Fatalf("the locked Worker's selection changed: %+v", sel)
		}
		if code, body := e.read(e.owner, ops.OpSessionSnapshot, "beta"); code != 200 {
			t.Fatalf("the lock hid a shared session: %d %s", code, body)
		}
	})
	t.Run("preview only", func(t *testing.T) {
		e := startShareE2E(t, func(p *agent.Policy) { p.SetPreviewOnly(true) })
		code, _, body := e.shareSet(e.owner, "none")
		if code != 403 || !strings.Contains(body, "preview_only") {
			t.Fatalf("share_set on a preview-only Worker = %d %s", code, body)
		}
		if code, _, body := e.shareGet(e.owner); code != 403 {
			t.Fatalf("share_get on a preview-only Worker = %d %s (it needs operate, which a preview-only Worker takes away)", code, body)
		}
		if sel, _ := e.wstore.LoadShare(); sel.Mode != wstore.ShareAll {
			t.Fatalf("a preview-only Worker's selection changed: %+v", sel)
		}
	})
}

func TestShareE2EWebPanelIsGuardedAndWorksThroughTheRealRoutes(t *testing.T) {
	e := startShareE2E(t, nil)
	e.tmux.SendKeys(e.targets["beta"], secretText, "Enter")
	e.tmux.WaitFor(e.targets["beta"], secretText, 5*time.Second)
	panel := "/w/" + e.wid + "/share"

	// The Worker page of the owner offers the panel, at the canonical id.
	code, page := e.get(e.owner, "/workers/"+e.wid)
	if code != 200 || !strings.Contains(page, `hx-get="`+panel+`"`) {
		t.Fatalf("worker page = %d (no share panel link)", code)
	}
	// The panel lists names (checked when shared) and nothing of the sessions' content.
	code, html := e.get(e.owner, panel)
	if code != 200 || !strings.Contains(html, `value="alpha"`) || !strings.Contains(html, `value="beta"`) || strings.Contains(html, secretText) {
		t.Fatalf("panel = %d %s", code, html)
	}
	// A foreign user gets the same plain 404 as for an unknown Worker, on both routes.
	if code, _ := e.get(e.foreign, panel); code != 404 {
		t.Fatalf("a foreign user's panel = %d, want 404", code)
	}
	// (The form routes answer an error as a result fragment, like the session routes do: what
	// matters is that nothing was changed and nothing of the Worker is in the answer.)
	if _, out := e.ph.postForm(panel, "mode=none", e.foreign); strings.Contains(out, "Saved") || strings.Contains(out, "alpha") {
		t.Fatalf("a foreign user's POST was answered: %s", out)
	}
	if sel, _ := e.wstore.LoadShare(); sel.Mode != wstore.ShareAll {
		t.Fatalf("a foreign user's POST changed the selection: %+v", sel)
	}
	// Anonymous: sent to the sign-in page, never the panel.
	if _, anon := e.get(nil, panel); strings.Contains(anon, `value="alpha"`) || strings.Contains(anon, "share-form") {
		t.Fatalf("an anonymous request got the panel: %s", anon)
	}
	// The POST needs the CSRF token of the session.
	noCSRF := map[string]string{"Cookie": e.owner["Cookie"]}
	if code, _ := e.ph.postForm(panel, "mode=none", noCSRF); code == 200 {
		t.Fatalf("a POST without the CSRF token was answered 200")
	}
	if sel, _ := e.wstore.LoadShare(); sel.Mode != wstore.ShareAll {
		t.Fatalf("a refused POST changed the selection: %+v", sel)
	}

	// The owner chooses alpha only, through the form: the Worker applied it.
	code, out := e.ph.postForm(panel, "mode=select&name=alpha", e.owner)
	if code != 200 || !strings.Contains(out, "Saved") {
		t.Fatalf("POST = %d %s", code, out)
	}
	if sel, _ := e.wstore.LoadShare(); sel.Mode != wstore.ShareSelect || len(sel.Sessions) != 1 || sel.Sessions[0] != "alpha" {
		t.Fatalf("the Worker's selection = %+v", sel)
	}
	if code, body := e.read(e.owner, ops.OpSessionCapture, "beta"); code != 403 || strings.Contains(body, secretText) {
		t.Fatalf("capture after the form = %d %s", code, body)
	}
	// An empty select is refused by the API and says why; nothing changes.
	code, out = e.ph.postForm(panel, "mode=select", e.owner)
	if code != 200 || strings.Contains(out, "Saved") || !strings.Contains(out, "cannot be applied") {
		t.Fatalf("an empty select = %d %s", code, out)
	}
	if sel, _ := e.wstore.LoadShare(); sel.Mode != wstore.ShareSelect || len(sel.Sessions) != 1 {
		t.Fatalf("the empty select changed the selection: %+v", sel)
	}
}
