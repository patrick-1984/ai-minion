package server

import (
	"context"
	"encoding/json"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// sessionWorker is a test Worker on the far end of an in-process connection: it
// records every op request it gets and answers the ops of the session view.
type sessionWorker struct {
	mu   sync.Mutex
	reqs []workers.OpRequest
}

func (w *sessionWorker) calls(op string) []map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []map[string]any
	for _, r := range w.reqs {
		if r.Op != op {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(r.Args, &m)
		out = append(out, m)
	}
	return out
}

func (w *sessionWorker) total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.reqs)
}

func (w *sessionWorker) serve(ws *transport.Session) {
	for {
		st, err := ws.Accept(context.Background())
		if err != nil {
			return
		}
		go func() {
			defer st.Close()
			body, err := st.ReadFrame()
			if err != nil {
				return
			}
			var req workers.OpRequest
			if json.Unmarshal(body, &req) != nil {
				return
			}
			w.mu.Lock()
			w.reqs = append(w.reqs, req)
			w.mu.Unlock()
			var args map[string]any
			_ = json.Unmarshal(req.Args, &args)
			reply := func(v any) { _ = st.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": v}) }
			switch req.Op {
			case ops.OpSessionSnapshot:
				reply(ops.SessionSnapshotResult{
					Pane: fixtureDialogPane(),
					Screen: ops.Screen{Width: 40, Height: 2,
						Lines:  []string{`<img src=x onerror=alert(1)> done`, `ok`},
						Spans:  []ops.StyleSpan{{Line: 0, Start: 0, End: 5, FG: "p196", Bold: true}},
						Cursor: &ops.Cursor{Row: 1, Col: 2}},
				})
			case ops.OpSessionsList:
				reply(ops.SessionsListResult{Sessions: []ops.SessionInfo{{Name: "minion-1", Task: "write the docs"}}})
			case ops.OpSendText:
				if args["force"] != true && args["text"] == "busy text" {
					reply(ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "pane_busy", Forceable: true})
					return
				}
				reply(ops.WriteResult{Outcome: ops.OutcomeDelivered, Forced: args["force"] == true})
			case ops.OpSessionCreate:
				reply(ops.WriteResult{Outcome: ops.OutcomeDelivered, Pane: &ops.PaneInfo{Session: "minion-5", ID: "%7"}})
			case ops.OpAnswer:
				// The Worker compares the reviewed dialog (expect) with the
				// pane it has now and refuses a different one; force never
				// overrides that (EPICS A-22). A request without expect is
				// not valid on the wire either.
				var a ops.AnswerArgs
				if json.Unmarshal(req.Args, &a) != nil || a.Expect.Validate() != nil {
					_ = st.WriteFrame(proto.NewError(proto.CodeInvalidArgument, "expect is required").Frame())
					return
				}
				now := reviewedOfFixture(fixtureDialogPane())
				switch {
				case a.Expect.PaneID != now.PaneID || a.Expect.PID != now.PID || a.Expect.Command != now.Command || a.Expect.SessionCreated != now.SessionCreated:
					reply(ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "pane_changed"})
				case a.Expect.DialogDigest != now.DialogDigest:
					reply(ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "dialog_changed"})
				default:
					reply(ops.WriteResult{Outcome: ops.OutcomeDelivered})
				}
			default: // send_keys, set_task
				reply(ops.WriteResult{Outcome: ops.OutcomeDelivered})
			}
		}()
	}
}

// fixtureDialogPane is the pane a current Worker reports for a session that
// shows a dialog: the snapshot carries the process, the session generation and
// the dialog digest that the answer form binds the answer to (EPICS A-22).
func fixtureDialogPane() ops.PaneInfo {
	return ops.PaneInfo{
		Session: "minion-1", ID: "%1", Kind: "agent:claude", State: ops.PaneStateDialog,
		Command: "claude", PID: 4242, SessionCreated: 1794308400, DialogDigest: strings.Repeat("ab", 32),
	}
}

func reviewedOfFixture(p ops.PaneInfo) ops.ReviewedDialog {
	return ops.ReviewedDialog{PaneID: p.ID, PID: p.PID, Command: p.Command, SessionCreated: p.SessionCreated, DialogDigest: p.DialogDigest}
}

type sessionPageHub struct {
	hc       *hubClient
	wk       *sessionWorker
	wid      string
	adminTok string
	adminCSRF,
	bobTok, bobCSRF string
}

// startSessionPageHub composes a Hub from the real modules (store, auth,
// authz, workers, API, routing, events and the web module with the
// default page registry, which holds the session view), brings one Worker
// "lab" of the admin online and signs in the admin and a second user "bob".
// modernWorkerFeatures are the features of a current Worker: the fixture Worker
// answers a reviewed dialog, submits in one request and pastes verbatim, so it
// announces them like one (spine AM-13). An older Worker announces none.
var modernWorkerFeatures = []string{proto.FeatureAnswerReviewed, proto.FeatureSendTextSubmit, proto.FeatureTextVerbatim}

func startSessionPageHub(t *testing.T) *sessionPageHub {
	return startSessionPageHubWith(t, modernWorkerFeatures)
}

// startSessionPageHubWith is the hub of the session page tests with a Worker
// that announces the given features.
func startSessionPageHubWith(t *testing.T, features []string) *sessionPageHub {
	t.Helper()
	t.Setenv("MINION_ADMIN_USERNAME", "root")
	t.Setenv("MINION_ADMIN_PASSWORD", "session-page-integration-1")
	t.Setenv("MINION_PUBLIC_URL", "")
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
	registerHubIDModule(r)
	registerWorkersModule(r)
	registerAPI(r, ops.Default)
	registerRouteModule(r)
	registerEventsModule(r)
	registerWeb(r, web.DefaultRegistry)
	var (
		st  *store.Store
		mgr *session.Manager
		wsv *workers.Service
	)
	if err := r.Register("grab", 90, func(_ context.Context, env *Env) error {
		var err error
		if st, err = Require[*store.Store](env); err != nil {
			return err
		}
		if mgr, err = Require[*session.Manager](env); err != nil {
			return err
		}
		wsv, err = Require[*workers.Service](env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := startHub(t, &Config{DataDir: hubstore.Dir(t)}, r)
	sp := &sessionPageHub{hc: newHubClient(t, h, "https"), wk: &sessionWorker{}, wid: store.NewID()}

	admin, err := st.UserByIdentifier(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, ?, 1, 1) ; INSERT INTO user_roles (user_id, role, source) SELECT id, 'user', 'local' FROM users WHERE id NOT IN (SELECT user_id FROM user_roles)`, store.NewID(), "bob"); err != nil {
		t.Fatal(err)
	}
	bob, err := st.UserByIdentifier(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	login := func(u store.UserRow) (string, string) {
		is, err := mgr.Create(context.Background(), u, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return is.Token, is.CSRFToken
	}
	sp.adminTok, sp.adminCSRF = login(admin)
	sp.bobTok, sp.bobCSRF = login(bob)

	key, _ := identity.GenerateKey()
	if err := st.InsertWorker(context.Background(), nil, store.WorkerRow{
		ID: sp.wid, Pub: key.Public(), Name: "lab", OwnerID: admin.ID, Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	hubSide, workerSide := net.Pipe()
	hs, err := transport.NewSession(hubSide, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := transport.NewSession(workerSide, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	wsv.Registry().Add(workers.Online{
		WorkerID: sp.wid, Name: "lab", OwnerID: admin.ID, Kind: "private", Version: proto.Version{Major: 1},
		Ops:      []string{ops.OpSessionSnapshot, ops.OpSessionsList, ops.OpSendText, ops.OpSendKeys, ops.OpSetTask, ops.OpAnswer, ops.OpSessionCreate},
		Features: features,
	}, hs)
	go sp.wk.serve(ws)
	return sp
}

// browser sends a request the way the page's htmx does: the session cookie and
// the CSRF header (when csrf is not empty), a urlencoded form.
func (sp *sessionPageHub) browser(method, path, token, csrf string, form url.Values) (int, string, http.Header) {
	sp.hc.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, sp.hc.base+path, body)
	req.Header.Set("Cookie", session.CookieName+"="+token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
	}
	if csrf != "" {
		req.Header.Set(session.CSRFHeader, csrf)
	}
	// No cookie jar: the cookie above is the only credential of this request.
	c := *sp.hc.c
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		sp.hc.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

var reHiddenInput = regexp.MustCompile(`<input type="hidden" name="([^"]*)" value="([^"]*)">`)

// The session view on a real Hub: the page is served by the web module, every
// action goes page route -> in-process REST -> authz -> router -> Worker, and
// the CSRF check, the owner/admin rules and the API's refusals are the Hub's own.
func TestSessionViewOnARealHub(t *testing.T) {
	sp := startSessionPageHub(t)
	const view = "/s/lab/minion-1"

	// Anonymous: the login, and nothing reaches the Worker.
	if code, _, hdr := sp.browser("GET", view, "", "", nil); code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login") {
		t.Fatalf("anonymous GET = %d %q", code, hdr.Get("Location"))
	}

	// The page: the screen of the Worker, escaped, with the controls of an owner.
	code, body, _ := sp.browser("GET", view, sp.adminTok, "", nil)
	if code != 200 {
		t.Fatalf("GET = %d\n%s", code, body)
	}
	for _, want := range []string{
		`<span class="t-b t-fg-196">&lt;img </span>src=x onerror=alert(1)&gt; done`, `State: dialog.`, `Size: 40x2.`,
		`hx-post="/s/lab/minion-1/text"`, `value="write the docs"`, `hx-post="/s/lab/minion-1/answer"`,
		`data-events-url="/api/v1/events?all=true&amp;worker=` + sp.wid + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
	if strings.Contains(body, "<img src=x") {
		t.Errorf("Worker text was rendered as markup:\n%s", body)
	}
	// The answer form binds the answer to the dialog the page showed: its hidden
	// fields are exactly the tokens of the snapshot the Worker reported.
	pane := fixtureDialogPane()
	reviewed := url.Values{}
	for _, m := range reHiddenInput.FindAllStringSubmatch(body, -1) {
		if strings.HasPrefix(m[1], "expect_") {
			reviewed.Add(m[1], m[2])
		}
	}
	wantReviewed := url.Values{
		"expect_pane_id": {pane.ID}, "expect_pid": {strconv.Itoa(pane.PID)}, "expect_command": {pane.Command},
		"expect_session_created": {strconv.FormatInt(pane.SessionCreated, 10)}, "expect_dialog_digest": {pane.DialogDigest},
	}
	// The answer form of the panel carries the five tokens, each once and with
	// the value of the snapshot the Worker reported (the page has one answer form).
	if len(reviewed) != len(wantReviewed) {
		t.Fatalf("answer form fields = %v, want %v", reviewed, wantReviewed)
	}
	for k, want := range wantReviewed {
		got := reviewed[k]
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("answer form field %s = %q, want the value %q once", k, got, want[0])
		}
	}
	snap := sp.wk.calls(ops.OpSessionSnapshot)
	if len(snap) == 0 {
		t.Fatal("the Worker never got a session_snapshot")
	}
	if tgt, _ := snap[0]["target"].(map[string]any); tgt["session_name"] != "minion-1" {
		t.Errorf("snapshot target = %+v", snap[0]["target"])
	}
	// The fragment route.
	code, body, _ = sp.browser("GET", view+"/screen", sp.adminTok, "", nil)
	if code != 200 || !strings.HasPrefix(body, `<div id="screen-panel">`) {
		t.Errorf("panel = %d\n%s", code, body)
	}

	// CSRF: a post without the header is refused and the Worker sees no write.
	before := sp.wk.total()
	code, _, _ = sp.browser("POST", view+"/text", sp.adminTok, "", url.Values{"text": {"hello"}})
	if code != http.StatusForbidden {
		t.Errorf("POST without CSRF = %d, want 403", code)
	}
	if sp.wk.total() != before || len(sp.wk.calls(ops.OpSendText)) != 0 {
		t.Error("a post without a CSRF token reached the Worker")
	}

	// send_text with the force flow: refused (forceable), then "send anyway".
	code, body, hdr := sp.browser("POST", view+"/text", sp.adminTok, sp.adminCSRF, url.Values{"text": {"busy text"}})
	if code != 200 || !strings.Contains(body, "pane_busy") || !strings.Contains(body, "Send anyway") {
		t.Fatalf("refused text = %d\n%s", code, body)
	}
	if strings.Contains(hdr.Get("HX-Trigger"), "minion-input-sent") {
		t.Error("the input box is cleared after a refusal")
	}
	form := url.Values{}
	for _, m := range reHiddenInput.FindAllStringSubmatch(body, -1) {
		form.Add(m[1], m[2])
	}
	code, body, _ = sp.browser("POST", view+"/text", sp.adminTok, sp.adminCSRF, form)
	if code != 200 || !strings.Contains(body, "Delivered (sent anyway).") {
		t.Fatalf("forced text = %d\n%s", code, body)
	}
	texts := sp.wk.calls(ops.OpSendText)
	if len(texts) != 2 || texts[0]["text"] != "busy text" || texts[1]["text"] != "busy text" ||
		texts[0]["force"] != false || texts[1]["force"] != true || texts[0]["request_id"] == texts[1]["request_id"] {
		t.Errorf("Worker send_text calls = %+v", texts)
	}

	// send_keys, set_task and answer reach the Worker with the right arguments.
	for _, tc := range []struct {
		path string
		form url.Values
		op   string
		key  string
		want any
	}{
		{"/keys", url.Values{"key": {"C-c"}}, ops.OpSendKeys, "keys", []any{"C-c"}},
		{"/task", url.Values{"task": {"ship it"}}, ops.OpSetTask, "task", "ship it"},
		{"/answer", url.Values{"choice": {"2"}}, ops.OpAnswer, "choice", "2"},
	} {
		if tc.path == "/answer" { // the browser posts the hidden fields of the form it showed
			for k := range reviewed {
				tc.form.Set(k, reviewed.Get(k))
			}
		}
		code, body, _ := sp.browser("POST", view+tc.path, sp.adminTok, sp.adminCSRF, tc.form)
		ok := strings.Contains(body, "Delivered.") || (tc.path == "/task" && strings.Contains(body, "Saved."))
		if code != 200 || !ok {
			t.Errorf("POST %s = %d\n%s", tc.path, code, body)
			continue
		}
		calls := sp.wk.calls(tc.op)
		if len(calls) != 1 || !ops.ValidUUIDv7(calls[0]["request_id"].(string)) {
			t.Errorf("%s: Worker calls = %+v", tc.op, calls)
			continue
		}
		got, _ := json.Marshal(calls[0][tc.key])
		want, _ := json.Marshal(tc.want)
		if string(got) != string(want) {
			t.Errorf("%s: %s = %s, want %s", tc.op, tc.key, got, want)
		}
	}

	// The Worker got the reviewed dialog as expect, as the form carried it.
	if ans := sp.wk.calls(ops.OpAnswer); len(ans) != 1 {
		t.Fatalf("Worker answer calls = %d, want 1", len(ans))
	} else {
		raw, _ := json.Marshal(ans[0]["expect"])
		var got ops.ReviewedDialog
		if err := json.Unmarshal(raw, &got); err != nil || got != reviewedOfFixture(pane) {
			t.Errorf("answer expect = %s, want %+v", raw, reviewedOfFixture(pane))
		}
	}

	// An answer without the reviewed dialog is never sent (the page lost it) and
	// an answer to another dialog is refused by the Worker, not retried.
	answers := len(sp.wk.calls(ops.OpAnswer))
	code, body, _ = sp.browser("POST", view+"/answer", sp.adminTok, sp.adminCSRF, url.Values{"choice": {"1"}})
	if code != 200 || !strings.Contains(body, "lost the dialog") || len(sp.wk.calls(ops.OpAnswer)) != answers {
		t.Errorf("answer without the reviewed dialog = %d, Worker answers %d -> %d\n%s", code, answers, len(sp.wk.calls(ops.OpAnswer)), body)
	}
	other := url.Values{"choice": {"1"}}
	for k := range reviewed {
		other.Set(k, reviewed.Get(k))
	}
	other.Set("expect_dialog_digest", strings.Repeat("cd", 32))
	code, body, hdr = sp.browser("POST", view+"/answer", sp.adminTok, sp.adminCSRF, other)
	if code != 200 || !strings.Contains(body, "The dialog changed") ||
		strings.Contains(body, "Send anyway") || !strings.Contains(hdr.Get("HX-Trigger"), "minion-dialog-stale") ||
		len(sp.wk.calls(ops.OpAnswer)) != answers+1 {
		t.Errorf("answer to another dialog = %d, HX-Trigger %q, Worker answers %d\n%s", code, hdr.Get("HX-Trigger"), len(sp.wk.calls(ops.OpAnswer)), body)
	}

	// New session: the Worker names it, the browser is sent to it.
	code, _, hdr = sp.browser("POST", "/w/lab/sessions", sp.adminTok, sp.adminCSRF, url.Values{})
	if code != 200 || hdr.Get("HX-Redirect") != "/s/lab/minion-5" {
		t.Errorf("new session = %d, HX-Redirect %q", code, hdr.Get("HX-Redirect"))
	}
	if n := len(sp.wk.calls(ops.OpSessionCreate)); n != 1 {
		t.Errorf("Worker session_create calls = %d", n)
	}
}

// A user who does not own the Worker (and is no admin) gets a plain 404 for the
// page, and the API refuses every write the page routes forward (defence in
// depth: nothing depends on the hidden controls).
func TestSessionViewRefusesAUserWithoutAccessOnARealHub(t *testing.T) {
	sp := startSessionPageHub(t)
	const view = "/s/lab/minion-1"
	before := sp.wk.total()
	if code, _, _ := sp.browser("GET", view, sp.bobTok, "", nil); code != http.StatusNotFound {
		t.Errorf("GET as bob = %d, want 404", code)
	}
	if code, _, _ := sp.browser("GET", view+"/screen", sp.bobTok, "", nil); code != http.StatusNotFound {
		t.Errorf("panel as bob = %d, want 404", code)
	}
	for path, form := range map[string]url.Values{
		view + "/text":    {"text": {"rm -rf /"}},
		view + "/keys":    {"key": {"Enter"}},
		view + "/task":    {"task": {"x"}},
		view + "/answer":  {"choice": {"1"}},
		"/w/lab/sessions": {},
	} {
		code, body, hdr := sp.browser("POST", path, sp.bobTok, sp.bobCSRF, form)
		if code != 200 || !strings.Contains(body, "result-danger") || hdr.Get("HX-Redirect") != "" {
			t.Errorf("POST %s as bob = %d\n%s", path, code, body)
		}
	}
	// The same through the API itself, with bob's session and a worker id.
	for _, op := range []string{ops.OpSendText, ops.OpSetTask} {
		args := map[string]any{"request_id": ops.NewUUIDv7(), "target": map[string]any{"worker_id": sp.wid, "session_name": "minion-1"}, "text": "x", "task": "x"}
		b, _ := json.Marshal(args)
		req, _ := http.NewRequest("POST", sp.hc.base+ops.RESTPrefix+op, strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", session.CookieName+"="+sp.bobTok)
		req.Header.Set(session.CSRFHeader, sp.bobCSRF)
		resp, err := sp.hc.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("API %s as bob = %d, want a refusal", op, resp.StatusCode)
		}
	}
	if sp.wk.total() != before {
		t.Errorf("bob's requests reached the Worker: %d calls", sp.wk.total()-before)
	}
}

// TestSessionViewWithAnOlderWorkerFailsClosedBeforeAnyEffect is the composition
// of the page and the Hub's capability guard: the same page, the same reviewed
// tokens and the same real Hub, with a Worker that announces no features (an older
// build, which would ignore the expect binding and the verbatim paste). The
// answer and a multi-line text are refused with the update-needed message and
// the Worker receives neither; the reads of the page and a plain one-line text
// still work. Nothing here loosens the guard to make the page work.
func TestSessionViewWithAnOlderWorkerFailsClosedBeforeAnyEffect(t *testing.T) {
	sp := startSessionPageHubWith(t, nil)
	const view = "/s/lab/minion-1"
	code, body, _ := sp.browser("GET", view, sp.adminTok, "", nil)
	if code != 200 {
		t.Fatalf("GET %s = %d", view, code)
	}
	reviewed := url.Values{}
	for _, m := range reHiddenInput.FindAllStringSubmatch(body, -1) {
		if strings.HasPrefix(m[1], "expect_") {
			reviewed.Add(m[1], m[2])
		}
	}
	if len(reviewed) != 5 {
		t.Fatalf("the page did not carry the reviewed tokens: %v", reviewed)
	}
	answer := url.Values{"choice": {"2"}}
	for k := range reviewed {
		answer.Set(k, reviewed.Get(k))
	}
	for name, tc := range map[string]struct {
		path string
		form url.Values
	}{
		"answer":          {"/answer", answer},
		"multi-line text": {"/text", url.Values{"text": {"line one\nline two"}}},
	} {
		code, body, _ := sp.browser("POST", view+tc.path, sp.adminTok, sp.adminCSRF, tc.form)
		if code != 200 || !strings.Contains(body, "must be updated") || !strings.Contains(body, "nothing was sent") || strings.Contains(body, "Delivered.") {
			t.Errorf("%s to an older Worker = %d\n%s", name, code, body)
		}
	}
	if n := len(sp.wk.calls(ops.OpAnswer)) + len(sp.wk.calls(ops.OpSendText)); n != 0 {
		t.Fatalf("the older Worker received %d guarded requests", n)
	}
	// A plain one-line text is handled the same by every Worker and still goes.
	if code, body, _ := sp.browser("POST", view+"/text", sp.adminTok, sp.adminCSRF, url.Values{"text": {"plain line"}}); code != 200 || !strings.Contains(body, "Delivered.") {
		t.Errorf("a plain text to an older Worker = %d\n%s", code, body)
	}
}
