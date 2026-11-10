package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const (
	sessWorkerID = "0190a1b2-c3d4-7e5f-8a6b-0000000000a1"
	sessOtherID  = "0190a1b2-c3d4-7e5f-8a6b-0000000000a2"
	sessUser     = "bob:user"
)

// hostileScreen is a screen a Worker could send: markup in the text, a style
// with a colour outside the model, a span out of range, an overlapping span, a
// cursor inside a styled run and one beyond the end of a short line.
func hostileScreen() ops.Screen {
	return ops.Screen{
		Width: 40, Height: 4,
		Lines: []string{
			`<script>alert(1)</script> & "q" 'x'`,
			`plain line`,
			"\u65e5\u672c\u8a9e ok",
			`ab`,
		},
		Spans: []ops.StyleSpan{
			{Line: 0, Start: 0, End: 8, FG: "p196", Bold: true},
			{Line: 0, Start: 8, End: 17, FG: `"><x onload=1>`, BG: "#00ff7f"}, // a bad colour is dropped, a good one stays
			{Line: 0, Start: 10, End: 12, FG: "p1"},                           // overlaps the span before: skipped
			{Line: 1, Start: 2, End: 99, FG: "p2"},                            // out of range: skipped
			{Line: 2, Start: 0, End: 3, Inverse: true, FG: "p4"},
			{Line: 9, Start: 0, End: 1, FG: "p1"}, // a line that does not exist
		},
		Cursor: &ops.Cursor{Row: 3, Col: 5},
	}
}

func sessionWorker() ops.WorkerInfo {
	return ops.WorkerInfo{ID: sessWorkerID, Name: "pi-1", OwnerID: "u-bob", Owner: "bob", Online: true}
}

// sessionOps is a fake caller for the session view: one Worker "pi-1" of bob,
// a snapshot of the hostile screen, a task, and delivered writes.
func sessionOps(t *testing.T) (*Server, *fakeOps) {
	t.Helper()
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{sessionWorker()}}).
		reply(ops.OpSessionSnapshot, ops.SessionSnapshotResult{
			Pane:   ops.PaneInfo{Session: "minion-1", ID: "%1", Kind: "agent:claude", State: "stable"},
			Screen: hostileScreen(),
		}).
		reply(ops.OpSessionsList, ops.SessionsListResult{Sessions: []ops.SessionInfo{
			{Name: "minion-2", Task: "other"}, {Name: "minion-1", Task: `fix "the" <build>`}}}).
		reply(ops.OpSendText, delivered).reply(ops.OpSendKeys, delivered).reply(ops.OpSetTask, delivered).
		reply(ops.OpAnswer, delivered)
	connect(t, f)
	return adminServer(t), f
}

// dialogPane is the pane of a snapshot that shows a dialog, with the reviewed
// metadata a current Worker reports (EPICS A-22).
func dialogPane() ops.PaneInfo {
	return ops.PaneInfo{Session: "minion-1", ID: "%1", Command: "claude", Kind: "agent:claude", State: ops.PaneStateDialog,
		PID: 4242, SessionCreated: 1794308400, DialogDigest: strings.Repeat("ab", 32)}
}

// dialogForm is what the browser posts for an answer form of dialogPane: the
// given fields and the hidden tokens of the panel.
func dialogForm(kv url.Values) url.Values {
	v := url.Values{
		fieldExpectPaneID: {"%1"}, fieldExpectPID: {"4242"}, fieldExpectCommand: {"claude"},
		fieldExpectCreated: {"1794308400"}, fieldExpectDigest: {strings.Repeat("ab", 32)},
	}
	for k, vals := range kv {
		v[k] = vals
	}
	return v
}

func sessGet(t *testing.T, s *Server, path, user string) *httptest.ResponseRecorder {
	t.Helper()
	if user == "" {
		return get(t, s, path)
	}
	return get(t, s, path, "X-Test-Session", user)
}

// -- the screen --------------------------------------------------------------

func TestScreenTemplateGoldenWithHostileContent(t *testing.T) {
	var sb strings.Builder
	if err := sessionTemplates().ExecuteTemplate(&sb, "screen", NewScreenView(hostileScreen())); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	golden(t, "session_screen", got)
	// Hostile text is text, never markup, and no style leaks from a bad colour.
	mustNotHave(t, got, "<script", "<x", "onload", "style=", "99")
	mustHave(t, got, "&lt;script&gt;", "alert(1)&lt;", "/script&gt; &amp; &#34;q&#34; &#39;x&#39;")
}

func TestNewScreenViewRuns(t *testing.T) {
	type run = ScreenRun
	for _, tc := range []struct {
		name   string
		screen ops.Screen
		want   [][]run
	}{
		{"plain", ops.Screen{Lines: []string{"abc"}}, [][]run{{{"abc", ""}}}},
		{"empty line has no runs", ops.Screen{Lines: []string{""}}, [][]run{nil}},
		{"span", ops.Screen{Lines: []string{"abcdef"}, Spans: []ops.StyleSpan{{Line: 0, Start: 1, End: 3, FG: "p1", Bold: true}}},
			[][]run{{{"a", ""}, {"bc", "t-b t-fg-1"}, {"def", ""}}}},
		{"true colour background", ops.Screen{Lines: []string{"ab"}, Spans: []ops.StyleSpan{{Line: 0, Start: 0, End: 2, BG: "#0a1b2c", Underline: true, Strike: true, Italic: true, Dim: true}}},
			[][]run{{{"ab", "t-dim t-i t-u t-s t-bgx-0a1b2c"}}}},
		{"inverse swaps and defaults", ops.Screen{Lines: []string{"ab", "cd"}, Spans: []ops.StyleSpan{
			{Line: 0, Start: 0, End: 2, Inverse: true},
			{Line: 1, Start: 0, End: 2, Inverse: true, FG: "p1", BG: "p2"}}},
			[][]run{{{"ab", "t-fg-bgdef t-bg-fgdef"}}, {{"cd", "t-fg-2 t-bg-1"}}}},
		{"overlap, out of range and bad colour are dropped", ops.Screen{Lines: []string{"abcdef"}, Spans: []ops.StyleSpan{
			{Line: 0, Start: 0, End: 3, FG: "p1"}, {Line: 0, Start: 2, End: 4, FG: "p2"}, {Line: 0, Start: 4, End: 99, FG: "p3"},
			{Line: 0, Start: 4, End: 5, FG: "red"}, {Line: 0, Start: 5, End: 5, FG: "p4"}, {Line: -1, Start: 0, End: 1, FG: "p5"}}},
			[][]run{{{"abc", "t-fg-1"}, {"def", ""}}}},
		{"cursor in text", ops.Screen{Lines: []string{"abc"}, Cursor: &ops.Cursor{Row: 0, Col: 1}},
			[][]run{{{"a", ""}, {"b", "t-cur"}, {"c", ""}}}},
		{"cursor in a styled run", ops.Screen{Lines: []string{"abc"}, Spans: []ops.StyleSpan{{Line: 0, Start: 0, End: 3, Bold: true}},
			Cursor: &ops.Cursor{Row: 0, Col: 2}},
			[][]run{{{"ab", "t-b"}, {"c", "t-b t-cur"}}}},
		{"cursor beyond the line", ops.Screen{Lines: []string{"ab"}, Cursor: &ops.Cursor{Row: 0, Col: 4}},
			[][]run{{{"ab  ", ""}, {" ", "t-cur"}}}},
		{"cursor on a row that does not exist or a negative column", ops.Screen{Lines: []string{"ab"}, Cursor: &ops.Cursor{Row: 5, Col: 1}},
			[][]run{{{"ab", ""}}}},
		{"spans count runes", ops.Screen{Lines: []string{"\u65e5\u672c\u8a9ex"}, Spans: []ops.StyleSpan{{Line: 0, Start: 1, End: 3, FG: "p9"}}},
			[][]run{{{"\u65e5", ""}, {"\u672c\u8a9e", "t-fg-9"}, {"x", ""}}}},
	} {
		v := NewScreenView(tc.screen)
		if len(v.Lines) != len(tc.want) {
			t.Errorf("%s: %d lines, want %d", tc.name, len(v.Lines), len(tc.want))
			continue
		}
		for i, l := range v.Lines {
			if len(l.Runs) != len(tc.want[i]) {
				t.Errorf("%s: line %d runs = %+v, want %+v", tc.name, i, l.Runs, tc.want[i])
				continue
			}
			for j, r := range l.Runs {
				if r != tc.want[i][j] {
					t.Errorf("%s: line %d run %d = %+v, want %+v", tc.name, i, j, r, tc.want[i][j])
				}
			}
		}
	}
}

func TestNewScreenViewBoundsTheCursorPad(t *testing.T) {
	v := NewScreenView(ops.Screen{Lines: []string{""}, Cursor: &ops.Cursor{Row: 0, Col: 1 << 30}})
	n := 0
	for _, r := range v.Lines[0].Runs {
		n += len(r.Text)
	}
	if n > maxCursorPad+1 {
		t.Errorf("cursor pad = %d bytes, want <= %d", n, maxCursorPad+1)
	}
}

func TestEverySpanClassIsDefinedByTheStyleSheetOrTheScript(t *testing.T) {
	// Every class the server can emit must be one that is defined, so that a
	// style never silently disappears: the fixed names are in minion.css, and the
	// script builds the colour rules (t-fg/bg-N, t-fgx/bgx) of the colours on screen.
	css, err := os.ReadFile(filepath.Join("static", "minion.css"))
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile(filepath.Join("static", "terminal-input.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{".t-b", ".t-dim", ".t-i", ".t-u", ".t-s", ".t-fg-bgdef", ".t-bg-fgdef", ".t-cur"} {
		if !strings.Contains(string(css), c) {
			t.Errorf("minion.css does not define %q", c)
		}
	}
	for _, c := range []string{"t-fg", "t-bg", "t-fgx", "t-bgx"} {
		if !strings.Contains(string(js), c) {
			t.Errorf("terminal-input.js does not mention %q", c)
		}
	}
}

// -- the page ----------------------------------------------------------------

func TestSessionPageRendersScreenControlsAndCallsTheSnapshot(t *testing.T) {
	s, f := sessionOps(t)
	rec := sessGet(t, s, "/s/pi-1/minion-1", sessUser)
	if rec.Code != 200 {
		t.Fatalf("GET = %d\n%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	mustHave(t, body,
		`<div id="screen-panel">`, `<pre class="term" id="screen"`,
		`&lt;script&gt;`, `/script&gt; &amp; &#34;q&#34;`,
		`<span class="t-b t-fg-196">`,
		`Kind: agent:claude.`, `State: stable.`, `Size: 40x4.`,
		`data-events-url="/api/v1/events?all=true&amp;worker=`+sessWorkerID+`"`,
		`data-screen-url="/s/pi-1/minion-1/screen"`, `data-session="minion-1"`,
		`hx-post="/s/pi-1/minion-1/text"`, `hx-post="/s/pi-1/minion-1/keys"`, `hx-post="/s/pi-1/minion-1/task"`,
		`hx-post="/w/pi-1/sessions"`,
		`value="fix &#34;the&#34; &lt;build&gt;"`, // the task of this session, escaped
		`<script src="/static/terminal-input.js" defer></script>`)
	for _, k := range sessionKeys {
		mustHave(t, body, `name="key" value="`+k.Key+`" class="btn-secondary">`+k.Label+`</button>`)
	}
	mustNotHave(t, body, "<script>alert", "onload", ` style=`, "Send anyway")
	// The snapshot call names the Worker by id and the session by name.
	calls := f.called(ops.OpSessionSnapshot)
	if len(calls) != 1 {
		t.Fatalf("snapshot calls = %d", len(calls))
	}
	tgt, _ := calls[0].Args["target"].(map[string]any)
	if tgt["worker_id"] != sessWorkerID || tgt["session_name"] != "minion-1" || calls[0].User != "bob" {
		t.Errorf("snapshot args = %+v as %q", calls[0].Args, calls[0].User)
	}
}

func TestSessionPageHasNoInlineStyleOrScriptAndKeepsCSP(t *testing.T) {
	s, _ := sessionOps(t)
	rec := sessGet(t, s, "/s/pi-1/minion-1", sessUser)
	body := rec.Body.String()
	for _, bad := range []*regexp.Regexp{regexp.MustCompile(`(?i)\sstyle=`), regexp.MustCompile(`(?i)<style`), regexp.MustCompile(`(?i)\son[a-z]+=`),
		regexp.MustCompile(`(?i)<script(?:\s[^>]*)?>[^<]`)} {
		if bad.MatchString(body) {
			t.Errorf("page matches %v", bad)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "style-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestSessionPageResolvesTheWorkerByIdToo(t *testing.T) {
	s, f := sessionOps(t)
	rec := sessGet(t, s, "/s/"+sessWorkerID+"/minion-1", sessUser)
	if rec.Code != 200 {
		t.Fatalf("GET by id = %d", rec.Code)
	}
	mustHave(t, rec.Body.String(), `hx-post="/s/`+sessWorkerID+`/minion-1/text"`)
	if len(f.called(ops.OpSessionSnapshot)) != 1 {
		t.Error("no snapshot call")
	}
}

func TestSessionPageEscapesPathParts(t *testing.T) {
	s, _ := sessionOps(t)
	rec := sessGet(t, s, "/s/pi-1/"+url.PathEscape(`a"><b`), sessUser)
	body := rec.Body.String()
	mustNotHave(t, body, `a"><b`)
}

func TestSessionPageForUnknownWorkerIs404AndAsksNothingElse(t *testing.T) {
	s, f := sessionOps(t)
	rec := sessGet(t, s, "/s/nope/minion-1", sessUser)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET unknown Worker = %d, want 404", rec.Code)
	}
	if len(f.called(ops.OpSessionSnapshot)) != 0 {
		t.Error("a snapshot was requested for a Worker the user cannot see")
	}
}

func TestSessionPageNeedsASession(t *testing.T) {
	s, f := sessionOps(t)
	rec := sessGet(t, s, "/s/pi-1/minion-1", "")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Errorf("anonymous GET = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, p := range []string{"/s/pi-1/minion-1/screen", "/s/pi-1/minion-1/text", "/w/pi-1/sessions"} {
		method := http.MethodPost
		if strings.HasSuffix(p, "/screen") {
			method = http.MethodGet
		}
		req := httptest.NewRequest(method, p, nil)
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusSeeOther && rr.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d", method, p, rr.Code)
		}
	}
	if f.total() != 0 {
		t.Errorf("an anonymous request reached the operations: %+v", f.calls)
	}
}

func TestSessionPageWithoutACallerAnswers503(t *testing.T) {
	SetAdminOps(nil)
	s := adminServer(t)
	if rec := sessGet(t, s, "/s/pi-1/minion-1", sessUser); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET without a caller = %d, want 503", rec.Code)
	}
	rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"x"}})
	mustHave(t, rec.Body.String(), "not connected")
}

func TestSessionPageOfAnOfflineWorkerShowsANotice(t *testing.T) {
	s, f := sessionOps(t)
	w := sessionWorker()
	w.Online = false
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w}})
	rec := sessGet(t, s, "/s/pi-1/minion-1", sessUser)
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	mustHave(t, rec.Body.String(), "This Worker is offline")
	mustNotHave(t, rec.Body.String(), `id="screen-panel"`, `hx-post="/s/pi-1/minion-1/text"`)
	if len(f.called(ops.OpSessionSnapshot)) != 0 {
		t.Error("a snapshot was requested from an offline Worker")
	}
	// The fragment route says so too, with a status htmx does not swap.
	if rec := sessGet(t, s, "/s/pi-1/minion-1/screen", sessUser); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("panel of an offline Worker = %d, want 503", rec.Code)
	}
}

func TestSessionPageWorkerGoingOfflineBetweenTheCalls(t *testing.T) {
	s, f := sessionOps(t)
	f.on(ops.OpSessionSnapshot, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 503, Code: "worker_offline", Message: "the Worker is offline"}
	})
	rec := sessGet(t, s, "/s/pi-1/minion-1", sessUser)
	if rec.Code != 200 {
		t.Fatalf("GET = %d\n%s", rec.Code, rec.Body)
	}
	mustHave(t, rec.Body.String(), "This Worker is offline")
}

func TestSessionPageRefusedSnapshotLooksLikeAMissingPage(t *testing.T) {
	s, f := sessionOps(t)
	for _, status := range []int{403, 404} {
		f.on(ops.OpSessionSnapshot, func(map[string]any) (any, error) {
			return nil, &OpError{Status: status, Code: "forbidden", Message: "no"}
		})
		if rec := sessGet(t, s, "/s/pi-1/minion-1", sessUser); rec.Code != http.StatusNotFound {
			t.Errorf("GET with snapshot %d = %d, want 404", status, rec.Code)
		}
		if rec := sessGet(t, s, "/s/pi-1/minion-1/screen", sessUser); rec.Code != http.StatusNotFound {
			t.Errorf("panel with snapshot %d = %d, want 404", status, rec.Code)
		}
	}
	f.on(ops.OpSessionSnapshot, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 502, Code: "internal", Message: strings.Repeat("x", 1000)}
	})
	rec := sessGet(t, s, "/s/pi-1/minion-1/screen", sessUser)
	if rec.Code != http.StatusBadGateway || len(rec.Body.String()) > maxSnapshotErrorRunes+8 {
		t.Errorf("panel with a failing snapshot = %d (%d bytes)", rec.Code, rec.Body.Len())
	}
}

func TestSessionPageSameNameOfTwoOwnersUsesTheOwn(t *testing.T) {
	s, f := sessionOps(t)
	mine, theirs := sessionWorker(), sessionWorker()
	theirs.ID, theirs.OwnerID, theirs.Owner = sessOtherID, "u-carol", "carol"
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{theirs, mine}})
	sessGet(t, s, "/s/pi-1/minion-1", sessUser)
	tgt, _ := f.called(ops.OpSessionSnapshot)[0].Args["target"].(map[string]any)
	if tgt["worker_id"] != sessWorkerID {
		t.Errorf("snapshot went to %v, want the own Worker", tgt["worker_id"])
	}
	// An admin who sees two Workers of that name, none of them own, must use the id.
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{theirs, {ID: "0190a1b2-c3d4-7e5f-8a6b-0000000000a3", Name: "pi-1", OwnerID: "u-dave"}}})
	if rec := sessGet(t, s, "/s/pi-1/minion-1", adminHeader); rec.Code != http.StatusNotFound {
		t.Errorf("ambiguous name = %d, want 404", rec.Code)
	}
	if rec := sessGet(t, s, "/s/"+sessOtherID+"/minion-1", adminHeader); rec.Code != 200 {
		t.Errorf("by id = %d, want 200", rec.Code)
	}
}

func TestFindWorker(t *testing.T) {
	a := ops.WorkerInfo{ID: "a", Name: "x", OwnerID: "u1"}
	b := ops.WorkerInfo{ID: "b", Name: "x", OwnerID: "u2"}
	c := ops.WorkerInfo{ID: "c", Name: "y", OwnerID: "u2"}
	for _, tc := range []struct {
		list []ops.WorkerInfo
		ref  string
		user string
		want string
	}{
		{[]ops.WorkerInfo{a, b, c}, "x", "u1", "a"},
		{[]ops.WorkerInfo{a, b, c}, "x", "u3", ""}, // two foreign ones: ambiguous
		{[]ops.WorkerInfo{a, c}, "y", "u1", "c"},   // one foreign one: fine
		{[]ops.WorkerInfo{a, b, c}, "b", "u1", "b"},
		{[]ops.WorkerInfo{a, b}, "zzz", "u1", ""},
	} {
		w, ok := findWorker(tc.list, tc.ref, tc.user)
		if (tc.want == "") == ok || w.ID != tc.want {
			t.Errorf("findWorker(%q,%q) = %q, %v; want %q", tc.ref, tc.user, w.ID, ok, tc.want)
		}
	}
}

func TestSessionPathHelpersEscape(t *testing.T) {
	if got := SessionPath("pi 1", `a/b?c`); got != "/s/pi%201/a%2Fb%3Fc" {
		t.Errorf("SessionPath = %q", got)
	}
	if got := NewSessionPath("pi 1"); got != "/w/pi%201/sessions" {
		t.Errorf("NewSessionPath = %q", got)
	}
}

// The browser opens the Hub's own feed; the two constants must stay equal.
func TestEventsPathIsTheAPIEventsPath(t *testing.T) {
	if eventsPath != api.EventsPath {
		t.Errorf("eventsPath = %q, api.EventsPath = %q", eventsPath, api.EventsPath)
	}
}

// -- who may operate ---------------------------------------------------------

func TestSessionPageHidesControlsWithoutOperateButTheAPIStillDecides(t *testing.T) {
	s, f := sessionOps(t)
	foreign := sessionWorker()
	foreign.OwnerID, foreign.Owner = "u-carol", "carol"
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{foreign}})
	// A dialog is open: the answer form is a control too.
	f.reply(ops.OpSessionSnapshot, ops.SessionSnapshotResult{
		Pane: dialogPane(), Screen: hostileScreen()})

	rec := sessGet(t, s, "/s/pi-1/minion-1", sessUser)
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body, `<pre class="term"`, "State: dialog.") // watching is allowed...
	mustNotHave(t, body, `hx-post=`, `id="session-controls"`, `href="#session-controls"`, `id="send-text"`, `name="task"`, `name="key"`, `class="answer"`)
	if len(f.called(ops.OpSessionsList)) != 0 {
		t.Error("the task was read for a user who cannot operate")
	}
	// ...but the fragment has no answer form either.
	panel := sessGet(t, s, "/s/pi-1/minion-1/screen", sessUser).Body.String()
	mustNotHave(t, panel, `<form`)

	// Defence in depth: the routes do not trust the hidden controls. Every write
	// is forwarded and the API's refusal is what the user sees.
	f.mu.Lock()
	for _, op := range []string{ops.OpSendText, ops.OpSendKeys, ops.OpSetTask, ops.OpAnswer, ops.OpSessionCreate} {
		f.handlers[op] = func(map[string]any) (any, error) {
			return nil, &OpError{Status: 403, Code: "forbidden", Message: "you may not operate this session"}
		}
	}
	f.mu.Unlock()
	for path, form := range map[string]url.Values{
		"/s/pi-1/minion-1/text":   {"text": {"ls"}},
		"/s/pi-1/minion-1/keys":   {"key": {"Enter"}},
		"/s/pi-1/minion-1/task":   {"task": {"x"}},
		"/s/pi-1/minion-1/answer": dialogForm(url.Values{"choice": {"1"}}),
		"/w/pi-1/sessions":        {},
	} {
		rr := post(t, s, path, sessUser, form)
		mustHave(t, rr.Body.String(), "you may not operate this session")
		if rr.Header().Get("HX-Redirect") != "" {
			t.Errorf("POST %s redirected after a refusal", path)
		}
	}
	for _, op := range []string{ops.OpSendText, ops.OpSendKeys, ops.OpSetTask, ops.OpAnswer, ops.OpSessionCreate} {
		if len(f.called(op)) != 1 {
			t.Errorf("%s was called %d times, want 1 (the route must forward it to the API)", op, len(f.called(op)))
		}
	}
}

func TestSessionPageOfAnAdminWithoutOwnershipOperates(t *testing.T) {
	s, f := sessionOps(t)
	foreign := sessionWorker()
	foreign.OwnerID, foreign.Owner = "u-carol", "carol"
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{foreign}})
	rec := sessGet(t, s, "/s/pi-1/minion-1", adminHeader)
	mustHave(t, rec.Body.String(), `hx-post="/s/pi-1/minion-1/text"`)
	// An admin sees every Worker: the list is asked with all=true.
	if a := f.called(ops.OpWorkersList)[0].Args; a["all"] != true {
		t.Errorf("workers_list args = %+v", a)
	}
	// A user's list is not widened.
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{sessionWorker()}})
	sessGet(t, s, "/s/pi-1/minion-1", sessUser)
	calls := f.called(ops.OpWorkersList)
	if a := calls[len(calls)-1].Args; a["all"] == true {
		t.Errorf("a user's workers_list args = %+v", a)
	}
}

// While an admin acts as another user the API sees that user, so the page does
// too: not admin (list not widened) and operate decided by ownership.
func TestSessionPageWhileActingAsUsesTheTarget(t *testing.T) {
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{
			{ID: sessWorkerID, Name: "pi-1", OwnerID: "u-target", Online: true}}}).
		reply(ops.OpSessionSnapshot, ops.SessionSnapshotResult{Screen: ops.Screen{Lines: []string{"x"}}}).
		reply(ops.OpSessionsList, ops.SessionsListResult{})
	connect(t, f)
	s := actAsServer(t, actAsSessions{target: "bob", expires: time.Now().Add(time.Hour)}, DefaultRegistry)
	rec := get(t, s, "/s/pi-1/minion-1")
	if rec.Code != 200 {
		t.Fatalf("GET = %d\n%s", rec.Code, rec.Body)
	}
	mustHave(t, rec.Body.String(), `hx-post="/s/pi-1/minion-1/text"`)
	if a := f.called(ops.OpWorkersList)[0].Args; a["all"] == true {
		t.Errorf("workers_list while acting as = %+v", a)
	}
}

// -- the panel fragment ------------------------------------------------------

func TestPanelFragmentIsOnlyThePanel(t *testing.T) {
	s, _ := sessionOps(t)
	rec := sessGet(t, s, "/s/pi-1/minion-1/screen", sessUser)
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, `<div id="screen-panel">`) || strings.Contains(body, "<html") || strings.Contains(body, `id="session-view"`) {
		t.Errorf("fragment is not the bare panel:\n%s", body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	mustHave(t, body, `&lt;script&gt;`)
	mustNotHave(t, body, "<script")
}

func TestPanelShowsTheAnswerFormForAnOperatorWhileADialogIsOpen(t *testing.T) {
	s, f := sessionOps(t)
	f.reply(ops.OpSessionSnapshot, ops.SessionSnapshotResult{
		Pane: dialogPane(), Screen: hostileScreen()})
	body := sessGet(t, s, "/s/pi-1/minion-1/screen", sessUser).Body.String()
	mustHave(t, body, `hx-post="/s/pi-1/minion-1/answer"`, `name="choice" value="1"`, `name="choice" value="y"`, `name="choice_text"`)
	// Both forms carry the tokens of the snapshot that rendered the panel.
	for _, form := range reForm.FindAllString(body, -1) {
		if !strings.Contains(form, `/answer"`) {
			continue
		}
		mustHave(t, form, `name="expect_pane_id" value="%1"`, `name="expect_pid" value="4242"`, `name="expect_command" value="claude"`,
			`name="expect_session_created" value="1794308400"`, `name="expect_dialog_digest" value="`+strings.Repeat("ab", 32)+`"`)
	}
	// No dialog, no form.
	f.reply(ops.OpSessionSnapshot, ops.SessionSnapshotResult{Pane: ops.PaneInfo{State: "stable"}, Screen: hostileScreen()})
	mustNotHave(t, sessGet(t, s, "/s/pi-1/minion-1/screen", sessUser).Body.String(), `class="answer"`)
}

// Native Enter in the answer box submits its form with the form's FIRST submit
// button. That button must be the one plain "Send answer" button, never a button
// that carries an option (choice=1 would be sent instead of the typed text). The
// options are radio buttons: choosing one is not sending it, and a separate
// button sends.
func TestTypedAnswerFormDefaultsToItsOwnAnswerButton(t *testing.T) {
	s, f := sessionOps(t)
	f.reply(ops.OpSessionSnapshot, ops.SessionSnapshotResult{
		Pane: dialogPane(), Screen: hostileScreen()})
	reButton := regexp.MustCompile(`<button[^>]*>`)
	reInput := regexp.MustCompile(`<input[^>]*>`)
	for _, path := range []string{"/s/pi-1/minion-1/screen", "/s/pi-1/minion-1"} {
		body := sessGet(t, s, path, sessUser).Body.String()
		answers := 0
		for _, form := range reForm.FindAllString(body, -1) {
			if !strings.Contains(form, `name="choice_text"`) {
				continue
			}
			answers++
			buttons := reButton.FindAllString(form, -1)
			if len(buttons) != 1 || strings.Contains(buttons[0], `name=`) || strings.Contains(buttons[0], `value=`) || !strings.Contains(form, ">Send answer</button>") {
				t.Errorf("%s: the answer form must have one plain Send answer button, has %v", path, buttons)
			}
			radios := 0
			for _, in := range reInput.FindAllString(form, -1) {
				if strings.Contains(in, `name="choice"`) {
					radios++
					if !strings.Contains(in, `type="radio"`) || strings.Contains(in, "checked") {
						t.Errorf("%s: an option must be an unchecked radio button, not a way to send: %s", path, in)
					}
				}
			}
			if radios != len(answerChoices) {
				t.Errorf("%s: %d options, want %d", path, radios, len(answerChoices))
			}
		}
		if answers != 1 {
			t.Errorf("%s: %d answer forms, want one", path, answers)
		}
	}
	// What the browser posts for Enter in the box: the text box only.
	post(t, s, "/s/pi-1/minion-1/answer", sessUser, dialogForm(url.Values{"choice_text": {"n"}}))
	if a := argsOf(f, ops.OpAnswer, 0); a["choice"] != "n" {
		t.Errorf("answer = %+v, want the typed n", a)
	}
}

// A chosen option and a typed answer that differ are refused before anything is
// sent: the page cannot tell which one the person meant.
func TestAnswerWithAnOptionAndADifferentTypedTextCallsNothing(t *testing.T) {
	s, f := sessionOps(t)
	rec := post(t, s, "/s/pi-1/minion-1/answer", sessUser, dialogForm(url.Values{"choice": {"1"}, "choice_text": {"no"}}))
	mustHave(t, rec.Body.String(), "not both", "Nothing was sent")
	if n := len(f.called(ops.OpAnswer)); n != 0 {
		t.Errorf("%d answer calls, want none", n)
	}
	// The same answer in both fields is one answer.
	post(t, s, "/s/pi-1/minion-1/answer", sessUser, dialogForm(url.Values{"choice": {"y"}, "choice_text": {"y"}}))
	if a := argsOf(f, ops.OpAnswer, 0); a["choice"] != "y" {
		t.Errorf("answer = %+v, want y", a)
	}
}

// A Worker that does not report the identity of the dialog cannot be answered
// safely: the panel says so and offers no answer form.
func TestPanelOffersNoAnswerFormWhenTheWorkerReportsNoDialogIdentity(t *testing.T) {
	s, f := sessionOps(t)
	old := dialogPane()
	old.DialogDigest, old.PID, old.SessionCreated = "", 0, 0
	f.reply(ops.OpSessionSnapshot, ops.SessionSnapshotResult{Pane: old, Screen: hostileScreen()})
	body := sessGet(t, s, "/s/pi-1/minion-1/screen", sessUser).Body.String()
	mustHave(t, body, "does not report the identity of the dialog")
	mustNotHave(t, body, "session-answer", `name="choice_text"`, `name="choice" value=`)
}

// The script tells an offline page (no screen panel to swap) from an online one.
func TestSessionViewTellsTheScriptWhetherTheWorkerIsOnline(t *testing.T) {
	s, f := sessionOps(t)
	mustHave(t, sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String(), `data-online="true"`)
	w := sessionWorker()
	w.Online = false
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w}})
	mustHave(t, sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String(), `data-online="false"`)
}

// -- writes ------------------------------------------------------------------

func argsOf(f *fakeOps, op string, i int) map[string]any {
	c := f.called(op)
	if i >= len(c) {
		return nil
	}
	return c[i].Args
}

func TestSendTextCallsSendTextWithAFreshRequestID(t *testing.T) {
	s, f := sessionOps(t)
	ids := map[string]bool{}
	for i := 0; i < 3; i++ {
		rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"echo <hi>"}})
		if rec.Code != 200 {
			t.Fatalf("POST = %d", rec.Code)
		}
		mustHave(t, rec.Body.String(), `class="result result-info"`, "<strong>Text:</strong> Delivered.")
		a := argsOf(f, ops.OpSendText, i)
		id, _ := a["request_id"].(string)
		if !ops.ValidUUIDv7(id) || ids[id] {
			t.Errorf("call %d request_id %q is not a fresh UUIDv7", i, id)
		}
		ids[id] = true
		tgt, _ := a["target"].(map[string]any)
		if a["text"] != "echo <hi>" || a["force"] != false || tgt["worker_id"] != sessWorkerID || tgt["session_name"] != "minion-1" {
			t.Errorf("args = %+v", a)
		}
	}
	// No Enter was asked for: no key was sent.
	if len(f.called(ops.OpSendKeys)) != 0 {
		t.Error("send_keys was called without enter=1")
	}
}

func TestSendTextEventsTellTheBrowserToRefreshAndClearTheBox(t *testing.T) {
	s, _ := sessionOps(t)
	rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"x"}})
	h := rec.Header().Get("HX-Trigger")
	mustHave(t, h, `"minion-screen-refresh":true`, `"minion-input-sent":true`)
	// A key press refreshes the screen but keeps the text box.
	rec = post(t, s, "/s/pi-1/minion-1/keys", sessUser, url.Values{"key": {"Up"}})
	h = rec.Header().Get("HX-Trigger")
	mustHave(t, h, `"minion-screen-refresh":true`)
	mustNotHave(t, h, "minion-input-sent")
}

// The text and its Enter are ONE call: one request id, submit=true, no second
// send_keys that could reach another pane or follow another writer's text.
func TestSendTextWithEnterIsOneAtomicSubmit(t *testing.T) {
	s, f := sessionOps(t)
	rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"ls"}, "enter": {"1"}})
	mustHave(t, rec.Body.String(), "<strong>Text:</strong> Delivered.")
	mustNotHave(t, rec.Body.String(), "Key Enter")
	if n := len(f.called(ops.OpSendKeys)); n != 0 {
		t.Fatalf("send_keys was called %d times; the Enter belongs to the send_text call", n)
	}
	if n := len(f.called(ops.OpSendText)); n != 1 {
		t.Fatalf("send_text was called %d times, want 1", n)
	}
	ta := argsOf(f, ops.OpSendText, 0)
	if ta["submit"] != true || ta["text"] != "ls" || !ops.ValidUUIDv7(ta["request_id"].(string)) {
		t.Errorf("args = %+v", ta)
	}
	// Without the checkbox nothing is submitted.
	post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"ls"}})
	if tb := argsOf(f, ops.OpSendText, 1); tb["submit"] == true {
		t.Errorf("submit without enter=1: %+v", tb)
	}
}

// A browser posts line breaks as CRLF; the Worker refuses CR, so the page sends LF.
func TestSendTextNormalisesLineBreaks(t *testing.T) {
	s, f := sessionOps(t)
	post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"one\r\ntwo\rthree\nfour"}})
	if a := argsOf(f, ops.OpSendText, 0); a["text"] != "one\ntwo\nthree\nfour" {
		t.Errorf("text = %q", a["text"])
	}
}

// After the text went out and the Enter did not (partial) or its outcome is
// unknown, the page says so and offers no "send anyway": repeating would paste
// the text again.
func TestPartialSubmitIsShownAndNeverOffersAResend(t *testing.T) {
	for _, tc := range []struct {
		res     ops.WriteResult
		summary string
	}{
		{ops.WriteResult{Outcome: ops.OutcomePartial, Reason: "pane_changed"}, "Partly sent"},
		{ops.WriteResult{Outcome: ops.OutcomeUnknown, Reason: "internal"}, "Delivery is unknown"},
	} {
		s, f := sessionOps(t)
		f.reply(ops.OpSendText, tc.res)
		rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"ls"}, "enter": {"1"}})
		mustHave(t, rec.Body.String(), tc.summary, "result-warning")
		mustNotHave(t, rec.Body.String(), "Send anyway", `name="force"`)
		mustNotHave(t, rec.Header().Get("HX-Trigger"), "minion-input-sent")
		if len(f.called(ops.OpSendKeys)) != 0 {
			t.Error("a key was sent after a partial submit")
		}
	}
}

func TestSendTextRefusedDoesNotSendEnterAndClearsNothing(t *testing.T) {
	s, f := sessionOps(t)
	f.reply(ops.OpSendText, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "worker_busy", Forceable: true})
	rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"ls"}, "enter": {"1"}})
	if len(f.called(ops.OpSendKeys)) != 0 {
		t.Error("Enter was sent after a refused text")
	}
	mustNotHave(t, rec.Header().Get("HX-Trigger"), "minion-input-sent")
}

// AC: a FORCE-class refusal shows the reason and a "send anyway" button that
// repeats the same call with force=true and the same text.
func TestForceFlowRepeatsTheSameCallWithForce(t *testing.T) {
	s, f := sessionOps(t)
	f.on(ops.OpSendText, func(a map[string]any) (any, error) {
		if a["force"] == true {
			return ops.WriteResult{Outcome: ops.OutcomeDelivered, Forced: true,
				Warnings: []ops.Warning{{Code: "pane_busy", Message: "The pane was busy."}}}, nil
		}
		return ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "pane_busy", Forceable: true,
			Warnings: []ops.Warning{{Code: "w", Message: "<b>careful</b>"}}}, nil
	})
	text := `rm -rf "/tmp/<x>" & more`
	rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {text}})
	body := rec.Body.String()
	mustHave(t, body, `class="result result-danger"`, "Not sent", "<code>pane_busy</code>",
		`<button type="submit" class="btn-danger">Send anyway</button>`, `action="/s/pi-1/minion-1/text"`,
		`name="force" value="1"`, "Warning: &lt;b&gt;careful&lt;/b&gt;")
	mustNotHave(t, body, "<b>careful</b>")

	// The hidden fields are exactly the original text (escaped) and force=1:
	// post what the browser would post for that form.
	form := formOf(t, body)
	if form.Get("text") != text || form.Get("force") != "1" {
		t.Fatalf("force form = %v", form)
	}
	rec = post(t, s, "/s/pi-1/minion-1/text", sessUser, form)
	mustHave(t, rec.Body.String(), "Delivered (sent anyway).", "Warning: The pane was busy.", `result result-warning`)
	mustNotHave(t, rec.Body.String(), "Send anyway")

	first, second := argsOf(f, ops.OpSendText, 0), argsOf(f, ops.OpSendText, 1)
	if first["text"] != text || second["text"] != text || first["force"] != false || second["force"] != true {
		t.Errorf("calls = %+v then %+v", first, second)
	}
	if first["request_id"] == second["request_id"] || !ops.ValidUUIDv7(second["request_id"].(string)) {
		t.Errorf("the forced call reuses a request_id: %v %v", first["request_id"], second["request_id"])
	}
}

// A refused paste-and-submit is repeated as the SAME single call (text and
// enter, force=1, a new request id), never as a text call plus a key call.
func TestForceFlowOfASubmitRepeatsOneCall(t *testing.T) {
	s, f := sessionOps(t)
	f.on(ops.OpSendText, func(a map[string]any) (any, error) {
		if a["force"] == true {
			return ops.WriteResult{Outcome: ops.OutcomeDelivered, Forced: true}, nil
		}
		return ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "shell_pane", Forceable: true}, nil
	})
	rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"ls"}, "enter": {"1"}})
	body := rec.Body.String()
	mustHave(t, body, "<strong>Text:</strong> Not sent", `action="/s/pi-1/minion-1/text"`)
	mustNotHave(t, body, `/keys"`)
	form := formOf(t, body)
	if form.Get("text") != "ls" || form.Get("enter") != "1" || form.Get("force") != "1" || form.Get("key") != "" {
		t.Fatalf("force form = %v", form)
	}
	post(t, s, "/s/pi-1/minion-1/text", sessUser, form)
	a0, a1 := argsOf(f, ops.OpSendText, 0), argsOf(f, ops.OpSendText, 1)
	if a0["submit"] != true || a1["submit"] != true || a0["force"] != false || a1["force"] != true || a0["request_id"] == a1["request_id"] {
		t.Errorf("calls %+v then %+v", a0, a1)
	}
	if len(f.called(ops.OpSendKeys)) != 0 {
		t.Error("send_keys was called")
	}
}

func TestForceFlowOfAKeyTaskAndAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		form       url.Values
		op         string
		check      func(a map[string]any) bool
	}{
		{"key", "/s/pi-1/minion-1/keys", url.Values{"key": {"C-c"}}, ops.OpSendKeys,
			func(a map[string]any) bool { k, _ := a["keys"].([]any); return len(k) == 1 && k[0] == "C-c" }},
		{"task", "/s/pi-1/minion-1/task", url.Values{"task": {"write docs"}}, ops.OpSetTask,
			func(a map[string]any) bool { return a["task"] == "write docs" }},
	} {
		s, f := sessionOps(t)
		f.on(tc.op, func(a map[string]any) (any, error) {
			if a["force"] == true {
				return delivered, nil
			}
			return ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "pane_busy", Forceable: true}, nil
		})
		rec := post(t, s, tc.path, sessUser, tc.form)
		form := formOf(t, rec.Body.String())
		if form.Get("force") != "1" {
			t.Errorf("%s: no force form in\n%s", tc.name, rec.Body)
			continue
		}
		post(t, s, tc.path, sessUser, form)
		a0, a1 := argsOf(f, tc.op, 0), argsOf(f, tc.op, 1)
		if a0 == nil || a1 == nil || a0["force"] != false || a1["force"] != true || !tc.check(a0) || !tc.check(a1) || a0["request_id"] == a1["request_id"] {
			t.Errorf("%s: calls %+v then %+v", tc.name, a0, a1)
		}
	}
}

func TestRefusalThatIsNotForceableOffersNoSendAnyway(t *testing.T) {
	s, f := sessionOps(t)
	f.reply(ops.OpSendText, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "not_shared"})
	rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"x"}})
	mustHave(t, rec.Body.String(), "<code>not_shared</code>", "Not sent")
	mustNotHave(t, rec.Body.String(), "Send anyway", `name="force"`)
}

func TestOutcomesAreSpelledOut(t *testing.T) {
	s, f := sessionOps(t)
	for _, tc := range []struct {
		res  ops.WriteResult
		want []string
	}{
		{ops.WriteResult{Outcome: ops.OutcomePartial, Reason: "x"}, []string{"result-warning", "Partly sent", "Check the screen"}},
		{ops.WriteResult{Outcome: ops.OutcomeUnknown, Reason: "timeout"}, []string{"result-warning", "Delivery is unknown", "before deciding whether to send again"}},
		{ops.WriteResult{Outcome: ops.OutcomeDelivered, Warnings: []ops.Warning{{Code: "w", Message: "heads up"}}}, []string{"result-warning", "Warning: heads up"}},
	} {
		f.reply(ops.OpSendText, tc.res)
		mustHave(t, post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"x"}}).Body.String(), tc.want...)
	}
}

func TestAPIRefusalAndFailureAreShown(t *testing.T) {
	s, f := sessionOps(t)
	f.on(ops.OpSendText, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 503, Code: "worker_offline", Message: "the Worker is offline"}
	})
	rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"x"}})
	mustHave(t, rec.Body.String(), "result-danger", "the Worker is offline")
	mustNotHave(t, rec.Header().Get("HX-Trigger"), "minion-input-sent")
	f.on(ops.OpSendText, func(map[string]any) (any, error) { return nil, http.ErrAbortHandler })
	rec = post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {"x"}})
	mustHave(t, rec.Body.String(), "The Hub could not complete the request.")
	mustNotHave(t, rec.Body.String(), "ErrAbortHandler")
}

func TestPostToAWorkerTheUserCannotSeeShowsAnErrorAndCallsNoWrite(t *testing.T) {
	s, f := sessionOps(t)
	rec := post(t, s, "/s/nope/minion-1/text", sessUser, url.Values{"text": {"x"}})
	mustHave(t, rec.Body.String(), "result-danger", "That page does not exist.")
	if len(f.called(ops.OpSendText)) != 0 {
		t.Error("send_text was called for an unknown Worker")
	}
}

func TestEmptyInputsCallNothing(t *testing.T) {
	s, f := sessionOps(t)
	for path, form := range map[string]url.Values{
		"/s/pi-1/minion-1/text":   {"text": {""}},
		"/s/pi-1/minion-1/keys":   {"key": {"F13"}},
		"/s/pi-1/minion-1/answer": {"choice": {"  "}, "choice_text": {""}},
	} {
		rec := post(t, s, path, sessUser, form)
		mustHave(t, rec.Body.String(), "result-danger")
	}
	for _, op := range []string{ops.OpSendText, ops.OpSendKeys, ops.OpAnswer} {
		if n := len(f.called(op)); n != 0 {
			t.Errorf("%s was called %d times for an empty or invalid input", op, n)
		}
	}
}

func TestEveryKeyButtonIsSentAsItsTmuxName(t *testing.T) {
	want := map[string]string{
		"Enter": "Enter", "Esc": "Escape", "Ctrl-C": "C-c", "Tab": "Tab",
		"Shift-Tab": "BTab", "Space": "Space", "Backspace": "BSpace", "Delete": "DC",
		"Up": "Up", "Down": "Down", "Left": "Left", "Right": "Right",
	}
	if len(sessionKeys) != len(want) {
		t.Fatalf("keys = %+v", sessionKeys)
	}
	s, f := sessionOps(t)
	for i, k := range sessionKeys {
		if want[k.Name] != k.Key {
			t.Errorf("button %q sends %q, want %q", k.Name, k.Key, want[k.Name])
		}
		rec := post(t, s, "/s/pi-1/minion-1/keys", sessUser, url.Values{"key": {k.Key}})
		mustHave(t, rec.Body.String(), "<strong>Key "+k.Name+":</strong> Delivered.")
		keys, _ := argsOf(f, ops.OpSendKeys, i)["keys"].([]any)
		if len(keys) != 1 || keys[0] != k.Key {
			t.Errorf("button %q: keys = %v", k.Label, keys)
		}
	}
}

func TestSetTaskTrimsAndCanClear(t *testing.T) {
	s, f := sessionOps(t)
	rec := post(t, s, "/s/pi-1/minion-1/task", sessUser, url.Values{"task": {"  ship it \n"}})
	mustHave(t, rec.Body.String(), "<strong>Task:</strong> Saved.")
	post(t, s, "/s/pi-1/minion-1/task", sessUser, url.Values{"task": {" "}})
	if argsOf(f, ops.OpSetTask, 0)["task"] != "ship it" || argsOf(f, ops.OpSetTask, 1)["task"] != "" {
		t.Errorf("tasks = %+v %+v", argsOf(f, ops.OpSetTask, 0), argsOf(f, ops.OpSetTask, 1))
	}
}

func TestAnswerAcceptsAButtonOrTheTextBox(t *testing.T) {
	s, f := sessionOps(t)
	post(t, s, "/s/pi-1/minion-1/answer", sessUser, dialogForm(url.Values{"choice": {"y"}}))
	post(t, s, "/s/pi-1/minion-1/answer", sessUser, dialogForm(url.Values{"choice_text": {" Yes, always "}}))
	if argsOf(f, ops.OpAnswer, 0)["choice"] != "y" || argsOf(f, ops.OpAnswer, 1)["choice"] != "Yes, always" {
		t.Errorf("answers = %+v %+v", argsOf(f, ops.OpAnswer, 0), argsOf(f, ops.OpAnswer, 1))
	}
}

// The answer carries, as "expect", exactly the dialog the panel showed: the
// tokens of the form, not the current state of the pane.
func TestAnswerCarriesTheReviewedDialogOfTheForm(t *testing.T) {
	s, f := sessionOps(t)
	post(t, s, "/s/pi-1/minion-1/answer", sessUser, dialogForm(url.Values{"choice": {"1"}}))
	exp, _ := argsOf(f, ops.OpAnswer, 0)["expect"].(map[string]any)
	if exp["pane_id"] != "%1" || exp["pid"] != float64(4242) || exp["command"] != "claude" ||
		exp["session_created"] != float64(1794308400) || exp["dialog_digest"] != strings.Repeat("ab", 32) {
		t.Errorf("expect = %+v", exp)
	}
	if len(f.called(ops.OpSessionSnapshot)) != 0 {
		t.Error("the answer route read a fresh snapshot: the page must not refresh the reviewed dialog itself")
	}
}

// An answer whose tokens are missing or malformed makes no call: the page never
// invents the dialog from the current screen.
func TestAnswerWithoutTheReviewedDialogCallsNothing(t *testing.T) {
	for name, drop := range map[string]string{
		"pane": fieldExpectPaneID, "pid": fieldExpectPID, "command": fieldExpectCommand, "created": fieldExpectCreated, "digest": fieldExpectDigest,
	} {
		s, f := sessionOps(t)
		form := dialogForm(url.Values{"choice": {"1"}})
		form.Del(drop)
		rec := post(t, s, "/s/pi-1/minion-1/answer", sessUser, form)
		mustHave(t, rec.Body.String(), "Nothing was sent", "result-danger")
		mustHave(t, rec.Header().Get("HX-Trigger"), "minion-dialog-stale")
		if n := len(f.called(ops.OpAnswer)) + len(f.called(ops.OpSessionSnapshot)); n != 0 {
			t.Errorf("%s: the answer reached the operations without the reviewed dialog: %+v", name, f.calls)
		}
	}
	s, f := sessionOps(t)
	bad := dialogForm(url.Values{"choice": {"1"}, fieldExpectPID: {"abc"}})
	post(t, s, "/s/pi-1/minion-1/answer", sessUser, bad)
	post(t, s, "/s/pi-1/minion-1/answer", sessUser, url.Values{"choice": {"1"}})
	if n := len(f.called(ops.OpAnswer)) + len(f.called(ops.OpSessionSnapshot)); n != 0 {
		t.Errorf("a malformed or missing token reached the operations: %+v", f.calls)
	}
}

// The Worker refuses an answer for a dialog that is gone. The page says so,
// offers no "send anyway" and no automatic retry, and tells the browser to drop
// the typed answer and show the current screen.
func TestStaleAnswerIsRefusedWithoutResendAndRefreshesTheScreen(t *testing.T) {
	for _, reason := range []string{"dialog_changed", "pane_changed"} {
		s, f := sessionOps(t)
		f.reply(ops.OpAnswer, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: reason})
		rec := post(t, s, "/s/pi-1/minion-1/answer", sessUser, dialogForm(url.Values{"choice_text": {"y"}, "force": {"1"}}))
		body := rec.Body.String()
		mustHave(t, body, "The dialog changed since you looked at it. Nothing was sent.", "<code>"+reason+"</code>", "result-danger")
		mustNotHave(t, body, "Send anyway")
		h := rec.Header().Get("HX-Trigger")
		mustHave(t, h, `"minion-screen-refresh":true`, `"minion-dialog-stale":true`)
		mustNotHave(t, h, "minion-input-sent")
		if n := len(f.called(ops.OpAnswer)); n != 1 {
			t.Errorf("%s: answer was called %d times; the page must not retry with a fresh dialog", reason, n)
		}
		if len(f.called(ops.OpSessionSnapshot)) != 0 {
			t.Errorf("%s: the answer route took a new snapshot", reason)
		}
	}
}

// The browser releases a typed answer only when the response says that it was
// delivered: minion-input-sent comes with a delivered answer, never with a refusal.
func TestAnswerEventsSayWhetherTheTypedAnswerWasDelivered(t *testing.T) {
	s, f := sessionOps(t)
	rec := post(t, s, "/s/pi-1/minion-1/answer", sessUser, dialogForm(url.Values{"choice_text": {"n"}}))
	mustHave(t, rec.Header().Get("HX-Trigger"), `"minion-screen-refresh":true`, `"minion-input-sent":true`)
	mustNotHave(t, rec.Header().Get("HX-Trigger"), "minion-dialog-stale")
	f.reply(ops.OpAnswer, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "pane_busy", Forceable: true})
	rec = post(t, s, "/s/pi-1/minion-1/answer", sessUser, dialogForm(url.Values{"choice_text": {"n"}}))
	mustNotHave(t, rec.Header().Get("HX-Trigger"), "minion-input-sent")
	// The binding is KEEP and the Worker's answer has no FORCE guard: no resend button.
	mustNotHave(t, rec.Body.String(), "Send anyway")
	f.on(ops.OpAnswer, func(map[string]any) (any, error) { return nil, &OpError{Status: 403, Code: "forbidden", Message: "no"} })
	rec = post(t, s, "/s/pi-1/minion-1/answer", sessUser, dialogForm(url.Values{"choice_text": {"n"}}))
	mustNotHave(t, rec.Header().Get("HX-Trigger"), "minion-input-sent")
}

func TestWriteRouteBoundsTheBody(t *testing.T) {
	s, f := sessionOps(t)
	rec := post(t, s, "/s/pi-1/minion-1/text", sessUser, url.Values{"text": {strings.Repeat("a", maxSessionFormBytes+1)}})
	mustHave(t, rec.Body.String(), "The form could not be read.")
	if f.total() != 0 {
		t.Errorf("an oversized form reached the operations: %+v", f.calls)
	}
}

// -- new session -------------------------------------------------------------

// AC: "new session" runs session_create and the new minion-N session opens.
func TestNewSessionCreatesAndOpensTheSession(t *testing.T) {
	s, f := sessionOps(t)
	f.reply(ops.OpSessionCreate, ops.WriteResult{Outcome: ops.OutcomeDelivered, Pane: &ops.PaneInfo{Session: "minion-7", ID: "%9"}})
	rec := post(t, s, "/w/pi-1/sessions", sessUser, url.Values{})
	if rec.Header().Get("HX-Redirect") != "/s/pi-1/minion-7" {
		t.Errorf("HX-Redirect = %q (status %d)\n%s", rec.Header().Get("HX-Redirect"), rec.Code, rec.Body)
	}
	a := argsOf(f, ops.OpSessionCreate, 0)
	if a["worker_id"] != sessWorkerID || !ops.ValidUUIDv7(a["request_id"].(string)) || a["force"] != false {
		t.Errorf("session_create args = %+v", a)
	}
	// Without htmx (a plain form post) it is a 303.
	req := httptest.NewRequest(http.MethodPost, "/w/pi-1/sessions", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Test-Session", sessUser)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/s/pi-1/minion-7" {
		t.Errorf("plain post = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if ids := []any{argsOf(f, ops.OpSessionCreate, 0)["request_id"], argsOf(f, ops.OpSessionCreate, 1)["request_id"]}; ids[0] == ids[1] {
		t.Error("two creates share a request_id")
	}
}

func TestNewSessionEscapesTheRedirectAndKeepsTheWorkerReference(t *testing.T) {
	s, f := sessionOps(t)
	f.reply(ops.OpSessionCreate, ops.WriteResult{Outcome: ops.OutcomeDelivered, Pane: &ops.PaneInfo{Session: "a b/c"}})
	rec := post(t, s, "/w/"+sessWorkerID+"/sessions", sessUser, url.Values{})
	if got := rec.Header().Get("HX-Redirect"); got != "/s/"+sessWorkerID+"/a%20b%2Fc" {
		t.Errorf("HX-Redirect = %q", got)
	}
}

func TestNewSessionFailuresShowTheResultAndDoNotRedirect(t *testing.T) {
	s, f := sessionOps(t)
	for name, v := range map[string]any{
		"refused":  ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "tmux_unavailable"},
		"unnamed":  ops.WriteResult{Outcome: ops.OutcomeDelivered},
		"unknown":  ops.WriteResult{Outcome: ops.OutcomeUnknown, Reason: "timeout"},
		"no pane":  ops.WriteResult{Outcome: ops.OutcomeDelivered, Pane: &ops.PaneInfo{}},
		"too soon": ops.WriteResult{Outcome: ops.OutcomePartial},
	} {
		f.reply(ops.OpSessionCreate, v)
		rec := post(t, s, "/w/pi-1/sessions", sessUser, url.Values{})
		if rec.Header().Get("HX-Redirect") != "" || !strings.Contains(rec.Body.String(), "<strong>New session:</strong>") {
			t.Errorf("%s: %q\n%s", name, rec.Header().Get("HX-Redirect"), rec.Body)
		}
	}
	f.on(ops.OpSessionCreate, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 503, Code: "worker_offline", Message: "the Worker is offline"}
	})
	mustHave(t, post(t, s, "/w/pi-1/sessions", sessUser, url.Values{}).Body.String(), "the Worker is offline")
}

// -- the script --------------------------------------------------------------

// The live-refresh script runs against a DOM stub: bursts of events make one
// fetch, other sessions are ignored, colours are defined once, a bad class
// never becomes a rule.
func TestTerminalInputScriptLiveRefresh(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script, _ := filepath.Abs(filepath.Join("testdata", "session_live_test.js"))
	src, _ := filepath.Abs(filepath.Join("static", "terminal-input.js"))
	out, err := exec.Command(node, script, src).CombinedOutput()
	if err != nil {
		t.Fatalf("terminal-input.js: %v\n%s", err, out)
	}
}

// -- helpers -----------------------------------------------------------------

var (
	reForm   = regexp.MustCompile(`(?s)<form[^>]*>.*?</form>`)
	reHidden = regexp.MustCompile(`<input type="hidden" name="([^"]*)" value="([^"]*)">`)
)

// formOf reads the "send anyway" form of a result fragment the way a browser
// would: the hidden inputs (HTML-unescaped) become the posted values.
func formOf(t *testing.T, body string) url.Values {
	t.Helper()
	v := url.Values{}
	for _, f := range reForm.FindAllString(body, -1) {
		if !strings.Contains(f, "Send anyway") {
			continue
		}
		for _, m := range reHidden.FindAllStringSubmatch(f, -1) {
			v.Add(unescapeAttr(m[1]), unescapeAttr(m[2]))
		}
		return v
	}
	return v
}

func unescapeAttr(s string) string {
	return strings.NewReplacer("&#34;", `"`, "&#39;", "'", "&lt;", "<", "&gt;", ">", "&amp;", "&", "&#43;", "+", "&#x2b;", "+").Replace(s)
}

// A write the Worker reports as unknown or partial tells the browser with an
// HX-Trigger event, so that the page keeps the submitted payload outside the
// replaceable result area until the operator dismisses it. Every other outcome
// carries no such event, and the trigger holds codes only (never the payload).
func TestWorkerUnknownAndPartialOutcomesAreSignalledToTheBrowser(t *testing.T) {
	unknown := func(res ops.WriteResult, op, path string, form url.Values) map[string]any {
		t.Helper()
		s, f := sessionOps(t)
		f.reply(op, res)
		rec := post(t, s, path, sessUser, form)
		var trig map[string]any
		if h := rec.Header().Get("HX-Trigger"); h != "" {
			if err := json.Unmarshal([]byte(h), &trig); err != nil {
				t.Fatalf("HX-Trigger %q is not JSON: %v", h, err)
			}
		}
		if _, ok := trig["minion-screen-refresh"]; !ok {
			t.Errorf("%s lost the screen refresh: %v", path, trig)
		}
		return trig
	}
	for _, tc := range []struct {
		op, path string
		form     url.Values
	}{
		{ops.OpSendText, "/s/pi-1/minion-1/text", url.Values{"text": {"secret-token-123"}}},
		{ops.OpSendKeys, "/s/pi-1/minion-1/keys", url.Values{"key": {"C-c"}}},
		{ops.OpAnswer, "/s/pi-1/minion-1/answer", dialogForm(url.Values{"choice_text": {"secret-answer-456"}})},
		// A task save sends no terminal input, but an unknown outcome still may have changed the label.
		{ops.OpSetTask, "/s/pi-1/minion-1/task", url.Values{"task": {"secret-task-789"}}},
	} {
		for _, o := range []struct {
			outcome ops.Outcome
			want    string
		}{{ops.OutcomeUnknown, "unknown"}, {ops.OutcomePartial, "partial"}} {
			trig := unknown(ops.WriteResult{Outcome: o.outcome, Reason: "timeout"}, tc.op, tc.path, tc.form)
			d, ok := trig["minion-input-unknown"].(map[string]any)
			if !ok || d["outcome"] != o.want || d["reason"] != "timeout" {
				t.Errorf("%s %s: minion-input-unknown = %v", tc.path, o.outcome, trig["minion-input-unknown"])
			}
			if _, sent := trig["minion-input-sent"]; sent {
				t.Errorf("%s %s: reported as sent", tc.path, o.outcome)
			}
			b, _ := json.Marshal(trig)
			if strings.Contains(string(b), "secret-") {
				t.Errorf("%s: the trigger carries the payload: %s", tc.path, b)
			}
		}
		for _, res := range []ops.WriteResult{{Outcome: ops.OutcomeDelivered}, {Outcome: ops.OutcomeFailedNotSent, Reason: "not_shared"}} {
			if trig := unknown(res, tc.op, tc.path, tc.form); trig["minion-input-unknown"] != nil {
				t.Errorf("%s %s: signalled as unknown: %v", tc.path, res.Outcome, trig)
			}
		}
	}
}
