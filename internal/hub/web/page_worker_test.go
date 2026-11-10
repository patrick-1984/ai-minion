package web

// Tests of the Worker detail page (Story 5.7): /workers/<id>. Real rendered markup is parsed
// (golang.org/x/net/html), not matched as strings only; the unit tests use the fake session
// layer, and the same routes run on a composed Hub with real session cookies in
// internal/hub/server (worker_detail_native_test.go).

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

var (
	wAliceInfo = ops.WorkerInfo{ID: "w1", Name: "laptop", OwnerID: "u-alice", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true, ProtoVersion: "1.0"}
	wBobInfo   = ops.WorkerInfo{ID: "w2", Name: "buildbox", OwnerID: "u-bob", Owner: "bob", Kind: ops.WorkerKindInfrastructure}
)

// workerPageServer serves every registered page with a fake live source and a fake caller whose
// workers_list is the given list (the base permission: the owner or an administrator operates).
func workerPageServer(t *testing.T, mine, all Dashboard, infos ...ops.WorkerInfo) (*Server, *fakeSource) {
	t.Helper()
	connect(t, newFakeOps().reply(ops.OpWorkersList, ops.WorkersListResult{Workers: infos}))
	return listServer(t, mine, all)
}

var (
	snapMine = Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent, sWaiting, sIdle}}
	snapAll  = Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sWaiting, sIdle, sShell}}
)

func parse(t *testing.T, body string) *html.Node {
	t.Helper()
	return parseHTML(t, body)
}

// find returns every element for which pred holds.
func find(n *html.Node, pred func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.ElementNode && pred(x) {
			out = append(out, x)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func hasAttr(n *html.Node, k string) bool { _, ok := attrOf(n, k); return ok }
func attr(n *html.Node, k string) string  { v, _ := attrOf(n, k); return v }

func text(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			sb.WriteString(x.Data)
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(sb.String()), " ")
}

func TestWorkerDetailShowsTheWorkerAndOnlyItsSessionsFromTheAuthorisedSnapshot(t *testing.T) {
	s, src := workerPageServer(t, snapMine, snapAll, wAliceInfo, wBobInfo)
	rec := get(t, s, "/workers/w1", "X-Test-Session", "alice:user")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /workers/w1 = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	doc := parse(t, body)
	if h1 := find(doc, func(n *html.Node) bool { return n.Data == "h1" }); len(h1) != 1 || text(h1[0]) != "laptop" {
		t.Errorf("headings: %v", h1)
	}
	mustHave(t, body, `<title>Worker - Minion</title>`, `<span class="badge" data-online="true">online</span>`, `<span class="badge plain" data-kind="private">private</span>`,
		`<dt>Owner</dt><dd>alice</dd>`, `<dt>Protocol</dt><dd>1.0</dd>`, `<dt>Worker id</dt><dd><code>w1</code></dd>`,
		`<h2 id="worker-sessions-h">Sessions</h2>`, `3 sessions.`, `id="inspector"`, `<script src="/static/inspector.js" defer></script>`,
		`sse-connect="/live/workers/w1"`)
	// The sessions of THIS worker only, from the same snapshot; the table has no Worker column (the heading is the worker).
	rows := find(doc, func(n *html.Node) bool { return n.Data == "tr" && hasAttr(n, "data-session") })
	var names []string
	for _, r := range rows {
		names = append(names, attr(r, "data-session"))
		if attr(r, "data-worker") != "w1" || attr(r, "data-row-link") != "session" {
			t.Errorf("row %v", r.Attr)
		}
	}
	if strings.Join(names, ",") != "fix-tests,deploy,notes" {
		t.Errorf("sessions = %v", names)
	}
	var heads []string
	for _, th := range find(doc, func(n *html.Node) bool { return n.Data == "th" && attr(n, "scope") == "col" }) {
		heads = append(heads, text(th))
	}
	if strings.Join(heads, ",") != "Session,Task,Kind,State" {
		t.Errorf("session table headings = %v", heads)
	}
	mustNotHave(t, body, "buildbox", "/s/buildbox/ci", `>Actions<`)
	// The one action is the existing New session route, by the canonical id, for the owner of an online worker.
	mustHave(t, body, `<form method="post" action="/w/w1/sessions" hx-post="/w/w1/sessions"`, `aria-label="New session on laptop"`)
	// Active navigation and breadcrumb: Workers is the current section, the Worker is the last crumb.
	mustHave(t, body, `<li><a href="/workers" aria-current="page"><svg`, `<li><a href="/workers">Workers</a></li>`)
	if m := regexp.MustCompile(`<ol>(.*?)</ol>`).FindStringSubmatch(strings.ReplaceAll(body, "\n", "")); m == nil || !strings.Contains(m[1], `<span aria-current="page">laptop</span>`) {
		t.Errorf("breadcrumb: %v", m)
	}
	// The snapshot was asked as an ordinary user: Mine.
	for _, all := range src.asked() {
		if all {
			t.Error("an ordinary user's page asked the source with all=true")
		}
	}
	// Headers: a host page (it opens the inspector), never framed.
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") || rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("headers: %s %s", csp, rec.Header().Get("X-Frame-Options"))
	}
}

func TestWorkerDetailOffersNewSessionOnlyWhereItIsAllowedAndMeaningful(t *testing.T) {
	// A worker that someone else owns and the user may only see (shared): no button. An administrator operates it.
	bobOnline := wInfra
	bobOnline.Online = true
	shared := Dashboard{Workers: []WorkerRow{wPrivate, bobOnline}, Sessions: []SessionRow{sShell}}
	s, _ := workerPageServer(t, shared, shared, wAliceInfo, func() ops.WorkerInfo { w := wBobInfo; w.Online = true; return w }())
	_, own := page(t, s, "/workers/w1", "alice:user")
	mustHave(t, own, `action="/w/w1/sessions"`)
	_, other := page(t, s, "/workers/w2", "alice:user")
	mustNotHave(t, other, `/w/w2/sessions`, `data-control="new-session"`)
	mustHave(t, other, "buildbox")
	_, adm := page(t, s, "/workers/w2?scope=all", "root:admin")
	mustHave(t, adm, `action="/w/w2/sessions"`, `<dt>Owner</dt><dd>bob</dd>`)
	// An offline worker has no button, even for its owner, and says why there is no session.
	offline := Dashboard{Workers: []WorkerRow{{ID: "w1", Name: "laptop", Owner: "alice", Kind: ops.WorkerKindPrivate}}}
	off := wAliceInfo
	off.Online = false
	s2, _ := workerPageServer(t, offline, offline, off)
	_, body := page(t, s2, "/workers/w1", "alice:user")
	mustNotHave(t, body, `/w/w1/sessions`)
	mustHave(t, body, `<span class="badge muted" data-online="false">offline</span>`, "The worker is offline, so no session is listed.")
	// Nothing is invented for a worker that reports no protocol: a dash, no host, version or uptime.
	mustHave(t, body, `<dt>Protocol</dt><dd><span class="muted">-</span></dd>`)
	mustNotHave(t, body, "uptime", "Uptime", "Hostname", "Host name")
	// A worker with no session says so, for an owner who can start one.
	empty := Dashboard{Workers: []WorkerRow{wPrivate}}
	s3, _ := workerPageServer(t, empty, empty, wAliceInfo)
	_, nb := page(t, s3, "/workers/w1", "alice:user")
	mustHave(t, nb, "No sessions on this worker", "Choose New session to start one.")
}

// The page shows what the lists show for the same person and scope, and a worker outside that is the same plain 404 as an id that does
// not exist: nothing about it (not even that it exists) is visible, and a display name never addresses a worker.
func TestWorkerDetailIsNotFoundForAnythingOutsideTheScopeAndNeverLeaks(t *testing.T) {
	s, _ := workerPageServer(t, snapMine, snapAll, wAliceInfo, wBobInfo)
	missing := get(t, s, "/workers/no-such-id", "X-Test-Session", "alice:user")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d", missing.Code)
	}
	for what, c := range map[string]struct{ path, user string }{
		"another owner's worker (not in the user's snapshot)": {"/workers/w2", "alice:user"},
		"another owner's worker, ?scope=all by a user":        {"/workers/w2?scope=all", "alice:user"},
		"an administrator's Mine scope":                       {"/workers/w2", "root:admin"},
		"a display name instead of the id":                    {"/workers/laptop", "alice:user"},
		"an upper-case id":                                    {"/workers/W1", "alice:user"},
		"an id with a path":                                   {"/workers/w1%2Fx", "alice:user"},
	} {
		rec := get(t, s, c.path, "X-Test-Session", c.user)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", what, rec.Code)
			continue
		}
		b := rec.Body.String()
		if strings.Contains(b, "buildbox") || strings.Contains(b, "bob") || strings.Contains(b, "w2") && c.path != "/workers/w2" {
			t.Errorf("%s: the 404 names something: %s", what, b)
		}
		// The same message and page as an unknown id (the layout differs only by who is signed in).
		msg := func(x string) string { return regexp.MustCompile(`<h1>(.*?)</h1>`).FindString(x) }
		if msg(b) == "" || msg(b) != msg(missing.Body.String()) {
			t.Errorf("%s: the 404 differs from the one of an unknown id: %q and %q", what, msg(b), msg(missing.Body.String()))
		}
	}
	// An administrator who chooses All users sees it, with the owner.
	rec := get(t, s, "/workers/w2?scope=all", "X-Test-Session", "root:admin")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "buildbox") || !strings.Contains(rec.Body.String(), `<dt>Owner</dt><dd>bob</dd>`) {
		t.Errorf("admin, All users = %d", rec.Code)
	}
	// Signed out, and a must-change account: the usual answers, nothing of the worker.
	out := get(t, s, "/workers/w1")
	if out.Code != http.StatusSeeOther || strings.Contains(out.Body.String(), "laptop") {
		t.Errorf("signed out = %d", out.Code)
	}
	must := get(t, s, "/workers/w1", "X-Test-Session", "alice:user:must")
	if must.Code != http.StatusSeeOther || must.Header().Get("Location") != "/account/password" {
		t.Errorf("must change = %d %q", must.Code, must.Header().Get("Location"))
	}
	// A refused snapshot is not a page.
	mustErr := newFakeSource(snapMine, snapAll)
	SetLiveSource(mustErr)
	setSnapErr(mustErr, &OpError{Status: 403, Code: "forbidden", Message: "secret detail"})
	if r := get(t, s, "/workers/w1", "X-Test-Session", "alice:user"); r.Code == http.StatusOK || strings.Contains(r.Body.String(), "secret detail") {
		t.Errorf("refused snapshot = %d %s", r.Code, r.Body)
	}
}

func TestWorkerDetailKeepsTheScopeInItsLinksAndTheAddWorkerPageIsUntouched(t *testing.T) {
	s, _ := workerPageServer(t, snapMine, snapAll, wAliceInfo, wBobInfo)
	_, body := page(t, s, "/workers/w2?scope=all", "root:admin")
	doc := parse(t, body)
	links := map[string]string{}
	for _, a := range find(doc, func(n *html.Node) bool { return n.Data == "a" }) {
		links[text(a)] = attr(a, "href")
	}
	if links["All workers"] != "/workers?scope=all" || links["Mine"] != "/workers/w2" || links["All users"] != "/workers/w2?scope=all" {
		t.Errorf("scope links: %v", links)
	}
	mustHave(t, body, `<li><a href="/workers?scope=all">Workers</a></li>`, `sse-connect="/live/workers/w2?scope=all"`, `aria-current="true">All users</a>`)
	if links["In the session list"] != "/sessions?scope=all&worker=w2" {
		t.Errorf("session list link = %q", links["In the session list"])
	}
	// The sessions page links back to the worker in the same scope, and the Workers page links each worker by its canonical id.
	_, ses := page(t, s, "/sessions?scope=all", "root:admin")
	mustHave(t, ses, `<a href="/workers/w1?scope=all" data-control="worker" data-worker-link>laptop</a>`, `<a href="/workers/w2?scope=all" data-control="worker" data-worker-link>buildbox</a>`)
	_, wl := page(t, s, "/workers?scope=all", "root:admin")
	mustHave(t, wl, `<th scope="row"><a href="/workers/w2?scope=all" data-control="name" data-worker-link>buildbox</a></th>`)
	_, mine := page(t, s, "/workers", "alice:user")
	mustHave(t, mine, `<th scope="row"><a href="/workers/w1" data-control="name" data-worker-link>laptop</a></th>`)
	// /workers/add is still the add-worker page: a literal route wins over the {worker} wildcard.
	rec := get(t, s, "/workers/add", "X-Test-Session", "alice:user")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Add worker") || strings.Contains(rec.Body.String(), `id="worker-sessions-h"`) {
		t.Errorf("/workers/add = %d", rec.Code)
	}
}

// Two owners may have workers of the same name: the link is by id, so each opens its own worker.
func TestWorkerLinksUseTheCanonicalIDNotTheDisplayName(t *testing.T) {
	a := WorkerRow{ID: "id-A", Name: "pi", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true}
	b := WorkerRow{ID: "id/B c", Name: "pi", Owner: "bob", Kind: ops.WorkerKindPrivate, Online: true}
	all := Dashboard{Workers: []WorkerRow{a, b}, Sessions: []SessionRow{{WorkerID: "id-A", Worker: "pi", Owner: "alice", Name: "s1", State: "stable", Online: true}, {WorkerID: "id/B c", Worker: "pi", Owner: "bob", Name: "s2", State: "stable", Online: true}}}
	s, _ := workerPageServer(t, all, all, ops.WorkerInfo{ID: "id-A", Name: "pi", OwnerID: "u-root", Online: true}, ops.WorkerInfo{ID: "id/B c", Name: "pi", OwnerID: "u-root", Online: true})
	_, body := page(t, s, "/sessions?scope=all", "root:admin")
	mustHave(t, body, `href="/workers/id-A?scope=all"`, `href="/workers/id%2FB%20c?scope=all"`)
	// Each page shows its own worker's sessions only.
	_, pa := page(t, s, "/workers/id-A?scope=all", "root:admin")
	_, pb := page(t, s, "/workers/id%2FB%20c?scope=all", "root:admin")
	if !strings.Contains(pa, `data-session="s1"`) || strings.Contains(pa, `data-session="s2"`) || !strings.Contains(pb, `data-session="s2"`) || strings.Contains(pb, `data-session="s1"`) {
		t.Error("the two workers of the same name were mixed up")
	}
}

// ---- the real markup of the rows ----------------------------------------------------------------------------------

func TestSessionRowsAreRealTableRowsWithOneSessionLinkAndOneWorkerLinkAndNoActionsColumn(t *testing.T) {
	s, _ := workerPageServer(t, snapMine, snapAll, wAliceInfo, wBobInfo)
	for _, path := range []string{"/sessions", "/?x=1", "/sessions?scope=all"} {
		user := "alice:user"
		if strings.Contains(path, "scope=all") {
			user = "root:admin"
		}
		_, body := page(t, s, path, user)
		doc := parse(t, body)
		tables := find(doc, func(n *html.Node) bool {
			return n.Data == "table" && strings.Contains(attr(n, "class"), "session-table")
		})
		if len(tables) != 1 {
			t.Fatalf("%s: %d session tables", path, len(tables))
		}
		var heads []string
		for _, th := range find(tables[0], func(n *html.Node) bool { return n.Data == "th" && attr(n, "scope") == "col" }) {
			heads = append(heads, text(th))
		}
		want := "Session,Task,Worker,Kind,State"
		if strings.Contains(path, "scope=all") {
			want = "Session,Task,Worker,Owner,Kind,State"
		}
		if strings.Join(heads, ",") != want {
			t.Errorf("%s: headings %v, want %s (no Actions column)", path, heads, want)
		}
		rows := find(tables[0], func(n *html.Node) bool { return n.Data == "tr" && hasAttr(n, "data-session") })
		if len(rows) < 3 {
			t.Fatalf("%s: %d rows", path, len(rows))
		}
		for _, r := range rows {
			if attr(r, "data-row-link") != "session" || hasAttr(r, "tabindex") || hasAttr(r, "role") || hasAttr(r, "onclick") {
				t.Errorf("%s: row attributes %v (a row is a plain row: not a button, not focusable)", path, r.Attr)
			}
			if n := len(find(r, func(n *html.Node) bool { return n.Data == "td" })); n != len(heads)-1 {
				t.Errorf("%s: %d cells for %d headings", path, n, len(heads))
			}
			sess := find(r, func(n *html.Node) bool { return n.Data == "a" && hasAttr(n, "data-inspect") })
			wl := find(r, func(n *html.Node) bool { return n.Data == "a" && hasAttr(n, "data-worker-link") })
			if len(sess) != 1 || attr(sess[0], "data-control") != "name" || !strings.HasPrefix(attr(sess[0], "href"), "/s/") {
				t.Errorf("%s: a row must have exactly one session link: %d", path, len(sess))
			}
			if len(wl) != 1 || !regexp.MustCompile(`^/workers/[^/?]+(\?scope=all)?$`).MatchString(attr(wl[0], "href")) || attr(wl[0], "href") == "/workers" {
				t.Errorf("%s: a row must have exactly one Worker link to a Worker page: %v", path, wl)
			}
			if len(find(r, func(n *html.Node) bool { return n.Data == "button" || n.Data == "form" })) != 0 {
				t.Errorf("%s: a session row has a control of its own", path)
			}
			if txt := text(r); strings.Contains(txt, "Open") && !strings.Contains(attr(sess[0], "data-inspect-label"), "Open") && strings.Contains(strings.Fields(txt)[len(strings.Fields(txt))-1], "Open") {
				t.Errorf("%s: an Open button is back: %s", path, txt)
			}
		}
		// The Worker link and the session link go to different places: the Worker link is never the pane.
		mustNotHave(t, body, `data-worker-link data-inspect`, `data-inspect data-worker-link`)
	}
	// The empty and the no-match states keep their colspans with the five (six) columns.
	empty := Dashboard{Workers: []WorkerRow{wPrivate}}
	s2, _ := workerPageServer(t, empty, empty, wAliceInfo)
	_, home := page(t, s2, "/", "alice:user")
	mustHave(t, home, `<td colspan="5" class="empty">No sessions yet.`)
	_, home = page(t, s2, "/?scope=all", "root:admin")
	mustHave(t, home, `<td colspan="6" class="empty">No sessions yet.`)
}

func TestWorkerRowsLinkTheirNameAndKeepTheirLegitimateControls(t *testing.T) {
	s, _ := workerPageServer(t, snapMine, snapAll, wAliceInfo, wBobInfo)
	for _, path := range []string{"/workers", "/"} {
		_, body := page(t, s, path, "alice:user")
		doc := parse(t, body)
		rows := find(doc, func(n *html.Node) bool { return n.Data == "tr" && attr(n, "data-row-link") == "worker" })
		if len(rows) != 1 {
			t.Fatalf("%s: %d worker rows", path, len(rows))
		}
		r := rows[0]
		if hasAttr(r, "tabindex") || hasAttr(r, "role") {
			t.Errorf("%s: the worker row became a control", path)
		}
		links := find(r, func(n *html.Node) bool { return n.Data == "a" && hasAttr(n, "data-worker-link") })
		if len(links) != 1 || attr(links[0], "href") != "/workers/w1" || attr(links[0], "data-control") != "name" {
			t.Errorf("%s: the Worker link: %v", path, links)
		}
		// New session and the sessions count stay what they were.
		mustHave(t, body, `data-control="new-session" aria-label="New session on laptop"`, `data-control="sessions"`)
	}
	// Heading cells: the name is a link, the Actions column of the WORKER table (New session) is a legitimate control.
	_, wl := page(t, s, "/workers", "alice:user")
	mustHave(t, wl, `<th scope="col">Actions</th>`)
}

// ---- the stylesheet ----------------------------------------------------------------------------------------------

func TestSessionRowsAreCompactAndRowsLookClickableOnlyWithAScript(t *testing.T) {
	rules := loadSheet(t)
	tbl := []cssEl{el("table", "rtable session-table")}
	td := el("td", "")
	for _, w := range []int{320, 700, 1280, 1920} {
		if got := valueAt(rules, "padding", td, tbl, w); got != "9px 14px" {
			t.Errorf("session table cell padding at %dpx = %q, want 9px 14px", w, got)
		}
	}
	// The general rule (and the 60px minimum of every other table) is unchanged; the session table has none.
	if got := valueAt(rules, "padding", td, []cssEl{el("table", "rtable")}, 1280); got != "10px 14px" {
		t.Errorf("another table's cell padding = %q", got)
	}
	if got := valueAt(rules, "min-height", el("tr", ""), []cssEl{el("tbody", ""), el("table", "rtable")}, 1280); got != "60px" {
		t.Errorf("another table's row minimum = %q", got)
	}
	if got := valueAt(rules, "min-height", el("tr", ""), []cssEl{el("tbody", ""), el("table", "rtable session-table")}, 1280); got != "0" {
		t.Errorf("the session table's row minimum = %q, want 0", got)
	}
	row := el("tr", "", "data-row-link", "session")
	anc := append([]cssEl{el("tbody", ""), el("table", "rtable session-table")}, scripted...)
	if got := valueAt(rules, "cursor", row, anc, 1280); got != "pointer" {
		t.Errorf("a clickable row shows the cursor %q with a script", got)
	}
	if got := valueAt(rules, "cursor", row, anc[:2], 1280); got != "" {
		t.Errorf("a row shows a pointer without a script: %q", got)
	}
	// Long names and tasks wrap instead of clipping or widening the table.
	if got := valueAt(rules, "overflow-wrap", td, tbl, 1280); got != "anywhere" {
		t.Errorf("overflow-wrap = %q", got)
	}
}

// ---- the live stream -------------------------------------------------------------------------------------------------

func TestWorkerStreamSendsTheFragmentOfThisWorkerAndSaysWhenItIsGone(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{wAliceInfo}}))
	src := newFakeSource(snapMine, snapAll)
	reg := NewRegistry()
	if err := registerWorkerPage(reg, func() LiveSource { return src }, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newServer(t, reg))
	t.Cleanup(ts.Close)
	SetLiveTimers(LiveTimers{Debounce: 5 * time.Millisecond, Heartbeat: time.Hour})
	t.Cleanup(func() { SetLiveTimers(LiveTimers{}) })
	resp, rd, _ := openStream(t, ts, "/live/workers/w1", "alice:user")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	ev, _ := rd.nextWithin(2 * time.Second)
	if ev.name != "dashboard" || !strings.Contains(ev.data, `data-session="fix-tests"`) || strings.Contains(ev.data, `/s/buildbox/ci`) || !strings.Contains(ev.data, `action="/w/w1/sessions"`) {
		t.Fatalf("first event = %+v", ev)
	}
	// A session appears: the table follows without a reload; the worker leaves the list: the fragment says so.
	src.set(Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent, sIdle, sWaiting}})
	src.changes <- struct{}{}
	ev, _ = rd.nextWithin(2 * time.Second)
	if !strings.Contains(ev.data, `data-session="deploy"`) {
		t.Fatalf("second event = %+v", ev)
	}
	src.set(Dashboard{})
	src.changes <- struct{}{}
	ev, _ = rd.nextWithin(2 * time.Second)
	if !strings.Contains(ev.data, "This worker is no longer in your list") || strings.Contains(ev.data, "laptop") {
		t.Fatalf("third event = %+v", ev)
	}
}

// S5.7 review 1: the controls of the Worker detail page lose the keyboard focus on every live update unless the live list can
// find them again. static/live-list.js finds a control by the data-key of its group and its own data-control, never by position;
// both must therefore be on the real markup and the same on every update (they come from the canonical Worker id, not from the
// session list or the display name), and the two controls must not share an identity.
func TestWorkerDetailControlsHaveAStableFocusIdentityAcrossLiveUpdates(t *testing.T) {
	type ident struct{ key, control string }
	identities := func(t *testing.T, body string) map[string]ident {
		t.Helper()
		out := map[string]ident{}
		root := parse(t, body)
		// Every control outside the session table: the Worker's own actions.
		for _, c := range find(root, func(n *html.Node) bool {
			if n.Data != "a" && n.Data != "button" {
				return false
			}
			for p := n.Parent; p != nil; p = p.Parent {
				if p.Type == html.ElementNode && (p.Data == "table" || attr(p, "class") == "page-head" || attr(p, "class") == "page-actions") {
					return false
				}
				if p.Type == html.ElementNode && attr(p, "id") == "results" {
					return true
				}
			}
			return false
		}) {
			var key string
			for p := c.Parent; p != nil; p = p.Parent {
				if p.Type == html.ElementNode && hasAttr(p, "data-key") {
					key = attr(p, "data-key")
					break
				}
			}
			out[attr(c, "data-control")] = ident{key: key, control: attr(c, "data-control")}
			if attr(c, "data-control") == "" || key == "" {
				t.Errorf("a control of the Worker page has no focus identity (key %q, control %q): %s", key, attr(c, "data-control"), text(c))
			}
		}
		return out
	}
	one := Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}
	more := Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent, sWaiting, sIdle}}
	s, _ := workerPageServer(t, one, one, wAliceInfo)
	_, b1 := page(t, s, "/workers/w1", "alice:user")
	s2, _ := workerPageServer(t, more, more, wAliceInfo)
	_, b2 := page(t, s2, "/workers/w1", "alice:user")
	id1, id2 := identities(t, b1), identities(t, b2)
	for _, want := range []ident{{"worker-actions-w1", "new-session"}, {"worker-sessions-w1", "session-list"}} {
		if id1[want.control] != want || id2[want.control] != want {
			t.Errorf("control %q: identity %+v before and %+v after an update, want %+v both times", want.control, id1[want.control], id2[want.control], want)
		}
	}
	if len(id1) != 2 || len(id2) != 2 {
		t.Errorf("unexpected controls on the Worker page: %v / %v", id1, id2)
	}
	// The identity is the canonical Worker id: another Worker has other keys, and a display name never makes one.
	if strings.Contains(b1, `data-key="worker-actions-laptop"`) {
		t.Error("the focus identity uses the display name")
	}
	// A Worker without sessions has no session-list link: the other control keeps its identity, nothing replaces the missing one.
	empty := Dashboard{Workers: []WorkerRow{wPrivate}}
	s3, _ := workerPageServer(t, empty, empty, wAliceInfo)
	_, b3 := page(t, s3, "/workers/w1", "alice:user")
	id3 := identities(t, b3)
	if id3["new-session"] != (ident{"worker-actions-w1", "new-session"}) || len(id3) != 1 {
		t.Errorf("without sessions: %v", id3)
	}
	// Offline: no action at all; the key of the group is gone with it, so the focus goes to the heading, never to another action.
	offline := Dashboard{Workers: []WorkerRow{{ID: "w1", Name: "laptop", Owner: "alice", Kind: ops.WorkerKindPrivate}}, Sessions: []SessionRow{sAgent}}
	off := wAliceInfo
	off.Online = false
	s4, _ := workerPageServer(t, offline, offline, off)
	_, b4 := page(t, s4, "/workers/w1", "alice:user")
	if strings.Contains(b4, `data-control="new-session"`) || strings.Contains(b4, "worker-actions-w1") {
		t.Error("an offline Worker still has the New session group")
	}
}
