package web

// Tests of the workbench shell and pages (docs/design/workbench): role-derived
// navigation, the account and administration pages, the accessible Menu and
// theme markup, the filterable Workers and sessions lists, the session
// workspace and its pluggable right pane, and the scripts of the shell.

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// listServer serves the registered pages (the Workers and sessions lists
// included) with a fake live source.
func listServer(t *testing.T, mine, all Dashboard) (*Server, *fakeSource) {
	t.Helper()
	src := newFakeSource(mine, all)
	SetLiveSource(src)
	t.Cleanup(func() { SetLiveSource(nil) })
	return newServer(t, DefaultRegistry), src
}

var (
	wAlice2  = WorkerRow{ID: "w3", Name: "laptop", Owner: "carol", Kind: ops.WorkerKindPrivate, Online: true, Version: "1.0"}
	sWaiting = SessionRow{WorkerID: "w1", Worker: "laptop", Owner: "alice", Name: "deploy", Kind: "agent:claude", State: ops.PaneStateDialog, Task: "ship it", Online: true}
	sIdle    = SessionRow{WorkerID: "w1", Worker: "laptop", Owner: "alice", Name: "notes", Kind: "shell", State: ops.PaneStateStable, Online: true}
)

// ---- the shell ---------------------------------------------------------------

func TestShellHasTheAccessibleMenuThemeAndLandmarks(t *testing.T) {
	connect(t, newFakeOps())
	s := adminServer(t)
	rec := get(t, s, "/account", "X-Test-Session", userHeader)
	if rec.Code != 200 {
		t.Fatalf("GET /account = %d", rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body,
		// The Menu: a button with its state and the region it controls.
		`<button type="button" class="btn btn-secondary menu-btn" id="menu-button" aria-expanded="false" aria-controls="site-rail">`,
		`<aside class="rail" id="site-rail" aria-label="Minion" data-modal-inert>`, `<button type="button" class="rail-close" id="menu-close">`,
		`<div class="scrim" id="scrim"></div>`,
		// The theme is applied before the first paint, and offers System, Light, Dark.
		`<script src="/static/theme.js"></script>`, `<script src="/static/shell.js" defer></script>`,
		`<option value="system">System</option>`, `<option value="light">Light</option>`, `<option value="dark">Dark</option>`,
		// Skip link, landmarks, one h1, a labelled breadcrumb.
		`<a class="skip-link" href="#content" data-modal-inert>Skip to content</a>`, `<main id="content" tabindex="-1">`,
		`<nav aria-label="Main" id="site-nav">`, `<nav class="crumbs" aria-label="Breadcrumb">`,
		`<link rel="stylesheet" href="/static/minion.css">`, `<meta name="color-scheme" content="light dark">`)
	if n := strings.Count(body, "<h1"); n != 1 {
		t.Errorf("%d h1 elements, want exactly one", n)
	}
	// The sign-out form and the identity footer are part of every signed-in page.
	mustHave(t, body, `<form data-json-action="/auth/logout" data-next="/login">`, `<a class="identity" href="/account" aria-current="page">`)
}

func TestNoPageUsesInlineStylesOrAThirdPartyOrigin(t *testing.T) {
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{sessionWorker()}}).
		reply(ops.OpSessionSnapshot, ops.SessionSnapshotResult{Pane: dialogPane(), Screen: hostileScreen()}).
		reply(ops.OpSessionsList, ops.SessionsListResult{})
	connect(t, f)
	s, _ := listServer(t, Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}, Dashboard{})
	external := regexp.MustCompile(`(?i)(src|href|action|srcset)\s*=\s*["']?(https?:)?//`)
	for _, p := range []string{"/", "/workers", "/sessions", "/account", "/admin", "/admin/users", "/admin/settings", "/admin/oidc", "/admin/audit",
		"/invitations", "/api-keys", "/workers/add", "/account/password", "/s/pi-1/minion-1", "/nope"} {
		rec := get(t, s, p, "X-Test-Session", "alice:admin")
		body := rec.Body.String()
		if strings.Contains(body, " style=") || strings.Contains(body, "<style") {
			t.Errorf("%s: an inline style (the Content-Security-Policy forbids them)", p)
		}
		if external.MatchString(body) {
			t.Errorf("%s: a reference to another origin: %s", p, external.FindString(body))
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self'") || !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("%s: Content-Security-Policy = %q", p, csp)
		}
	}
	login := get(t, s, "/login").Body.String()
	if strings.Contains(login, " style=") || external.MatchString(login) {
		t.Errorf("login page: inline style or another origin")
	}
}

// ---- navigation follows the real role -----------------------------------------

var navHref = regexp.MustCompile(`<li><a href="([^"]+)"(?: aria-current="page")?><(?:svg|span)`)

func navPaths(body string) []string {
	var out []string
	for _, m := range navHref.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestNavigationGroupsFollowTheRoleAndKeepEveryDestination(t *testing.T) {
	connect(t, newFakeOps())
	s := adminServer(t)
	user := get(t, s, "/account", "X-Test-Session", userHeader).Body.String()
	admin := get(t, s, "/account", "X-Test-Session", adminHeader).Body.String()

	wantUser := []string{"/", "/workers", "/sessions", "/workers/add", "/invitations", "/api-keys"}
	if got := navPaths(user); strings.Join(got, ",") != strings.Join(wantUser, ",") {
		t.Errorf("user navigation = %v, want %v", got, wantUser)
	}
	wantAdmin := append(append([]string{}, wantUser...), "/admin/users", "/admin/settings", "/admin/oidc", "/admin/audit")
	if got := navPaths(admin); strings.Join(got, ",") != strings.Join(wantAdmin, ",") {
		t.Errorf("admin navigation = %v, want %v", got, wantAdmin)
	}
	// Groups are labelled; the administration heading exists only for an administrator.
	mustHave(t, user, `id="nav-workspace"`, `id="nav-access"`, ">Workspace<", ">Access<")
	mustNotHave(t, user, `id="nav-admin"`, "Administration", "/admin")
	mustHave(t, admin, `id="nav-admin"`, `<a href="/admin">Administration</a>`)
	for _, label := range []string{"Control center", "Workers", "Sessions", "Add worker", "Invitations", "API keys"} {
		mustHave(t, user, "<span>"+label+"</span>")
	}
	// Exactly one entry is the current page, and it follows the path.
	for path, label := range map[string]string{"/workers": "Workers", "/invitations": "Invitations", "/admin/audit": "Audit"} {
		body := get(t, s, path, "X-Test-Session", adminHeader).Body.String()
		if n := strings.Count(body, `aria-current="page"><svg`); n != 1 {
			t.Errorf("%s: %d current navigation entries, want 1", path, n)
		}
		if !regexp.MustCompile(`aria-current="page"><svg[^>]*><use href="#[a-z-]+"/></svg><span>` + label + `</span>`).MatchString(body) {
			t.Errorf("%s: the current entry is not %q", path, label)
		}
	}
	// A page below an entry keeps that entry current: a user detail belongs to Users.
	mustHave(t, get(t, s, "/admin/users/u-1", "X-Test-Session", adminHeader).Body.String(), `<a href="/admin/users" aria-current="page">`)
}

func TestHiddenNavigationIsNotTheBoundary(t *testing.T) {
	connect(t, newFakeOps())
	s := adminServer(t)
	for _, p := range []string{"/admin", "/admin/users", "/admin/settings", "/admin/oidc", "/admin/audit"} {
		rec := get(t, s, p, "X-Test-Session", userHeader)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
			t.Errorf("GET %s as a user = %d, want 403 or 404", p, rec.Code)
		}
		// A useful page without the protected content or the administration group.
		body := rec.Body.String()
		mustHave(t, body, "<h1>")
		mustNotHave(t, body, "Hub settings", `id="nav-admin"`, "Create a local user", "Audit")
	}
	rec := get(t, s, "/admin", "X-Test-Session", userHeader)
	mustHave(t, rec.Body.String(), "You are not allowed to open this page.", "Your account does not have access to this page.")
	if rec.Code != http.StatusForbidden {
		t.Errorf("/admin as a user = %d, want 403", rec.Code)
	}
}

func TestAdministrationOverviewListsTheAreasForAnAdministrator(t *testing.T) {
	connect(t, newFakeOps())
	s := adminServer(t)
	rec := get(t, s, "/admin", "X-Test-Session", adminHeader)
	if rec.Code != 200 {
		t.Fatalf("GET /admin = %d", rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body, "<title>Administration - Minion</title>", `<ul class="link-cards">`,
		`<a href="/admin/users">`, `<a href="/admin/settings">`, `<a href="/admin/oidc">`, `<a href="/admin/audit">`,
		`<span aria-current="page">Administration</span>`, `<a href="/">Control center</a>`)
	// A child page has the overview as a real ancestor.
	mustHave(t, get(t, s, "/admin/audit", "X-Test-Session", adminHeader).Body.String(), `<li><a href="/admin">Administration</a></li>`)
}

// ---- the account page ----------------------------------------------------------

func TestAccountShowsOnlyWhatTheSessionKnows(t *testing.T) {
	connect(t, newFakeOps())
	s := adminServer(t)
	body := get(t, s, "/account", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, "<h1>Account</h1>", "<dt>Name</dt><dd>bob</dd>", "<dt>Role</dt><dd>User</dd>",
		`href="/account/password"`, `href="/api-keys"`, `data-theme-select`, `data-json-action="/auth/logout"`)
	// Nothing invented: the session records neither how the person signed in nor
	// any group, so the page says neither.
	mustNotHave(t, body, "Acting as", "Sign-in method", "Authenticated with", "Single sign-on", "minion-admins", "minion-users")
	admin := get(t, s, "/account", "X-Test-Session", adminHeader).Body.String()
	mustHave(t, admin, "<dd>Administrator</dd>")
	// The password page stays reachable, with the same shell.
	rec := get(t, s, "/account/password", "X-Test-Session", userHeader)
	if rec.Code != 200 {
		t.Fatalf("GET /account/password = %d", rec.Code)
	}
	mustHave(t, rec.Body.String(), `id="old_password"`, `id="menu-button"`)
}

func TestAccountNamesTheUserAnAdministratorActsAs(t *testing.T) {
	reg := NewRegistry()
	if err := reg.RegisterPage(Page{Pattern: pathAccount, Title: "Account", Template: "account.html", Data: accountData}); err != nil {
		t.Fatal(err)
	}
	s := actAsServer(t, actAsSessions{target: "bob", expires: actAsNow.Add(time.Hour)}, reg)
	mustHave(t, get(t, s, "/account").Body.String(), "<dt>Acting as</dt><dd>bob (everything you do is audited with your own name)</dd>", "<dd>Administrator</dd>")
}

// ---- Workers and sessions lists ------------------------------------------------

func TestWorkersListFiltersByQueryStatusKindAndKeepsTheFormValues(t *testing.T) {
	s, src := listServer(t, Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent}}, Dashboard{})
	_, all := page(t, s, "/workers", "alice:user")
	mustHave(t, all, "<h1>Workers</h1>", "2 workers.", `href="/workers/add"`, `role="search"`, `name="q"`, `<option value="">Any</option>`,
		`<div id="results" data-live-list sse-connect="/live/workers" sse-swap="dashboard" hx-swap="innerHTML">`)
	mustNotHave(t, all, "Clear filters", `class="scope"`, `name="owner"`)

	_, one := page(t, s, "/workers?q=BUILD&status=offline&kind=infrastructure", "alice:user")
	mustHave(t, one, "1 of 2 workers shown.", "buildbox", `value="BUILD"`, `<option value="offline" selected>Offline</option>`,
		`<option value="infrastructure" selected>infrastructure</option>`, `<a class="btn btn-secondary" href="/workers">Clear filters</a>`,
		`sse-connect="/live/workers?kind=infrastructure&amp;q=BUILD&amp;status=offline"`)
	mustNotHave(t, one, ">laptop<")

	// No match is not the same as nothing: the scope and the query are repeated.
	_, none := page(t, s, "/workers?q=zzz", "alice:user")
	mustHave(t, none, "No workers match", "Scope: Mine.", "Search: <strong>zzz</strong>", `href="/workers">Clear filters</a>`)
	mustNotHave(t, none, "No workers connected")

	// A hostile query stays text.
	_, evil := page(t, s, "/workers?q="+url.QueryEscape(`"><script>alert(1)</script>`), "alice:user")
	mustNotHave(t, evil, "<script>alert")
	mustHave(t, evil, "&lt;script&gt;alert(1)&lt;/script&gt;")
	_ = src
}

func TestWorkersListEmptyStateHasTheNextStepAndNoFakeCounts(t *testing.T) {
	s, _ := listServer(t, Dashboard{}, Dashboard{})
	_, body := page(t, s, "/workers", "alice:user")
	mustHave(t, body, "No workers connected", `<a class="btn" href="/workers/add">`, "0 workers.")
	_, home := page(t, s, "/", "alice:user")
	mustHave(t, home, "No workers connected")
	// No healthy-looking numbers without a worker.
	mustNotHave(t, home, `class="summary"`, "workers online")
}

func TestControlCenterShowsRealCountsAndOnlyRealAttention(t *testing.T) {
	s, _ := listServer(t, Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sWaiting, sIdle}}, Dashboard{})
	_, body := page(t, s, "/", "alice:user")
	mustHave(t, body, "<strong>1 of 2</strong><span>workers online</span>", "<strong>3</strong><span>sessions</span>",
		"<strong>1</strong><span>waiting for an answer</span>", "Needs attention",
		"Session deploy on laptop is waiting for an answer.", `href="/s/laptop/deploy"`,
		"Worker buildbox is offline.", `href="/workers?worker=w2"`, `<a class="btn" href="/workers/add">`)
	// Nothing needs attention: no attention list.
	s2, _ := listServer(t, Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sIdle}}, Dashboard{})
	_, calm := page(t, s2, "/", "alice:user")
	mustNotHave(t, calm, "Needs attention")
	mustHave(t, calm, "<strong>1 of 1</strong><span>workers online</span>")
}

func TestSessionsListLinksToTheSessionAndFiltersByStateKindAndWorker(t *testing.T) {
	s, _ := listServer(t, Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sWaiting, sIdle, sShell}}, Dashboard{})
	_, all := page(t, s, "/sessions", "alice:user")
	mustHave(t, all, "<h1>Sessions</h1>", "4 sessions.", `<a href="/s/laptop/deploy" data-control="name" data-inspect data-inspect-label="deploy on laptop">deploy</a>`, `<a href="/s/buildbox/ci" data-control="name" data-inspect data-inspect-label="ci on buildbox">ci</a>`,
		`<span class="badge" data-state="dialog">Waiting for an answer</span>`, `<span class="badge" data-state="stable">Idle</span>`,
		`<a href="/workers/w1" data-control="worker" data-worker-link>laptop</a>`, `<option value="agent:claude">agent:claude</option>`, `<option value="worker-offline">Worker offline</option>`)
	_, waiting := page(t, s, "/sessions?status=dialog", "alice:user")
	mustHave(t, waiting, "1 of 4 sessions shown.", "deploy")
	mustNotHave(t, waiting, ">notes<", ">fix-tests<")
	_, byTask := page(t, s, "/sessions?q=ship", "alice:user")
	mustHave(t, byTask, "1 of 4 sessions shown.", ">deploy<")
	_, offline := page(t, s, "/sessions?status=worker-offline", "alice:user")
	mustHave(t, offline, "1 of 4 sessions shown.", ">ci<")
	_, one := page(t, s, "/sessions?worker=w1&kind=shell", "alice:user")
	mustHave(t, one, "1 of 4 sessions shown.", ">notes<", `<span class="chip">Worker: laptop</span>`, `<input type="hidden" name="worker" value="w1">`)
	_, none := page(t, s, "/sessions?q=zzz&status=busy", "alice:user")
	mustHave(t, none, "No sessions match", "Filters: status busy", `href="/sessions">Clear filters</a>`)
}

func TestSessionsListWithoutSessionsExplainsHowToStartOne(t *testing.T) {
	s, _ := listServer(t, Dashboard{Workers: []WorkerRow{wPrivate}}, Dashboard{})
	_, body := page(t, s, "/sessions", "alice:user")
	mustHave(t, body, "No sessions yet", "A connected worker can host a new session.", `href="/workers"`)
	// The worker list offers the explicit action that exists: New session, a POST form.
	_, workers := page(t, s, "/workers", "alice:user")
	mustHave(t, workers, `hx-post="/w/laptop/sessions"`, `aria-label="New session on laptop"`, `hx-target="#list-result"`)
}

func TestListScopeStaysServerSideAndOwnerFilterNeedsTheAllScope(t *testing.T) {
	s, src := listServer(t, Dashboard{Workers: []WorkerRow{wPrivate}},
		Dashboard{Workers: []WorkerRow{wPrivate, wInfra, wAlice2}, Sessions: []SessionRow{sAgent, sShell}})
	// An ordinary user cannot widen the list with the URL, with or without an owner.
	_, user := page(t, s, "/workers?scope=all&owner=bob", "alice:user")
	mustNotHave(t, user, "buildbox", `class="scope"`, `name="owner"`)
	for _, a := range src.asked() {
		if a {
			t.Fatalf("an ordinary user's request asked the source for everything: %v", src.asked())
		}
	}
	// An administrator chooses the scope; the owner choices come from the data; the links keep the filters.
	_, admin := page(t, s, "/workers?scope=all&owner=bob&q=build", "alice:admin")
	mustHave(t, admin, `<a href="/workers?owner=bob&amp;q=build">Mine</a>`, `<a href="/workers?owner=bob&amp;q=build&amp;scope=all" aria-current="true">All users</a>`,
		`<select id="f-owner" name="owner">`, `<option value="bob" selected>bob</option>`, `<option value="carol">carol</option>`,
		`<input type="hidden" name="scope" value="all">`, `<th scope="col">Owner</th>`, "1 of 3 workers shown.",
		`sse-connect="/live/workers?owner=bob&amp;q=build&amp;scope=all"`, `href="/workers?scope=all">Clear filters</a>`)
	mustNotHave(t, admin, `data-key="w-w3"`, `data-key="w-w1"`)
	// Two workers with the same name (different owners) are told apart by id in the links.
	_, dup := page(t, s, "/sessions?scope=all", "alice:admin")
	mustHave(t, dup, `href="/s/w1/fix-tests"`, `href="/s/buildbox/ci"`)
}

func TestListLiveStreamsCarryTheFilterOfTheirOwnRequest(t *testing.T) {
	src := newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sShell}}, Dashboard{})
	reg := NewRegistry()
	if err := registerDashboard(reg, func() LiveSource { return src }, nil); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(newServer(t, reg))
	t.Cleanup(ts.Close)
	_, rd, _ := openStream(t, ts, pathLiveWorkers+"?status=offline", "alice:user")
	ev := untilEvent(t, rd, 3*time.Second, func(e sseEvent) bool { return e.name == "dashboard" })
	if !strings.Contains(ev.data, "buildbox") || strings.Contains(ev.data, ">laptop<") || !strings.Contains(ev.data, "1 of 2 workers shown.") {
		t.Errorf("workers stream ignored the filter: %s", ev.data)
	}
	_, rd2, _ := openStream(t, ts, pathLiveSessions+"?q=fix", "alice:user")
	ev = untilEvent(t, rd2, 3*time.Second, func(e sseEvent) bool { return e.name == "dashboard" })
	if !strings.Contains(ev.data, "fix-tests") || strings.Contains(ev.data, ">ci<") || !strings.Contains(ev.data, `data-summary="1 of 2 sessions"`) {
		t.Errorf("sessions stream ignored the filter: %s", ev.data)
	}
	// A new snapshot with a changed row keeps the key the script uses to restore the focus.
	mustHave(t, ev.data, `data-key="s-w1-fix-tests"`)
}

func TestListRowsHaveStableKeysAndNoFilterFormInTheLiveFragment(t *testing.T) {
	v := newFilteredView(Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}, false, ListFilter{})
	for _, name := range []string{"workers_results", "sessions_results", "dashboard"} {
		out, err := renderListFragment(name, v)
		if err != nil {
			t.Fatal(err)
		}
		// The form the person types in is outside the swapped region, so a live update never touches it.
		mustNotHave(t, out, "<form class=\"toolbar\"", `type="search"`, "<input")
	}
	out, _ := renderListFragment("workers_results", v)
	mustHave(t, out, `data-key="w-w1"`)
}

func TestDuplicateWorkerNamesUseIDsInPaths(t *testing.T) {
	refs := workerRefs([]WorkerRow{wPrivate, wAlice2, wInfra})
	if refs["w1"] != "w1" || refs["w3"] != "w3" || refs["w2"] != "buildbox" {
		t.Errorf("refs = %v", refs)
	}
}

func TestCountText(t *testing.T) {
	for _, tc := range []struct {
		shown, total int
		want         string
	}{{0, 0, "0 workers"}, {1, 1, "1 worker"}, {3, 3, "3 workers"}, {1, 3, "1 of 3 workers"}, {0, 1, "0 of 1 worker"}} {
		if got := countText(tc.shown, tc.total, "worker", "workers"); got != tc.want {
			t.Errorf("countText(%d, %d) = %q, want %q", tc.shown, tc.total, got, tc.want)
		}
	}
}

// ---- the session workspace -------------------------------------------------------

func TestSessionWorkspaceIdentifiesTargetOwnerAccessAndConnection(t *testing.T) {
	s, _ := sessionOps(t)
	body := sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	mustHave(t, body,
		`<h1>minion-1 <span class="muted">on pi-1</span></h1>`,
		`Worker <strong>pi-1</strong> <span class="badge" data-online="true">online</span>`, `Owner <strong>bob</strong>`,
		`Access <strong>You can send input</strong>`,
		`data-target="minion-1 on pi-1"`,
		// The connection is its own element, separate from the process state of the panel.
		`<span class="conn" id="conn" data-conn="connecting">Connecting</span>`, `id="age"`, `id="pause-toggle" aria-pressed="false" hidden`,
		`id="copy-screen"`, `id="text-view"`, `<p class="sr-only" id="conn-live" role="status" aria-live="polite">`,
		`<span class="badge" data-state="stable">Idle</span>`, "Captured <time datetime=",
		`<a class="skip-link" href="#session-controls" data-modal-inert>Skip to session controls</a>`,
		`<script src="/static/terminal-input.js" defer></script>`, `<script src="/static/session-ui.js" defer></script>`)
	// The breadcrumb ancestors are real pages.
	mustHave(t, body, `<li><a href="/">Control center</a></li>`, `<li><a href="/sessions">Sessions</a></li>`, `<span aria-current="page">minion-1</span>`)
}

func TestSessionInputDockIsSafeByDefault(t *testing.T) {
	s, _ := sessionOps(t)
	body := sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	// Text: a textarea; Send (primary) is ONE request with enter=1 (the Hub makes it one send_text with submit=true), Enter in the box
	// is the same action, Shift+Enter a line break; "Paste without Enter" is the separate, clearly labelled secondary action. The two
	// buttons are the ONLY source of the enter field: no checkbox, no hidden input, so a request cannot carry two values.
	mustHave(t, body, `<textarea id="send-text" name="text"`, `<button type="submit" id="send-primary" name="enter" value="1">Send</button>`,
		`<button type="submit" id="send-paste" name="enter" value="0" class="btn-secondary" aria-describedby="paste-help">Paste without Enter</button>`,
		"Enter sends the text and presses Enter in the session. Shift+Enter adds a line break.", `to <strong>minion-1</strong> on <strong>pi-1</strong>`)
	mustNotHave(t, body, `type="checkbox"`, `<input type="text" id="send-text"`, `Press Enter after the text`, `Send text</button>`)
	if n := strings.Count(body, `name="enter"`); n != 2 {
		t.Errorf("the text form has %d controls named enter, want exactly the two buttons", n)
	}
	// The explicit keyboard surface for named keys: hidden until the script shows it, focusable, with its own way out, not a form control.
	mustHave(t, body, `id="remote-keys" tabindex="0" role="group"`, `id="remote-keys-leave"`, "Back to the page", "Tab, Shift+Tab and Escape leave this box and are never sent")
	// Keys: Send Enter is its own action, and Interrupt says what it may do.
	mustHave(t, body, `name="key" value="Enter" class="btn-secondary">Send Enter</button>`, `Interrupt (Ctrl-C)</button>`, "Interrupt can stop a command that is running.")
	// Task: separate form, says it sends no terminal input.
	mustHave(t, body, `id="task-text" name="task"`, `data-saved="fix &#34;the&#34; &lt;build&gt;"`, "Task is a label for this session. Saving it does not send terminal input.", `<button type="submit">Save task</button>`)
	// The tabs and the pane toggle exist for a script to show; without it every section is visible.
	mustHave(t, body, `id="dock-tabs" hidden`, `id="pane-toggle" aria-expanded="false" aria-controls="session-pane" hidden`)
	// Writes are marked, so the script can refuse them while paused or reconnecting.
	if n := strings.Count(body, " data-write"); n < 2 {
		t.Errorf("%d write forms marked data-write, want at least text and keys", n)
	}
	// A box left empty without Enter sends no Enter; the omitted checkbox is not the old default.
	s2, f := sessionOps(t)
	post(t, s2, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"echo hi"}})
	if a := argsOf(f, ops.OpSendText, 0); a["submit"] == true {
		t.Errorf("an omitted checkbox sent Enter: %+v", a)
	}
}

func TestSessionPaneIsPluggableAndHasNoInventedConversation(t *testing.T) {
	// This test is about the registry: with the Story 7.5 conversation view out of it, one view is Details and nothing
	// else is invented. The shipped conversation has its own tests (page_pipe_test.go).
	unregisterTestPane(t, "conversation")
	s, _ := sessionOps(t)
	body := sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	mustHave(t, body, `<aside class="session-pane" id="session-pane" aria-labelledby="pane-title">`, `<h2 id="pane-title">Details</h2>`,
		"<dt>Owner</dt><dd>bob</dd>", "<dt>Your access</dt><dd>You can view this session and send input to it.</dd>",
		"<summary>Technical details</summary>", "<code>"+sessWorkerID+"</code>")
	// One view: no tab list. No conversation, chat, message or trust control exists before the Hub has one.
	mustNotHave(t, body, `aria-label="Session views"`, "Conversation", "chat", "Trusted", "trust")

	// Another story adds a view; it appears as a tab with its own body.
	ctx := make(chan SessionContext, 1)
	registerTestPane(t, SessionPane{ID: "conversation-test", Label: "Conversation", Order: 50, Render: func(r *http.Request, sc SessionContext) (template.HTML, error) {
		ctx <- sc
		return template.HTML(`<p id="pane-probe">probe</p>`), nil
	}})
	registerTestPane(t, SessionPane{ID: "broken-test", Label: "Broken", Order: 60, Render: func(*http.Request, SessionContext) (template.HTML, error) {
		return "", fmt.Errorf("boom")
	}})
	registerTestPane(t, SessionPane{ID: "empty-test", Label: "Empty", Order: 70, Render: func(*http.Request, SessionContext) (template.HTML, error) { return "", nil }})
	body = sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	mustHave(t, body, `aria-label="Session views"`, `id="pane-tab-conversation-test"`, `id="pane-tab-details"`, `<p id="pane-probe">probe</p>`, `role="tabpanel"`)
	mustNotHave(t, body, "Broken", "Empty")
	if strings.Index(body, "pane-tab-conversation-test") > strings.Index(body, "pane-tab-details") {
		t.Error("the views are not in Order")
	}
	sc := <-ctx
	if sc.Worker != "pi-1" || sc.Session != "minion-1" || sc.WorkerID != sessWorkerID || !sc.Operate || sc.Owner != "bob" {
		t.Errorf("the pane got %+v", sc)
	}
	for _, bad := range []SessionPane{{ID: "", Label: "x", Render: func(*http.Request, SessionContext) (template.HTML, error) { return "", nil }},
		{ID: "Bad ID", Label: "x", Render: func(*http.Request, SessionContext) (template.HTML, error) { return "", nil }}, {ID: "ok", Label: "", Render: nil}, {ID: "details", Label: "dup", Render: func(*http.Request, SessionContext) (template.HTML, error) { return "", nil }}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("RegisterSessionPane(%+v) did not panic", bad.ID)
				}
			}()
			RegisterSessionPane(bad)
		}()
	}
}

// unregisterTestPane takes a registered view out of the registry for the test and puts it back afterwards.
func unregisterTestPane(t *testing.T, id string) {
	t.Helper()
	paneMu.Lock()
	var kept SessionPane
	found := false
	for i, o := range paneList {
		if o.ID == id {
			kept, found = o, true
			paneList = append(paneList[:i:i], paneList[i+1:]...)
			break
		}
	}
	paneMu.Unlock()
	if !found {
		t.Fatalf("pane %q is not registered", id)
	}
	t.Cleanup(func() {
		paneMu.Lock()
		defer paneMu.Unlock()
		paneList = append(paneList, kept)
		sort.SliceStable(paneList, func(i, j int) bool {
			if paneList[i].Order != paneList[j].Order {
				return paneList[i].Order < paneList[j].Order
			}
			return paneList[i].ID < paneList[j].ID
		})
	})
}

func registerTestPane(t *testing.T, p SessionPane) {
	t.Helper()
	RegisterSessionPane(p)
	t.Cleanup(func() {
		paneMu.Lock()
		defer paneMu.Unlock()
		for i, o := range paneList {
			if o.ID == p.ID {
				paneList = append(paneList[:i], paneList[i+1:]...)
				return
			}
		}
	})
}

func TestSessionViewOnlyUserSeesAccessAndNoDock(t *testing.T) {
	s, f := sessionOps(t)
	foreign := sessionWorker()
	foreign.OwnerID, foreign.Owner = "u-carol", "carol"
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{foreign}})
	body := sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	mustHave(t, body, "Access <strong>View only</strong>", "You can view this session. You don't have permission to send input.", "Owner <strong>carol</strong>")
	mustNotHave(t, body, `id="session-controls"`, `href="#session-controls"`, `New session on`, `id="dock-tabs"`)
}

func TestWriteResultsNameTheTargetTimeAndKeepTheSubmittedTextWhenUnknown(t *testing.T) {
	s, f := sessionOps(t)
	rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"echo hi"}})
	mustHave(t, rec.Body.String(), `<p class="result-meta">minion-1 on pi-1 at `, " UTC</p>")
	mustNotHave(t, rec.Body.String(), "The text that was submitted")

	f.reply(ops.OpSendText, ops.WriteResult{Outcome: ops.OutcomeUnknown, Reason: "timeout"})
	rec = post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"rm -rf <build>"}})
	body := rec.Body.String()
	mustHave(t, body, `class="result result-warning"`, "Delivery is unknown (timeout). Inspect the session before deciding whether to send again.",
		"<summary>The text that was submitted</summary><pre>rm -rf &lt;build&gt;</pre>")
	// Nothing was sent again and no "send anyway" is offered for an unknown outcome.
	mustNotHave(t, body, "Send anyway", `<dialog`)
	if n := len(f.called(ops.OpSendText)); n != 2 {
		t.Errorf("%d send_text calls, want exactly the two the person made", n)
	}
}

func TestForceIsOnlyOfferedByTheBackendAndAsksWithTheNamedTarget(t *testing.T) {
	s, f := sessionOps(t)
	f.reply(ops.OpSendText, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "pane_busy", Forceable: true})
	body := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"ls"}}).Body.String()
	mustHave(t, body, `<form method="post" action="/s/pi-1/minion-1/text"`, `data-force data-target="minion-1 on pi-1"`, `<input type="hidden" name="force" value="1">`,
		`<dialog class="force-dialog" aria-labelledby="force-h">`, `<h2 id="force-h">Send anyway to minion-1 on pi-1?</h2>`,
		"The session may have changed since the screen was captured. This can run a command.", `<button type="button" class="btn-secondary" data-dialog-cancel autofocus>Cancel</button>`)
	// Cancel comes before the final action, so it is the first thing in the dialog.
	if strings.Index(body, "data-dialog-cancel") > strings.Index(body, `class="btn-danger-solid"`) {
		t.Error("the confirming button comes before Cancel")
	}
	// A refusal the backend does not make forceable has no such form.
	f.reply(ops.OpSendText, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "not_shared"})
	mustNotHave(t, post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"ls"}}).Body.String(), "Send anyway", "<dialog", `name="force"`)
	// An answer is never forceable: a stale dialog is read again, not forced.
	f.reply(ops.OpAnswer, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "dialog_changed", Forceable: true})
	ans := post(t, s, "/s/pi-1/minion-1/answer", sessUser, dialogForm(url.Values{"choice": {"1"}}))
	mustNotHave(t, ans.Body.String(), "Send anyway", "<dialog")
	mustHave(t, ans.Header().Get("HX-Trigger"), `"minion-dialog-stale":true`)
}

func TestTaskSaveSaysItSentNoTerminalInputAndTellsTheScript(t *testing.T) {
	s, _ := sessionOps(t)
	rec := post(t, s, "/s/pi-1/minion-1/task", sessUser, url.Values{"task": {"new label"}})
	mustHave(t, rec.Body.String(), "<strong>Task:</strong> Saved. The task is a label for this session; no terminal input was sent.")
	mustHave(t, rec.Header().Get("HX-Trigger"), `"minion-task-saved":true`)
	mustNotHave(t, rec.Header().Get("HX-Trigger"), "minion-input-sent")
}

func TestSessionPanelOffersChoicesAsRadiosAndASeparateSendAnswer(t *testing.T) {
	s, f := sessionOps(t)
	f.reply(ops.OpSessionSnapshot, ops.SessionSnapshotResult{Pane: dialogPane(), Screen: hostileScreen()})
	body := sessGet(t, s, "/s/pi-1/minion-1/screen", sessUser).Body.String()
	mustHave(t, body, `<section class="answer" aria-labelledby="answer-h">`, "The page cannot read the option labels", `data-answer-form`,
		`<input type="radio" name="choice" value="1">`, `<input type="radio" name="choice" value="y">`, `<button type="submit">Send answer</button>`)
	mustNotHave(t, body, `name="choice" value="1" checked`, "<button type=\"submit\" name=\"choice\"")
}

// ---- tokens and contrast ------------------------------------------------------

var cssVar = regexp.MustCompile(`--([a-z0-9-]+):\s*(#[0-9a-fA-F]{6})`)

// cssBlock returns the declarations of the first rule that starts with sel.
func cssBlock(t *testing.T, css, sel string) string {
	t.Helper()
	i := strings.Index(css, sel+" {")
	if i < 0 {
		t.Fatalf("minion.css has no rule %q", sel)
	}
	rest := css[i+len(sel)+2:]
	return rest[:strings.Index(rest, "}")]
}

func tokens(block string) map[string]string {
	out := map[string]string{}
	for _, m := range cssVar.FindAllStringSubmatch(block, -1) {
		out[m[1]] = strings.ToLower(m[2])
	}
	return out
}

func luminance(hex string) float64 {
	ch := func(s string) float64 {
		v, _ := strconv.ParseUint(s, 16, 8)
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(hex[1:3]) + 0.7152*ch(hex[3:5]) + 0.0722*ch(hex[5:7])
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// The two dark rule sets (the operator's choice and the operating system's) are
// one design written twice; they must never drift apart.
func TestDarkTokensAreTheSameInBothRuleSets(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("static", "minion.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	chosen := tokens(cssBlock(t, css, `:root[data-theme="dark"]`))
	i := strings.Index(css, "@media (prefers-color-scheme: dark)")
	if i < 0 {
		t.Fatal("no system dark rule set")
	}
	system := tokens(cssBlock(t, css[i:], `:root:not([data-theme="light"])`))
	if len(chosen) < 15 || len(chosen) != len(system) {
		t.Fatalf("dark rule sets differ in size: %d and %d", len(chosen), len(system))
	}
	for k, v := range chosen {
		if system[k] != v {
			t.Errorf("--%s is %s when chosen and %s for the system theme", k, v, system[k])
		}
	}
	// A dark theme replaces every semantic colour the light theme defines.
	light := tokens(cssBlock(t, css, ":root"))
	for _, k := range []string{"canvas", "surface", "surface-raised", "surface-subtle", "text", "muted", "border", "input-border", "accent", "on-accent", "accent-subtle",
		"positive", "positive-subtle", "warning", "warning-subtle", "danger", "on-danger", "danger-subtle", "focus"} {
		if _, ok := light[k]; !ok {
			t.Errorf("light theme lacks --%s", k)
		}
		if _, ok := chosen[k]; !ok {
			t.Errorf("dark theme lacks --%s", k)
		}
	}
	// Navigation and terminal keep their own tokens in both themes.
	for _, k := range []string{"nav", "terminal", "terminal-text"} {
		if _, ok := chosen[k]; ok {
			t.Errorf("dark theme redefines --%s, which must stay the same in both themes", k)
		}
	}
}

// Contrast of the text pairs the pages really use, in both themes (WCAG 2.2: 4.5:1
// for text, 3:1 for focus and control boundaries).
func TestThemeContrastOfTheRealTextPairs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("static", "minion.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	light := tokens(cssBlock(t, css, ":root"))
	dark := tokens(cssBlock(t, css, `:root[data-theme="dark"]`))
	for name, th := range map[string]map[string]string{"light": light, "dark": dark} {
		get := func(k string) string {
			if v, ok := th[k]; ok {
				return v
			}
			return light[k] // nav and terminal tokens are shared
		}
		for _, p := range []struct {
			fg, bg string
			min    float64
		}{
			{"text", "canvas", 4.5}, {"text", "surface", 4.5}, {"text", "surface-subtle", 4.5}, {"muted", "surface", 4.5}, {"muted", "canvas", 4.5},
			{"muted", "surface-subtle", 4.5}, {"accent", "surface", 4.5}, {"accent", "canvas", 4.5}, {"accent", "accent-subtle", 4.5}, {"on-accent", "accent", 4.5},
			{"positive", "positive-subtle", 4.5}, {"warning", "warning-subtle", 4.5}, {"danger", "danger-subtle", 4.5}, {"on-danger", "danger", 4.5},
			{"text", "accent-subtle", 4.5}, {"danger", "surface", 4.5}, {"warning", "surface", 4.5}, {"positive", "surface", 4.5},
			{"nav-text", "nav", 4.5}, {"nav-muted", "nav", 4.5}, {"nav-text", "nav-selected", 4.5}, {"terminal-text", "terminal", 4.5},
			{"focus", "surface", 3}, {"focus", "canvas", 3}, {"border", "surface", 1.4},
			// The boundary of an editable control against its own fill and against every surface a field sits on
			// (page, panel, raised dialog, subtle panel, the answer box, a toolbar chip...): WCAG 1.4.11 asks for 3:1.
			{"input-border", "surface", 3}, {"input-border", "canvas", 3}, {"input-border", "surface-raised", 3}, {"input-border", "surface-subtle", 3},
			{"input-border", "warning-subtle", 3}, {"input-border", "accent-subtle", 3}, {"input-border", "danger-subtle", 3}, {"input-border", "positive-subtle", 3},
		} {
			if c := contrast(get(p.fg), get(p.bg)); c < p.min {
				t.Errorf("%s: --%s on --%s is %.2f:1, want at least %.1f:1", name, p.fg, p.bg, c, p.min)
			}
		}
	}
}

// ---- scripts of the shell -------------------------------------------------------

func TestWorkbenchScripts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script, _ := filepath.Abs(filepath.Join("testdata", "workbench_ui_test.js"))
	static, _ := filepath.Abs("static")
	out, err := exec.Command(node, script, static).CombinedOutput()
	if err != nil {
		t.Fatalf("workbench scripts: %v\n%s", err, out)
	}
}

func TestStaticAssetsOfTheShellAreServed(t *testing.T) {
	s := newServer(t, NewRegistry())
	for p, want := range map[string]string{"/static/theme.js": "minion-theme", "/static/shell.js": "aria-expanded", "/static/live-list.js": "htmx:sseBeforeMessage",
		"/static/session-ui.js": "showModal", "/static/minion.css": "prefers-reduced-motion", "/static/terminal-input.js": "htmx:beforeSend"} {
		rec := get(t, s, p)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %d, want a body with %q", p, rec.Code, want)
		}
	}
}

// ---- review round 2: scope links and stable focus identities ---------------------

// An administrator's drill-down links keep the All users scope. Without it the
// link opens Mine, the backend (correctly) leaves another owner's sessions out and
// the person sees a misleading empty list.
func TestWorkerDrillDownAndAttentionLinksKeepTheAllUsersScope(t *testing.T) {
	carolSession := SessionRow{WorkerID: "w3", Worker: "laptop", Owner: "carol", Name: "build", Kind: "shell", State: ops.PaneStateStable, Online: true}
	carolOffline := WorkerRow{ID: "w9", Name: "carol-box", Owner: "carol", Kind: ops.WorkerKindPrivate, Online: false}
	mine := Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}
	all := Dashboard{Workers: []WorkerRow{wPrivate, wAlice2, carolOffline}, Sessions: []SessionRow{sAgent, carolSession}}
	s, _ := listServer(t, mine, all)

	// The workers list, the control center and the live fragment of the workers list.
	_, workers := page(t, s, "/workers?scope=all", "alice:admin")
	mustHave(t, workers, `href="/sessions?scope=all&amp;worker=w3"`)
	mustNotHave(t, workers, `href="/sessions?worker=w3"`)
	_, home := page(t, s, "/?scope=all", "alice:admin")
	mustHave(t, home, `href="/sessions?scope=all&amp;worker=w3"`, `href="/workers?scope=all&amp;worker=w9"`)
	mustNotHave(t, home, `href="/workers?worker=w9"`, `href="/sessions?worker=w3"`)
	view := newFilteredView(all, true, ListFilter{})
	frag, err := renderListFragment("workers_results", view)
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, frag, `href="/sessions?scope=all&amp;worker=w3"`)

	// Following the link shows the other owner's session; the same link without the scope does not.
	_, followed := page(t, s, "/sessions?scope=all&worker=w3", "alice:admin")
	mustHave(t, followed, ">build<", "1 of 2 sessions shown.")
	_, bare := page(t, s, "/sessions?worker=w3", "alice:admin")
	mustNotHave(t, bare, ">build<")

	// Mine stays Mine, and an ordinary user can never broaden the scope with the URL.
	_, mineWorkers := page(t, s, "/workers", "alice:admin")
	mustHave(t, mineWorkers, `href="/sessions?worker=w1"`)
	mustNotHave(t, mineWorkers, "scope=all&amp;worker")
	_, user := page(t, s, "/workers?scope=all", "alice:user")
	mustHave(t, user, `href="/sessions?worker=w1"`)
	mustNotHave(t, user, `href="/sessions?scope=all`, "carol")
}

// Focus is restored by control identity (data-control) inside the keyed row. Every
// control of a keyed row must carry one, and a control that is only sometimes
// there (the session-count link) must not change the identity of the others.
func TestKeyedRowControlsHaveStableIdentitiesWhateverTheSessionCount(t *testing.T) {
	control := regexp.MustCompile(`<(a|button|input|select|textarea)\b[^>]*>`)
	identity := regexp.MustCompile(`data-control="([a-z-]+)"`)
	count := func(n int) map[string]bool {
		w := WorkerRow{ID: "w1", Name: "laptop", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true, Version: "1.0"}
		var sess []SessionRow
		for i := 0; i < n; i++ {
			sess = append(sess, SessionRow{WorkerID: "w1", Worker: "laptop", Owner: "alice", Name: fmt.Sprintf("minion-%d", i+1), Kind: "shell", State: ops.PaneStateStable, Online: true})
		}
		out, err := renderListFragment("workers_results", newFilteredView(Dashboard{Workers: []WorkerRow{w}, Sessions: sess}, false, ListFilter{}))
		if err != nil {
			t.Fatal(err)
		}
		ids := map[string]bool{}
		for _, tag := range control.FindAllString(out, -1) {
			if strings.Contains(tag, `type="hidden"`) {
				continue
			}
			m := identity.FindStringSubmatch(tag)
			if m == nil {
				t.Errorf("a control of a keyed row has no data-control: %s", tag)
				continue
			}
			if ids[m[1]] {
				t.Errorf("data-control %q is used twice in one row", m[1])
			}
			ids[m[1]] = true
		}
		return ids
	}
	none, one := count(0), count(1)
	if none["sessions"] || !one["sessions"] {
		t.Errorf("session-count link: with no session %v, with one %v", none["sessions"], one["sessions"])
	}
	if !none["new-session"] || !one["new-session"] {
		t.Errorf("New session lost its identity: %v %v", none, one)
	}
	sessions, err := renderListFragment("sessions_results", newFilteredView(Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}, false, ListFilter{}))
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, sessions, `data-control="name"`, `data-control="worker"`)
	// The Open button of the Actions column is gone; the row (name link, or anywhere else) opens the session pane.
	mustNotHave(t, sessions, `data-control="open"`, `>Actions<`, `>Open</a>`)
}

// A control that sits on a semantic fill takes its text colour from a semantic
// token too. The solid danger button ("Send anyway" in the confirmation) was white
// in every theme, which is 1.88:1 on the dark theme's light-red --danger. The test
// follows the real paths: it reads the declarations of the .btn-danger-solid rules
// and resolves their var() references in each of the three routes a page can end
// up in (light, the operator's Dark, and System with a dark OS preference).
func TestSolidDangerButtonTextHasContrastOnEveryThemeRoute(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("static", "minion.css"))
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	light := tokens(cssBlock(t, css, ":root"))
	dark := tokens(cssBlock(t, css, `:root[data-theme="dark"]`))
	i := strings.Index(css, "@media (prefers-color-scheme: dark)")
	if i < 0 {
		t.Fatal("no system dark rule set")
	}
	system := tokens(cssBlock(t, css[i:], `:root:not([data-theme="light"])`))
	routes := map[string]map[string]string{"light": light, "explicit dark": dark, "system dark": system}

	// Every rule that styles .btn-danger-solid, whatever its selector prefix.
	rule := regexp.MustCompile(`(?m)^([^{}\n]*\.btn-danger-solid[^{}\n]*)\{([^}]*)\}`)
	rules := rule.FindAllStringSubmatch(css, -1)
	if len(rules) != 1 {
		t.Fatalf("want one rule for .btn-danger-solid (the text colour must not be overridden per theme), got %d: %q", len(rules), rules)
	}
	decl := func(name string) string {
		m := regexp.MustCompile(`(?:^|[;\s])` + name + `:\s*([^;]+)`).FindStringSubmatch(rules[0][2])
		if m == nil {
			t.Fatalf(".btn-danger-solid has no %s declaration", name)
		}
		return strings.TrimSpace(m[1])
	}
	resolve := func(route map[string]string, v string) string {
		if m := regexp.MustCompile(`^var\(--([a-z0-9-]+)\)$`).FindStringSubmatch(v); m != nil {
			c, ok := route[m[1]]
			if !ok {
				t.Fatalf("--%s is not defined for this theme route", m[1])
			}
			return c
		}
		if !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(v) {
			t.Fatalf("cannot resolve the colour %q", v)
		}
		return strings.ToLower(v)
	}
	fg, bg := decl("color"), decl("background")
	if !strings.HasPrefix(fg, "var(--on-danger)") {
		t.Errorf(".btn-danger-solid text colour is %q, want the semantic token var(--on-danger)", fg)
	}
	for name, route := range routes {
		c := contrast(resolve(route, fg), resolve(route, bg))
		if c < 4.5 {
			t.Errorf("%s: .btn-danger-solid text %s on %s is %.2f:1, want at least 4.5:1", name, resolve(route, fg), resolve(route, bg), c)
		}
		// The danger text on its own subtle fill and the focus ring stay verified on this route too.
		for _, p := range []struct {
			fg, bg string
			min    float64
		}{{"danger", "danger-subtle", 4.5}, {"danger", "surface", 4.5}, {"focus", "surface", 3}, {"on-danger", "danger", 4.5}, {"input-border", "danger-subtle", 3}} {
			if c := contrast(route[p.fg], route[p.bg]); c < p.min {
				t.Errorf("%s: --%s on --%s is %.2f:1, want at least %.1f:1", name, p.fg, p.bg, c, p.min)
			}
		}
	}
}
