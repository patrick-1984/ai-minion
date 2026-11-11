package server

// Story 5.4 on a composed Hub: the real web, API, routing, events and act-as
// modules, real HTTP and real SSE, no fake source. Every wait is bounded.

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/session"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/web"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

const dashRootPassword = "dashboard-integration-pw-1"

// dashHub is a composed Hub with the dashboard's modules and an admin "root".
type dashHub struct {
	t   *testing.T
	h   *running
	hc  *hubClient
	st  *store.Store
	mgr *session.Manager
	wsv *workers.Service
}

func startDashHub(t *testing.T) *dashHub { return startDashHubAt(t, t.TempDir()) }

// startDashHubAt is startDashHub on a given data directory: starting it again on the same
// directory (in this or in another process) is a restart of the same Hub.
func startDashHubAt(t *testing.T, dataDir string) *dashHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", dashRootPassword)
	r := NewRegistry()
	for _, err := range []error{
		registerCore(r),
		registerStore(r),
		r.Register(settingsModuleName, settingsModuleOrder, initSettingsModule,
			DeclaresService[*SettingsService](), DeclaresService[config.PanelSource]()),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	registerAuditModule(r)
	registerAuthz(r)
	registerAuth(r)
	registerWorkersModule(r)
	registerAPI(r, ops.Default)
	registerActAs(r)
	registerRouteModule(r)
	registerEventsModule(r)
	registerWeb(r, web.DefaultRegistry)
	d := &dashHub{t: t}
	if err := r.Register("grab", 90, func(_ context.Context, env *Env) error {
		var err error
		if d.st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if d.mgr, err = Require[*session.Manager](env); err != nil {
			return err
		}
		d.wsv, err = Require[*workers.Service](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	d.h = startHub(t, &Config{DataDir: dataDir}, r)
	d.hc = newHubClient(t, d.h, "https")
	return d
}

// user adds a plain user and returns its id.
func (d *dashHub) user(name string) string {
	d.t.Helper()
	id := store.NewID()
	if _, err := d.st.DB().Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, ?, 1, 1) ; INSERT INTO user_roles (user_id, role, source) SELECT id, 'user', 'local' FROM users WHERE id NOT IN (SELECT user_id FROM user_roles)`, id, name); err != nil {
		d.t.Fatal(err)
	}
	return id
}

// login starts a browser session of the user and returns its headers.
func (d *dashHub) login(ident string) (hdr map[string]string, csrf string) {
	d.t.Helper()
	row, err := d.st.UserByIdentifier(context.Background(), ident)
	if err != nil {
		d.t.Fatal(err)
	}
	is, err := d.mgr.Create(context.Background(), row, time.Hour)
	if err != nil {
		d.t.Fatal(err)
	}
	return asSession(is.Token, is.CSRFToken), is.CSRFToken
}

func (d *dashHub) userID(ident string) string {
	d.t.Helper()
	row, err := d.st.UserByIdentifier(context.Background(), ident)
	if err != nil {
		d.t.Fatal(err)
	}
	return row.ID
}

// enrol records a Worker (offline) and returns its id.
func (d *dashHub) enrol(name, ownerID, kind string) string {
	d.t.Helper()
	key, _ := identity.GenerateKey()
	id := store.NewID()
	if err := d.st.InsertWorker(context.Background(), nil, store.WorkerRow{
		ID: id, Pub: key.Public(), Name: name, OwnerID: ownerID, Kind: kind, CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		d.t.Fatal(err)
	}
	return id
}

// workerLink is an online Worker: its event stream is open.
type workerLink struct {
	t   *testing.T
	ev  *transport.Stream
	seq int
	id  string
}

// connect brings an enrolled Worker online with the given sessions
// (name -> task).
func (d *dashHub) connect(wid, name, ownerID, kind string, sessions map[string]string) *workerLink {
	d.t.Helper()
	hubSide, workerSide := net.Pipe()
	hs, err := transport.NewSession(hubSide, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		d.t.Fatal(err)
	}
	ws, err := transport.NewSession(workerSide, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(func() { _ = ws.Close() })
	d.wsv.Registry().Add(workers.Online{WorkerID: wid, Name: name, OwnerID: ownerID, Kind: kind, Ops: []string{ops.OpSessionsList}}, hs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ev, err := ws.OpenEvents(ctx)
	if err != nil {
		d.t.Fatal(err)
	}
	var snap []map[string]any
	for n, task := range sessions {
		snap = append(snap, map[string]any{"name": n, "kind": "agent:claude", "state": "stable", "task": task})
	}
	if err := ev.WriteFrame(map[string]any{"type": proto.TypeEventsSnap, "seq": 0, "sessions": snap}); err != nil {
		d.t.Fatal(err)
	}
	return &workerLink{t: d.t, ev: ev, id: wid}
}

// state sends a state_changed of one session.
func (l *workerLink) state(session, state, task string) {
	l.t.Helper()
	l.seq++
	if err := l.ev.WriteFrame(map[string]any{"type": proto.TypeEvent, "seq": l.seq, "event": "state_changed",
		"session": session, "kind": "agent:claude", "state": state, "task": task}); err != nil {
		l.t.Fatal(err)
	}
}

// page fetches a page with the headers of a signed-in user.
func (d *dashHub) page(path string, hdr map[string]string) string {
	d.t.Helper()
	code, body, _ := d.hc.do("GET", path, nil, hdr)
	if code != 200 {
		d.t.Fatalf("GET %s = %d\n%s", path, code, body)
	}
	return body
}

// dashStream is an open /live/dashboard stream; it queues the fragments.
type dashStream struct {
	t     *testing.T
	frags chan string
}

func (d *dashHub) stream(path string, hdr map[string]string) *dashStream {
	d.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	d.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", d.hc.base+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Transport: d.hc.c.Transport}).Do(req)
	if err != nil {
		d.t.Fatal(err)
	}
	d.t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		d.t.Fatalf("GET %s = %d %v", path, resp.StatusCode, resp.Header)
	}
	s := &dashStream{t: d.t, frags: make(chan string, 256)}
	go func() {
		defer close(s.frags)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var name string
		var data []string
		for sc.Scan() {
			switch line := sc.Text(); {
			case line == "":
				if name == "dashboard" {
					select {
					case s.frags <- strings.Join(data, "\n"):
					default:
					}
				}
				name, data = "", nil
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return s
}

// until reads fragments until pred accepts one; it fails after 10 seconds.
func (s *dashStream) until(what string, pred func(string) bool) string {
	s.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f, ok := <-s.frags:
			if !ok {
				s.t.Fatalf("the stream ended before: %s", what)
			}
			if pred(f) {
				return f
			}
		case <-deadline:
			s.t.Fatalf("no fragment within 10s: %s", what)
		}
	}
}

// never reads fragments for the bounded time d and fails on one that bad
// accepts.
func (s *dashStream) never(what string, d time.Duration, bad func(string) bool) {
	s.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case f, ok := <-s.frags:
			if !ok {
				return
			}
			if bad(f) {
				s.t.Fatalf("%s:\n%s", what, f)
			}
		case <-deadline:
			return
		}
	}
}

func has(subs ...string) func(string) bool {
	return func(f string) bool {
		for _, s := range subs {
			if !strings.Contains(f, s) {
				return false
			}
		}
		return true
	}
}

func hasNot(subs ...string) func(string) bool {
	return func(f string) bool {
		for _, s := range subs {
			if strings.Contains(f, s) {
				return false
			}
		}
		return true
	}
}

func all(preds ...func(string) bool) func(string) bool {
	return func(f string) bool {
		for _, p := range preds {
			if !p(f) {
				return false
			}
		}
		return true
	}
}

func anyOf(subs ...string) func(string) bool {
	return func(f string) bool {
		for _, s := range subs {
			if strings.Contains(f, s) {
				return true
			}
		}
		return false
	}
}

// workerRow returns the table row of one Worker (not of its sessions) in a
// dashboard fragment, or "" when there is none.
func workerRow(frag, workerID string) string {
	// The row of a Worker, never the row of a session of it (which carries
	// data-session): the row has the id and its list key and nothing else.
	i := strings.Index(frag, `<tr data-worker="`+workerID+`" data-key="w-`+workerID+`" data-row-link="worker">`)
	if i < 0 {
		i = strings.Index(frag, `<tr data-worker="`+workerID+`" data-key="w-`+workerID+`">`)
	}
	if i < 0 {
		i = strings.Index(frag, `<tr data-worker="`+workerID+`">`)
	}
	if i < 0 {
		return ""
	}
	j := strings.Index(frag[i:], "</tr>")
	if j < 0 {
		return ""
	}
	return frag[i : i+j]
}

// rowHas accepts a fragment in which the row of the Worker contains every
// substring: it tells one Worker's owner cell from another's.
func rowHas(workerID string, subs ...string) func(string) bool {
	return func(f string) bool {
		row := workerRow(f, workerID)
		return row != "" && has(subs...)(row)
	}
}

func mustContain(t *testing.T, what, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(body, s) {
			t.Errorf("%s lacks %q:\n%s", what, s, body)
		}
	}
}

func mustNotContain(t *testing.T, what, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(body, s) {
			t.Errorf("%s contains %q:\n%s", what, s, body)
		}
	}
}

// TestHubDashboardListsWorkersAndSessionsLive runs the home page and its SSE
// stream as a signed-in user over a composed Hub: no fake source is involved.
func TestHubDashboardListsWorkersAndSessionsLive(t *testing.T) {
	d := startDashHub(t)
	hc := d.hc

	// Anonymous: the page and its stream are gated.
	if code, _, resp := hc.do("GET", "/", nil, nil); code != 200 || resp.Request.URL.Path != "/login" {
		t.Fatalf("anonymous GET / = %d at %s", code, resp.Request.URL)
	}
	if code, _, resp := hc.do("GET", "/live/dashboard", nil, nil); resp.Request.URL.Path != "/login" {
		t.Fatalf("anonymous GET /live/dashboard = %d at %s", code, resp.Request.URL)
	}
	if code, _, csrf := hc.login("root", dashRootPassword); code != 200 || csrf == "" {
		t.Fatalf("login = %d", code)
	}
	root, _ := d.login("root")
	rootID := d.userID("root")
	wid := d.enrol("lab", rootID, store.WorkerKindPrivate)

	// The enrolled Worker is listed (offline) before it connects.
	body := d.page("/", root)
	mustContain(t, "page", body, `data-worker="`+wid+`"`, `data-online="false"`, `data-kind="private"`, `sse-connect="/live/dashboard"`,
		`aria-current="true">Mine</a>`)
	mustNotContain(t, "page", body, `<th scope="col">Owner</th>`)
	// An admin sees the switch, and the owner column in "all users".
	mustContain(t, "all users page", d.page("/?scope=all", root), `<th scope="col">Owner</th>`, `<td data-label="Owner">root</td>`)

	st := d.stream("/live/dashboard", root)
	st.until("offline Worker", has(`data-worker="`+wid+`"`, `data-online="false"`))
	link := d.connect(wid, "lab", rootID, "private", map[string]string{"minion-1": "build <it>"})
	st.until("session of the connected Worker", has(`data-session="minion-1"`, `data-state="stable"`, "build &lt;it&gt;", `data-online="true"`))
	link.state("minion-1", "busy", "build <it>")
	st.until("state change", has(`data-state="busy"`))
	mustContain(t, "page after the change", d.page("/", root), `data-state="busy"`, `data-session="minion-1"`)
}

// Workers renamed, removed or reassigned while offline produce no event; the
// periodic reconciliation brings each user's stream up to date, and users never
// see one another's Workers.
func TestHubDashboardCatalogChangesReachEachUsersStream(t *testing.T) {
	defer web.SetLiveTimers(web.LiveTimers{Debounce: 20 * time.Millisecond, Reconcile: 100 * time.Millisecond})()
	d := startDashHub(t)
	alice, bob := d.user("alice"), d.user("bob")
	laptop := d.enrol("laptop", alice, store.WorkerKindPrivate)
	buildbox := d.enrol("buildbox", alice, store.WorkerKindInfrastructure)
	bobbox := d.enrol("bobbox", bob, store.WorkerKindPrivate)
	aliceH, _ := d.login("alice")
	bobH, _ := d.login("bob")
	rootH, _ := d.login("root")

	// Render, per user.
	ap, bp := d.page("/", aliceH), d.page("/", bobH)
	mustContain(t, "alice's page", ap, "laptop", "buildbox", `data-kind="infrastructure"`)
	mustNotContain(t, "alice's page", ap, "bobbox")
	mustContain(t, "bob's page", bp, "bobbox")
	mustNotContain(t, "bob's page", bp, "laptop", "buildbox")
	// A plain user asking for "all" gets the same as "mine", without the switch.
	au := d.page("/?scope=all", aliceH)
	mustNotContain(t, "alice's all-users page", au, "bobbox", `class="scope"`, `<th scope="col">Owner</th>`)

	as := d.stream("/live/dashboard", aliceH)
	bs := d.stream("/live/dashboard", bobH)
	rs := d.stream("/live/dashboard?scope=all", rootH)
	as.until("alice initial", all(has("laptop", "buildbox"), hasNot("bobbox")))
	bs.until("bob initial", all(has("bobbox"), hasNot("laptop", "buildbox")))
	// The owner cell is read in the row of the exact Worker: other rows carry
	// "alice" and "bob" too.
	rs.until("admin all users, with each owner in the row of its own Worker", all(
		has("laptop", "buildbox", "bobbox", `<th scope="col">Owner</th>`),
		rowHas(buildbox, `<td data-label="Owner">alice</td>`), rowHas(bobbox, `<td data-label="Owner">bob</td>`)))

	// Rename while offline.
	if _, err := d.st.RenameWorker(context.Background(), laptop, "workstation"); err != nil {
		t.Fatal(err)
	}
	as.until("rename", all(has("workstation"), hasNot(">laptop<")))
	mustContain(t, "page after the rename", d.page("/", aliceH), "workstation")

	// Reassignment of the infrastructure Worker from alice to bob.
	if _, err := d.st.ReassignWorker(context.Background(), store.ReassignParams{WorkerID: buildbox, NewOwnerID: bob, AtMS: store.NowMS()}); err != nil {
		t.Fatal(err)
	}
	as.until("reassigned away", all(has("workstation"), hasNot("buildbox")))
	bs.until("reassigned to bob", has("bobbox", "buildbox", `data-kind="infrastructure"`))
	rs.until("owner cell of buildbox's row follows", all(rowHas(buildbox, `<td data-label="Owner">bob</td>`), rowHas(bobbox, `<td data-label="Owner">bob</td>`),
		func(f string) bool {
			return !strings.Contains(workerRow(f, buildbox), `<td data-label="Owner">alice</td>`)
		}))
	mustNotContain(t, "alice's page after the reassignment", d.page("/", aliceH), "buildbox")
	mustContain(t, "bob's page after the reassignment", d.page("/", bobH), "buildbox")

	// Removal of the renamed Worker.
	if err := d.st.TombstoneWorker(context.Background(), laptop, d.userID("root"), store.NowMS()); err != nil {
		t.Fatal(err)
	}
	as.until("removal", all(hasNot("workstation"), has("No workers connected")))
	rs.until("removal in all users", hasNot("workstation"))
	// Bob never saw alice's Worker, not even under its old name.
	bs.never("bob's stream showed alice's Worker", 300*time.Millisecond, anyOf("laptop", "workstation"))
	mustNotContain(t, "bob's page", d.page("/", bobH), "laptop", "workstation")
}

// An admin who acts as a user gets that user's dashboard, for the page and for
// every live update, whatever the scope asks for; ending the act-as gives the
// admin's own space back. The backend decides, the dashboard only forwards.
func TestHubDashboardActAsFiltersRenderAndLiveUpdates(t *testing.T) {
	defer web.SetLiveTimers(web.LiveTimers{Debounce: 20 * time.Millisecond, Reconcile: 100 * time.Millisecond})()
	d := startDashHub(t)
	rootUID, alice, bob := d.userID("root"), d.user("alice"), d.user("bob")
	rw := d.enrol("rootbox", rootUID, store.WorkerKindPrivate)
	aw := d.enrol("laptop", alice, store.WorkerKindPrivate)
	bw := d.enrol("bobbox", bob, store.WorkerKindPrivate)
	d.connect(rw, "rootbox", rootUID, "private", map[string]string{"r1": "root-secret"})
	aliceLink := d.connect(aw, "laptop", alice, "private", map[string]string{"a1": "alice-task"})
	bobLink := d.connect(bw, "bobbox", bob, "private", map[string]string{"b1": "bob-secret"})
	root, _ := d.login("root")

	// Before: the admin sees everything under "all users", and only their own
	// under "mine".
	all0 := d.page("/?scope=all", root)
	mustContain(t, "admin all users", all0, "rootbox", "laptop", "bobbox", "root-secret", "alice-task", "bob-secret", `<th scope="col">Owner</th>`)
	mine0 := d.page("/", root)
	mustContain(t, "admin mine", mine0, "rootbox", "root-secret", `class="scope"`)
	mustNotContain(t, "admin mine", mine0, "laptop", "bobbox", "alice-task", "bob-secret")

	// An all-users stream is open when the act-as starts.
	live := d.stream("/live/dashboard?scope=all", root)
	live.until("admin all users stream", has("rootbox", "laptop", "bobbox", "bob-secret"))

	code, body, _ := d.hc.do("POST", "/api/v1/ops/act_as_start", map[string]any{
		"request_id": ops.NewUUIDv7(), "user_id": alice, "duration_minutes": 30}, root)
	if code != 200 {
		t.Fatalf("act_as_start = %d %s", code, body)
	}

	// Render while acting: alice's space, whatever the scope, and no switch.
	for _, path := range []string{"/", "/?scope=all"} {
		p := d.page(path, root)
		mustContain(t, "page "+path+" acting as alice", p, "laptop", "alice-task", "Acting as alice (admin root)")
		mustNotContain(t, "page "+path+" acting as alice", p, "rootbox", "bobbox", "root-secret", "bob-secret",
			`class="scope"`, `<th scope="col">Owner</th>`)
	}

	// Live: the stream that was open as admin catches up as alice, without a
	// reload, and a stream opened while acting starts as alice.
	live.until("the open stream drops the other users", all(has("laptop", "alice-task"), hasNot("rootbox", "bobbox", "bob-secret", "root-secret")))
	fresh := d.stream("/live/dashboard?scope=all", root)
	f := fresh.until("a new stream while acting", has("laptop"))
	mustNotContain(t, "stream opened while acting", f, "rootbox", "bobbox", "bob-secret", "root-secret", `<th scope="col">Owner</th>`)
	// Another user's changes never reach the acting admin's stream; alice's do.
	bobLink.state("b1", "busy", "bob-secret-2")
	live.never("a change of bob's session reached the stream acting as alice", 500*time.Millisecond, anyOf("bob-secret", "bobbox"))
	fresh.never("a change of bob's session reached the new stream", 100*time.Millisecond, anyOf("bob-secret", "bobbox"))
	aliceLink.state("a1", "busy", "alice-task-2")
	live.until("alice's change", has("alice-task-2"))
	fresh.until("alice's change on the new stream", has("alice-task-2"))

	// Exit: the admin's own space is back on the page and on a new stream.
	if code, body, _ := d.hc.do("POST", "/api/v1/ops/act_as_stop", map[string]any{}, root); code != 200 {
		t.Fatalf("act_as_stop = %d %s", code, body)
	}
	after := d.page("/", root)
	mustContain(t, "page after the exit", after, "rootbox", `class="scope"`)
	mustNotContain(t, "page after the exit", after, "laptop", "bobbox", "Acting as")
	mine := d.stream("/live/dashboard", root)
	mine.until("own space after the exit", all(has("rootbox"), hasNot("laptop", "bobbox")))
}

// The owner assertion reads the row of one Worker: a fragment in which another
// Worker has the expected owner does not satisfy it.
func TestWorkerRowSeparatesOwnerCells(t *testing.T) {
	stale := `<tr data-worker="b1">
  <th scope="row">buildbox</th>
  <td>alice</td>
</tr><tr data-worker="b1" data-session="s"><td>bob</td></tr><tr data-worker="b2">
  <th scope="row">bobbox</th>
  <td>bob</td>
</tr>`
	if rowHas("b1", "<td>bob</td>")(stale) {
		t.Error("buildbox's row accepted bob's owner cell from another row")
	}
	if !rowHas("b1", "<td>alice</td>")(stale) || !rowHas("b2", "<td>bob</td>")(stale) {
		t.Error("the owner cell of the exact row was not found")
	}
	if rowHas("b3", "<td>bob</td>")(stale) {
		t.Error("a missing row matched")
	}
}
