package web

// Tests of the server side of the session inspector (a contextual right pane
// that shows the compact session view in a same-origin frame): the framing
// headers, the compact embedded view, the markup of the three list pages and of
// the Workers disclosure, and the assets. The browser half is tested against a
// DOM stub in testdata/inspector_test.js (see TestInspectorScripts); neither is
// browser certification.

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func cspOf(t *testing.T, h http.Header) string {
	t.Helper()
	csp := h.Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy")
	}
	return csp
}

// ---- the compact view in the frame -------------------------------------------

func TestEmbeddedSessionViewIsCompactAndOnlyFrameableBySameOrigin(t *testing.T) {
	s, _ := sessionOps(t)
	rec := sessGet(t, s, "/s/pi-1/minion-1?embed=1", sessUser)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET embed = %d\n%s", rec.Code, rec.Body)
	}
	csp := cspOf(t, rec.Header())
	for _, want := range []string{"frame-ancestors 'self'", "frame-src 'none'", "script-src 'self'", "style-src 'self'", "default-src 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("embedded CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "*") || strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "http") {
		t.Errorf("embedded CSP is too open (or still closed): %s", csp)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	body := rec.Body.String()
	// The same session workspace: one implementation, the same forms and ids.
	mustHave(t, body, `<div id="session-view" data-embed="true"`, `data-worker-id="`+sessWorkerID+`"`, `data-session="minion-1"`, `data-target="minion-1 on pi-1"`,
		`<h1>minion-1 <span class="muted">on pi-1</span></h1>`, `<body class="embed"`,
		`hx-post="/s/pi-1/minion-1/text"`, `hx-post="/s/pi-1/minion-1/keys"`, `hx-post="/s/pi-1/minion-1/task"`,
		`id="send-primary" name="enter" value="1">Send</button>`, `id="pause-toggle"`, `id="copy-screen"`, `id="text-view"`,
		`<script src="/static/terminal-input.js" defer></script>`, `<script src="/static/session-ui.js" defer></script>`, `<script src="/static/frame-child.js" defer></script>`)
	mustNotHave(t, body, ` checked`, `type="checkbox"`) // nothing is pre-checked, and there is no Enter box: Send is the Enter action
	// No global chrome, nothing that could move the frame to another resource.
	mustNotHave(t, body, `id="site-rail"`, `id="menu-button"`, `class="topbar"`, `class="crumbs"`, `id="banner-slot"`, `id="flash-area"`, `class="skip-link" href="#content">Skip to content</a>`+"\n"+`<div class="shell">`,
		`hx-post="/w/pi-1/sessions"`, `id="pane-toggle"`, `id="session-pane"`)
	if m := regexp.MustCompile(`<a [^>]*href="/[^>]*>`).FindString(body); m != "" {
		t.Errorf("the compact view has a link that would move the frame: %s", m)
	}
}

func TestEmbedIsOnlyTheExactFlagAndTheFullViewKeepsItsDefaults(t *testing.T) {
	s, _ := sessionOps(t)
	for _, path := range []string{"/s/pi-1/minion-1", "/s/pi-1/minion-1?embed=0", "/s/pi-1/minion-1?embed=true", "/s/pi-1/minion-1?embed=", "/s/pi-1/minion-1?Embed=1"} {
		rec := sessGet(t, s, path, sessUser)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		csp := cspOf(t, rec.Header())
		if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "frame-src 'none'") || strings.Contains(csp, "'self'; frame-ancestors 'self'") {
			t.Errorf("%s: the full view must keep frame-ancestors 'none': %s", path, csp)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY", path, got)
		}
		body := rec.Body.String()
		mustHave(t, body, `id="site-rail"`, `id="pane-toggle"`, `hx-post="/w/pi-1/sessions"`)
		mustNotHave(t, body, `data-embed`, `frame-child.js`, `<body class="embed"`)
	}
}

// The embed flag is a layout choice: it never opens anything the full page does not.
func TestEmbedFlagBypassesNoAuthenticationAuthorisationOrFraming(t *testing.T) {
	s, f := sessionOps(t)
	// Signed out: the same answer as without the flag, and nothing of the session.
	plain := get(t, s, "/s/pi-1/minion-1")
	flagged := get(t, s, "/s/pi-1/minion-1?embed=1")
	if plain.Code != flagged.Code || !strings.HasPrefix(flagged.Header().Get("Location"), "/login") || !strings.HasPrefix(plain.Header().Get("Location"), "/login") {
		t.Errorf("signed out: %d (%q) with the flag, %d (%q) without", flagged.Code, flagged.Header().Get("Location"), plain.Code, plain.Header().Get("Location"))
	}
	if flagged.Code == http.StatusOK || strings.Contains(flagged.Body.String(), "plain line") || strings.Contains(flagged.Body.String(), `id="session-view"`) {
		t.Errorf("signed out with the flag: %d %s", flagged.Code, flagged.Body)
	}
	if got := flagged.Header().Get("X-Frame-Options"); got == "SAMEORIGIN" || strings.Contains(flagged.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") {
		t.Errorf("a signed-out answer must not be frameable: XFO %q, CSP %q", got, flagged.Header().Get("Content-Security-Policy"))
	}
	if len(f.called(ops.OpSessionSnapshot)) != 0 {
		t.Error("a snapshot was read for a signed-out request")
	}
	// A Worker the user cannot see is the same plain 404, with a message and no detail.
	rec := sessGet(t, s, "/s/nope/minion-1?embed=1", sessUser)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown Worker embedded = %d, want 404", rec.Code)
	}
	if len(f.called(ops.OpSessionSnapshot)) != 0 {
		t.Error("a snapshot was requested for a Worker the user cannot see")
	}
	body := rec.Body.String()
	mustHave(t, body, `<body class="embed"`, "That page does not exist.")
	mustNotHave(t, body, `id="site-rail"`, "nope")
	if m := regexp.MustCompile(`<a [^>]*href="/[^>]*>`).FindString(body); m != "" {
		t.Errorf("an embedded error has a link that would move the frame: %s", m)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("an embedded error is shown in the frame: X-Frame-Options = %q", got)
	}
	// A refused snapshot looks like a missing page, embedded or not.
	f.on(ops.OpSessionSnapshot, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 403, Code: "forbidden", Message: "secret detail"}
	})
	rec = sessGet(t, s, "/s/pi-1/minion-1?embed=1", sessUser)
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "secret detail") {
		t.Errorf("refused snapshot embedded = %d %s", rec.Code, rec.Body)
	}
	// The write routes do not know the flag: they are the same operations behind the same checks.
	text := post(t, s, "/s/pi-1/minion-1/text?embed=1", "", map[string][]string{"text": {"x"}})
	if len(f.called(ops.OpSendText)) != 0 {
		t.Errorf("a signed-out write reached the operation (%d)", text.Code)
	}
	if h := text.Header().Get("X-Frame-Options"); h == "SAMEORIGIN" {
		t.Errorf("a write answer must not be marked frameable")
	}
}

func TestOnlyTheSessionPageIsEmbeddable(t *testing.T) {
	s := dashboardServer(t, newFakeSource(Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}, Dashboard{}))
	for _, path := range []string{"/?embed=1", "/workers?embed=1", "/sessions?embed=1"} {
		rec := get(t, s, path, "X-Test-Session", "alice:user")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		body := rec.Body.String()
		mustHave(t, body, `id="site-rail"`)
		mustNotHave(t, body, `<body class="embed"`)
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, a list page is never framed", path, got)
		}
	}
}

// ---- the pages that host the inspector ---------------------------------------

func TestListPagesHostTheInspectorAndNoOtherPageFramesAnything(t *testing.T) {
	s, _ := listServer(t, Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}, Dashboard{})
	for _, path := range []string{"/", "/workers", "/sessions"} {
		rec := get(t, s, path, "X-Test-Session", "alice:user")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, rec.Code)
		}
		csp := cspOf(t, rec.Header())
		if !strings.Contains(csp, "frame-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "*") || strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s: CSP = %s", path, csp)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, a host page is never framed", path, got)
		}
		body := rec.Body.String()
		mustHave(t, body, `<aside class="inspector" id="inspector" aria-labelledby="inspector-title" data-context="`, `id="inspector-frames"`, `id="inspector-open" href="/sessions"`,
			`id="inspector-close"`, `<dialog class="force-dialog" id="inspector-capacity"`, `id="inspector-kept"`, `<p class="notice notice-danger inspector-alert" id="inspector-alert" role="alert" hidden></p>`, `<script src="/static/inspector.js" defer></script>`,
			`<script src="/static/live-list.js" defer></script>`)
		if strings.Contains(body, "<iframe") {
			t.Errorf("%s: the frame is made by the script on demand, never by the server", path)
		}
		// The list stays in its own column, apart from the pane and the live region.
		mustHave(t, body, `<div class="inspect-main" data-modal-inert>`)
	}
	for _, path := range []string{"/account", "/api-keys"} {
		rec := get(t, s, path, "X-Test-Session", "alice:user")
		if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
			t.Logf("%s = %d", path, rec.Code)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: an ordinary page forbids frames and framing: %s", path, csp)
		}
	}
	login := get(t, s, "/login")
	if csp := login.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") || login.Header().Get("X-Frame-Options") != "DENY" {
		t.Errorf("the login page keeps the anti-clickjacking defaults: %s", csp)
	}
}

func TestSessionLinksOpenTheInspectorButKeepTheirHrefAndOtherControlsAreLeftAlone(t *testing.T) {
	dash := Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sWaiting, sShell}}
	s, _ := listServer(t, dash, Dashboard{})
	for _, path := range []string{"/", "/sessions"} {
		_, body := page(t, s, path, "alice:user")
		mustHave(t, body,
			`<a href="/s/laptop/fix-tests" data-control="name" data-inspect data-inspect-label="fix-tests on laptop">fix-tests</a>`,
			`<tr data-worker="w1" data-session="fix-tests" data-key="s-w1-fix-tests" data-row-link="session">`,
			`<a href="/workers/w1" data-control="worker" data-worker-link>laptop</a>`)
		// The Open button and the Actions column are gone: the name, or anywhere in the row, opens the pane.
		mustNotHave(t, body, `aria-label="Open session fix-tests on laptop"`, `data-control="open" data-inspect data-inspect-label="fix-tests on laptop"`)
		// Not hijacked: New session, the sessions count link, filters, Add worker.
		for _, m := range regexp.MustCompile(`<[^>]*data-inspect[ >][^>]*>`).FindAllString(body, -1) {
			if !strings.Contains(m, `<a `) || !strings.Contains(m, `href="/s/`) {
				t.Errorf("%s: %s is marked for the inspector but is not a session link", path, m)
			}
		}
		mustNotHave(t, body, `data-control="new-session" data-inspect`, `data-control="sessions" data-inspect`)
	}
	// The attention link of a session that waits is a session link too; a worker's is not.
	_, home := page(t, s, "/", "alice:user")
	mustHave(t, home, `<li data-worker="w1" data-session="deploy" data-key="a-w1-deploy">`, `href="/s/laptop/deploy" data-inspect data-inspect-label="deploy on laptop" data-control="open"`)
	mustHave(t, home, `<a href="/workers?worker=w2">View worker</a>`)
}

// ---- the Workers page: authorised sessions, the same data -------------------

func TestWorkersPageOffersTheSessionsOfTheSameSnapshotAsLinks(t *testing.T) {
	dash := Dashboard{Workers: []WorkerRow{wPrivate, wInfra, {ID: "w9", Name: "empty", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true}},
		Sessions: []SessionRow{sAgent, sWaiting, sShell}}
	s, _ := listServer(t, dash, Dashboard{})
	_, body := page(t, s, "/workers", "alice:user")
	mustHave(t, body,
		`<details class="worker-sessions" data-keep-open="ws-w1">`,
		`<summary data-control="sessions-toggle">Show sessions<span class="sr-only"> on laptop</span></summary>`,
		`<ul class="worker-session-list" aria-label="Sessions on laptop">`,
		`<li data-worker="w1" data-session="fix-tests" data-key="ws-w1-fix-tests"><a href="/s/laptop/fix-tests" data-control="session:fix-tests" data-inspect data-inspect-label="fix-tests on laptop" aria-label="Open session fix-tests on laptop">fix-tests</a>`,
		`<li data-worker="w1" data-session="deploy" data-key="ws-w1-deploy">`,
		`<details class="worker-sessions" data-keep-open="ws-w2">`, `<li data-worker="w2" data-session="ci" data-key="ws-w2-ci">`)
	// The same sessions as the session list shows: nothing is added, nothing of another worker.
	w1 := body[strings.Index(body, `data-keep-open="ws-w1"`):strings.Index(body, `data-keep-open="ws-w2"`)]
	if strings.Contains(w1, "/s/buildbox/ci") || strings.Count(w1, "data-inspect-label=") != 2 {
		t.Errorf("worker w1 lists other sessions than its own: %s", w1)
	}
	// A worker without sessions has no disclosure (no invented sessions or counts).
	if strings.Contains(body, `data-keep-open="ws-w9"`) {
		t.Error("a worker with no session offers a disclosure")
	}
	mustHave(t, body, `<td data-label="Sessions" class="num">0</td>`)
	// The count link still leads to the filtered list; New session is untouched.
	mustHave(t, body, `data-control="sessions"`, `data-control="new-session" aria-label="New session on laptop"`)
	// The control center's worker table is a summary: no disclosure there.
	_, home := page(t, s, "/", "alice:user")
	mustNotHave(t, home, `worker-sessions`, `data-keep-open`)
}

func TestWorkersDisclosureIsBoundedAndSaysSo(t *testing.T) {
	var sessions []SessionRow
	for i := 0; i < maxWorkerSessions+3; i++ {
		sessions = append(sessions, SessionRow{WorkerID: "w1", Worker: "laptop", Owner: "alice", Name: "s" + strconv.Itoa(i), Kind: "shell", State: "stable", Online: true})
	}
	s, _ := listServer(t, Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: sessions}, Dashboard{})
	_, body := page(t, s, "/workers", "alice:user")
	if n := strings.Count(body, `data-inspect-label=`); n != maxWorkerSessions {
		t.Errorf("listed %d sessions, want the bound %d", n, maxWorkerSessions)
	}
	mustHave(t, body, fmt.Sprintf(`All %d sessions</a>`, maxWorkerSessions+3), `href="/sessions?worker=w1"`)
}

// The live stream sends the same fragment, so a live update keeps the disclosure
// and the link identities the page was rendered with.
func TestWorkersLiveFragmentHasTheSameDisclosure(t *testing.T) {
	d := Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}
	v := newFilteredView(d, false, ListFilter{})
	v.discloseSessions()
	out, err := renderListFragment("workers_results", v)
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, out, `data-keep-open="ws-w1"`, `data-control="session:fix-tests" data-inspect`)
	plain, err := renderListFragment("workers_results", newFilteredView(d, false, ListFilter{}))
	if err != nil {
		t.Fatal(err)
	}
	mustNotHave(t, plain, `data-keep-open`)
}

func TestAllUsersScopeListsOnlyWhatTheSnapshotAuthorises(t *testing.T) {
	mine := Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}
	all := Dashboard{Workers: []WorkerRow{wPrivate, wInfra}, Sessions: []SessionRow{sAgent, sShell}}
	s, src := listServer(t, mine, all)
	_, userBody := page(t, s, "/workers?scope=all", "alice:user")
	mustNotHave(t, userBody, "buildbox", "/s/buildbox/ci")
	if got := src.asked(); len(got) != 1 || got[0] {
		t.Errorf("a user's request asked the source with all=%v", got)
	}
	_, adminBody := page(t, s, "/workers?scope=all", "root:admin")
	mustHave(t, adminBody, `<li data-worker="w2" data-session="ci"`, `href="/s/buildbox/ci"`)
}

// ---- assets -------------------------------------------------------------------

func readStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("static", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInspectorScriptsNeverWriteStoreOrBuildMarkup(t *testing.T) {
	for _, name := range []string{"inspector.js", "frame-child.js"} {
		src := readStatic(t, name)
		for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "XMLHttpRequest", "sendBeacon", "htmx.ajax", "localStorage", "sessionStorage", "document.cookie", "'*'", `"*"`, "WebSocket", "EventSource"} {
			// Comments may name what the script does not do; code may not use it.
			code := regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(src, "")
			if strings.Contains(code, bad) {
				t.Errorf("%s uses %q", name, bad)
			}
		}
		if !strings.Contains(src, "postMessage") && name == "frame-child.js" {
			t.Error("frame-child.js does not talk to its host")
		}
	}
	// frame-child.js requests nothing at all. inspector.js makes exactly one kind of request, its own guarded READ of the sign-in:
	// a credentialed GET of the sessions of one worker, no body, no method, redirects not followed.
	if strings.Contains(regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(readStatic(t, "frame-child.js"), ""), "fetch(") {
		t.Error("frame-child.js makes a request")
	}
	js := readStatic(t, "inspector.js")
	code := regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(js, "")
	if n := strings.Count(code, "fetch("); n != 1 {
		t.Errorf("inspector.js has %d calls of fetch(, want the guarded read only", n)
	}
	// fetchJSON is the one place that asks: credentialed, uncached, redirects not followed, aborted after its deadline.
	call := regexp.MustCompile(`(?s)window\.fetch\(url, (\{.*?\})\)`).FindStringSubmatch(code)
	if call == nil {
		t.Fatal("the bounded read of inspector.js was not found")
	}
	for _, want := range []string{"credentials: 'same-origin'", "cache: 'no-store'", "redirect: 'manual'", "signal:"} {
		if !strings.Contains(call[1], want) {
			t.Errorf("the bounded read lacks %s: %s", want, call[1])
		}
	}
	for _, bad := range []string{"method", "body:", "POST", "PUT", "DELETE", "X-Minion-CSRF", "document.cookie", "sessionID", "csrf"} {
		// Whole words only: the tag name INPUT of the row-click exclusions is not the verb PUT.
		if regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(bad) + `($|[^A-Za-z0-9_])`).MatchString(code) {
			t.Errorf("inspector.js mentions %q: its only requests are plain GETs of the two context routes", bad)
		}
	}
	for _, want := range []string{"fetchJSON('/inspector/principal'", "fetchJSON(rec.full + '/context'", "ctl.abort()"} {
		if !strings.Contains(code, want) {
			t.Errorf("inspector.js lacks %s", want)
		}
	}
	if n := strings.Count(code, "fetchJSON("); n != 3 { // the definition and the two reads
		t.Errorf("inspector.js has %d fetchJSON( mentions, want the definition and the two context reads", n)
	}
	// Every message to a frame names the exact origin; the allowed types are the three read-only ones.
	if !strings.Contains(js, "w.postMessage(msg, origin)") {
		t.Error("inspector.js must post to the exact origin")
	}
	for _, send := range regexp.MustCompile(`send\(rec, '([a-z-]+)'`).FindAllStringSubmatch(js, -1) {
		if send[1] != "suspend" && send[1] != "resume" && send[1] != "query" {
			t.Errorf("inspector.js sends the message type %q", send[1])
		}
	}
}

func TestInspectorAssetsAreServedAndTheCSSHasTheDrawer(t *testing.T) {
	s, _ := listServer(t, Dashboard{}, Dashboard{})
	for p, want := range map[string]string{"/static/inspector.js": "close-request", "/static/frame-child.js": "minion-frame-control", "/static/minion.css": ".inspector-scrim"} {
		rec := get(t, s, p)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s = %d, want it to contain %q", p, rec.Code, want)
		}
	}
}

// ---- the stylesheet at the breakpoints ---------------------------------------

func TestInspectorIsDockedFromTheWideLayoutAndAModalDrawerBelow(t *testing.T) {
	rules := loadSheet(t)
	pane := el("aside", "inspector")
	scrim := el("div", "inspector-scrim open")
	layout := el("div", "inspect-layout inspect-open")
	declared := regexp.MustCompile(`--inspector-w:\s*(\d+)px`).FindStringSubmatch(loadCSS(t))
	if declared == nil {
		t.Fatal("no --inspector-w token")
	}
	paneWidth, _ := strconv.Atoi(declared[1])
	for _, w := range []int{320, 375, 768, 959, 960, 1279} {
		if p := valueAt(rules, "position", pane, scripted, w); p != "fixed" {
			t.Errorf("the open inspector has position %q at %dpx, want fixed (a drawer)", p, w)
		}
		if d := valueAt(rules, "display", scrim, scripted, w); d != "block" {
			t.Errorf("the scrim has display %q at %dpx", d, w)
		}
		vz, e1 := strconv.Atoi(valueAt(rules, "z-index", scrim, scripted, w))
		pz, e2 := strconv.Atoi(valueAt(rules, "z-index", pane, scripted, w))
		if e1 != nil || e2 != nil || pz <= vz {
			t.Errorf("at %dpx the drawer (z-index %d) must stand above its scrim (%d)", w, pz, vz)
		}
		if g := valueAt(rules, "grid-template-columns", layout, scripted, w); strings.Contains(g, "clamp") {
			t.Errorf("at %dpx the list must not give room to a docked pane: %q", w, g)
		}
		// Fits the layout viewport whatever the scrollbar gutter is (see the session pane test).
		for _, gutter := range []int{0, 15, 17} {
			l := w - gutter
			px := evalWidth(t, strings.ReplaceAll(valueAt(rules, "width", pane, scripted, w), "--inspector-w", "--pane-w"), paneWidth, w, l)
			if px > l {
				t.Errorf("at %dpx (gutter %d) the drawer is %dpx wide in a %dpx layout viewport", w, gutter, px, l)
			}
			if w <= paneWidth+gutter && px != l {
				t.Errorf("at %dpx a narrow drawer should fill the layout viewport (%dpx), got %dpx", w, l, px)
			}
		}
	}
	for _, w := range []int{1280, 1920} {
		if p := valueAt(rules, "position", pane, append([]cssEl{layout}, scripted...), w); p != "sticky" {
			t.Errorf("the docked inspector has position %q at %dpx, want sticky beside the list", p, w)
		}
		if d := valueAt(rules, "display", scrim, scripted, w); d != "none" {
			t.Errorf("the scrim has display %q at %dpx while docked", d, w)
		}
		if g := valueAt(rules, "grid-template-columns", layout, scripted, w); !strings.HasPrefix(g, "minmax(0, 1fr) clamp(") {
			t.Errorf("at %dpx the list keeps a flexible column beside the docked pane: %q", w, g)
		}
	}
	// Hidden means hidden, scripted or not; the frame fills the pane.
	for _, anc := range [][]cssEl{scripted, nil} {
		hidden := el("aside", "inspector", "hidden", "")
		if d := valueAt(rules, "display", hidden, anc, 1400); d != "none" {
			t.Errorf("a hidden inspector has display %q", d)
		}
		if d := valueAt(rules, "display", el("iframe", "inspector-frame", "hidden", ""), anc, 1400); d != "none" {
			t.Errorf("a hidden frame has display %q", d)
		}
	}
	if b := valueAt(rules, "border", el("iframe", "inspector-frame"), nil, 1400); b != "0" {
		t.Errorf("the frame border = %q", b)
	}
}

// ---- the browser half, against a DOM stub --------------------------------------

func TestInspectorScripts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script, _ := filepath.Abs(filepath.Join("testdata", "inspector_test.js"))
	static, _ := filepath.Abs("static")
	out, err := exec.Command(node, script, static).CombinedOutput()
	if err != nil {
		t.Fatalf("inspector scripts: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}

// ---- review fixes: the real shell, and the compact layout ---------------------------------------------------

func parseHTML(t *testing.T, body string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func attrOf(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func hasAncestorOrSelf(n *html.Node, pred func(*html.Node) bool) bool {
	for ; n != nil; n = n.Parent {
		if n.Type == html.ElementNode && pred(n) {
			return true
		}
	}
	return false
}

// focusable lists the elements of a document that the Tab key can reach (a tabindex of -1 can be focused by a script only).
func focusable(doc *html.Node) []*html.Node {
	var out []*html.Node
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			_, href := attrOf(n, "href")
			typ, _ := attrOf(n, "type")
			ti, hasTI := attrOf(n, "tabindex")
			switch {
			case n.Data == "a" && href, n.Data == "button", n.Data == "select", n.Data == "textarea", n.Data == "summary", n.Data == "iframe",
				n.Data == "input" && typ != "hidden":
				out = append(out, n)
			case hasTI && ti != "-1":
				out = append(out, n)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// Review finding 6: only data-modal-inert subtrees became inert, and the shell's skip link sits outside them (with a z-index above the
// drawer), so Shift+Tab from the opening heading reached it. This takes the REAL markup of every list page and asks of every element
// the Tab key can reach: is it inside the inspector pane or its dialog, or does it sit in a subtree that inspector.js makes inert
// (data-modal-inert, or the skip link by its class)?
func TestEveryFocusableElementOutsideTheInspectorIsInertWhileTheDrawerIsModal(t *testing.T) {
	s, _ := listServer(t, Dashboard{Workers: []WorkerRow{wPrivate}, Sessions: []SessionRow{sAgent}}, Dashboard{})
	for _, path := range []string{"/", "/workers", "/sessions"} {
		_, body := page(t, s, path, "alice:user")
		doc := parseHTML(t, body)
		n := 0
		for _, el := range focusable(doc) {
			n++
			inPane := hasAncestorOrSelf(el, func(x *html.Node) bool {
				id, _ := attrOf(x, "id")
				return id == "inspector" || id == "inspector-capacity"
			})
			if inPane {
				continue
			}
			inert := hasAncestorOrSelf(el, func(x *html.Node) bool {
				if _, ok := attrOf(x, "data-modal-inert"); ok {
					return true
				}
				cls, _ := attrOf(x, "class")
				return x.Data == "a" && strings.Contains(" "+cls+" ", " skip-link ")
			})
			if !inert {
				var sb strings.Builder
				_ = html.Render(&sb, el)
				t.Errorf("%s: %s can take the focus behind the modal drawer", path, strings.SplitN(sb.String(), ">", 2)[0]+">")
			}
		}
		if n < 10 {
			t.Errorf("%s: only %d focusable elements found: the check did not see the real shell", path, n)
		}
	}
	// The skip links carry the marker themselves as well, so they stay inert wherever a page puts them.
	_, body := page(t, s, "/sessions", "alice:user")
	mustHave(t, body, `<a class="skip-link" href="#content" data-modal-inert>`)
}

func TestInspectorFrameTakesTheHeightThePaneGives(t *testing.T) {
	rules := loadSheet(t)
	for _, w := range []int{320, 800, 1279, 1280, 1920} {
		frame := el("iframe", "inspector-frame")
		if v := valueAt(rules, "min-height", frame, scripted, w); v != "0" {
			t.Errorf("the frame has min-height %q at %dpx: on a short window it would be clipped by the pane (a drawer of the full window height, a docked pane sized to the window)", v, w)
		}
	}
	for _, w := range []int{1280, 1920} {
		pane := el("aside", "inspector")
		layout := el("div", "inspect-layout inspect-open")
		if v := valueAt(rules, "min-height", pane, append([]cssEl{layout}, scripted...), w); v != "0" {
			t.Errorf("the docked pane has min-height %q at %dpx: it cannot be shorter than that on a short window", v, w)
		}
	}
	if strings.Contains(loadCSS(t), "min-height: 24rem") || strings.Contains(loadCSS(t), "min-height: 28rem") {
		t.Error("a fixed minimum height of the pane or its frame is back")
	}
}

// The compact view is a fixed-height column in the frame: the screen takes what is left and scrolls inside, the input dock sits below
// it with its own bound, so the Text composer and Send stay in view. This evaluates the cascade of the real style sheet for the embedded
// document (body.embed) and for the full page; the browser result (real heights, zoom, phone) is a Shared browser check, not this test.
// fullTermMax is the height bound of the screen of the full page (unchanged by the compact view): 62vh, and from 960px the
// viewport-fit rule.
func fullTermMax(w int) string {
	if w >= 960 {
		return "max(12rem, calc(100vh - 31rem))"
	}
	return "62vh"
}

func TestCompactSessionViewKeepsTheComposerInViewAndTheFullPageIsUntouched(t *testing.T) {
	rules := loadSheet(t)
	emb := []cssEl{el("body", "embed")}
	for _, w := range []int{320, 560, 800, 960, 1400} {
		for _, c := range []struct {
			prop, class, tag string
			embedded, full   string
		}{
			{"display", "session-grid", "div", "flex", "grid"},
			{"max-height", "term", "pre", "none", fullTermMax(w)},
			{"min-height", "term", "pre", "4rem", "12rem"},
			{"flex", "term", "pre", "1 1 auto", ""},
			{"overflow-y", "dock", "div", "auto", ""},
			{"flex", "dock", "div", "0 1 auto", ""},
			{"display", "terminal-card", "div", "flex", ""},
		} {
			e := el(c.tag, c.class)
			if got := valueAt(rules, c.prop, e, emb, w); got != c.embedded {
				t.Errorf("embedded .%s %s at %dpx = %q, want %q", c.class, c.prop, w, got, c.embedded)
			}
			if got := valueAt(rules, c.prop, e, nil, w); got != c.full {
				t.Errorf("the full page .%s %s at %dpx = %q, want the unchanged %q", c.class, c.prop, w, got, c.full)
			}
		}
		// The 62vh / viewport-fit rules of the full page no longer decide the height of the screen in the frame.
		if got := valueAt(rules, "max-height", el("pre", "term"), emb, w); strings.Contains(got, "vh") || strings.Contains(got, "calc") {
			t.Errorf("the screen in the frame keeps a viewport-height rule at %dpx: %q", w, got)
		}
		if got := valueAt(rules, "overflow", el("body", "embed"), nil, w); got != "hidden" {
			t.Errorf("the frame document must not scroll as a whole while it is a fixed column: overflow %q", got)
		}
		// The boxes that can grow without bound scroll inside their own limit, so they never push Send out of view.
		for _, id := range []string{"session-result", "session-unknown"} {
			if got := valueAt(rules, "max-height", el("div", "", "id", id), emb, w); got != "8rem" {
				t.Errorf("#%s in the frame has max-height %q at %dpx, want 8rem", id, got, w)
			}
		}
		if got := valueAt(rules, "max-height", el("section", "answer"), emb, w); got != "45%" {
			t.Errorf("the answer panel has max-height %q at %dpx", got, w)
		}
	}
	// A short frame (a landscape phone, 200 percent zoom) falls back to the document flow: the frame scrolls as a whole.
	css := loadCSS(t)
	i := strings.Index(css, "@media (max-height: 520px)")
	if i < 0 {
		t.Fatal("no short-frame fallback")
	}
	block := css[i:]
	block = block[:strings.Index(block, "\n}\n")]
	for _, want := range []string{"body.embed { height: auto; min-height: 100vh; overflow: auto; display: block; }", "max-height: none; overflow: visible"} {
		if !strings.Contains(block, want) {
			t.Errorf("the short-frame fallback lacks %q", want)
		}
	}
	// Markup order: in the compact view the screen comes first and the dock (Text composer, Send) after it, in one card.
	s, _ := sessionOps(t)
	body := sessGet(t, s, "/s/pi-1/minion-1?embed=1", sessUser).Body.String()
	screen, dock, send := strings.Index(body, `id="screen-panel"`), strings.Index(body, `id="session-controls"`), strings.Index(body, `id="send-primary"`)
	if screen <= 0 || screen >= dock || dock >= send {
		t.Errorf("order in the compact view: screen %d, dock %d, Send %d", screen, dock, send)
	}
	mustHave(t, body, `<label for="send-text">Text to send</label>`, `<textarea id="send-text" name="text" rows="3"`, `class="dock-target">Input goes to <strong>minion-1</strong> on <strong>pi-1</strong>`)
}
