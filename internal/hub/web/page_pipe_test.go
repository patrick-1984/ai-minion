package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Story 7.5 on the web side: the conversation component, its routes and its outcomes, against a fake broker that
// answers the three pipe ops like the Hub's pipe service does (the real broker is exercised in
// internal/hub/server/pipe_web_test.go).

const hostileTurn = `<script>alert(1)</script> & "q" 'x' <img src=x onerror=alert(2)>`

func pipeTurns() []ops.PipeTurn {
	at := ops.Timestamp(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	return []ops.PipeTurn{
		{TurnID: "0190a1b2-c3d4-7e5f-8a6b-000000000001", Session: "minion-1", Direction: ops.PipeSent, Text: "hello <b>agent</b>", At: at},
		{TurnID: "0190a1b2-c3d4-7e5f-8a6b-000000000002", Session: "minion-1", Direction: ops.PipeReceived, Text: hostileTurn, At: at},
	}
}

func pipeOps(t *testing.T) (*Server, *fakeOps) {
	t.Helper()
	s, f := sessionOps(t)
	f.on(ops.OpPipeRead, func(args map[string]any) (any, error) {
		if args["after_turn_id"] != nil && args["after_turn_id"] != "" {
			return ops.PipeReadResult{Turns: []ops.PipeTurn{}}, nil
		}
		return ops.PipeReadResult{Turns: pipeTurns()}, nil
	}).reply(ops.OpPipeSend, delivered)
	return s, f
}

func count(body, sub string) int { return strings.Count(body, sub) }

func TestConversationIsOneEscapedComponentInTheFullPage(t *testing.T) {
	s, f := pipeOps(t)
	body := sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	// One component with one set of ids; it is a view of the right pane, next to Details.
	for _, id := range []string{`id="pipe-chat"`, `id="pipe-turns"`, `id="pipe-text"`, `id="pipe-send"`, `id="pipe-jump"`, `id="pipe-form"`, `id="pipe-live"`} {
		if n := count(body, id); n != 1 {
			t.Errorf("%s appears %d times, want exactly once", id, n)
		}
	}
	mustHave(t, body, `id="pane-tab-conversation"`, `id="pane-tab-details"`, `id="pane-view-conversation"`, `<script src="/static/pipe-chat.js" defer></script>`,
		`data-pipe-read="/s/pi-1/minion-1/pipe/turns"`, `hx-post="/s/pi-1/minion-1/pipe"`, ` data-write data-pipe>`,
		`Enter sends the message. Shift+Enter adds a line break.`, `The pipe submits them to the session`)
	// No dock tab of the compact view in the full page (the same component is not drawn twice).
	mustNotHave(t, body, `id="tab-pipe"`, `id="dock-pipe"`)
	// Text is text: a turn is never markup.
	mustHave(t, body, "hello &lt;b&gt;agent&lt;/b&gt;", "&lt;script&gt;alert(1)&lt;/script&gt; &amp; &#34;q&#34; &#39;x&#39; &lt;img src=x onerror=alert(2)&gt;")
	mustNotHave(t, body, "<script>alert", "<img src=x", "<b>agent")
	// Sent and received turns are told apart by words, not only by position or colour.
	mustHave(t, body, `data-dir="sent"`, `data-dir="received"`, "Your message to the agent", "Agent reply", `data-latest="0190a1b2-c3d4-7e5f-8a6b-000000000002"`)
	// The read is the broker's pipe_read of the canonical target (the Worker id and the session of the path).
	if n := len(f.called(ops.OpPipeRead)); n == 0 {
		t.Fatal("the conversation was not read through pipe_read")
	}
	a := argsOf(f, ops.OpPipeRead, 0)
	tg, _ := a["target"].(map[string]any)
	if tg["worker_id"] != sessWorkerID || tg["session_name"] != "minion-1" {
		t.Errorf("pipe_read target %+v, want the canonical worker id and the session", a["target"])
	}
	if n := len(f.called(ops.OpSendText)) + len(f.called(ops.OpPipeSend)); n != 0 {
		t.Errorf("rendering the page sent %d writes", n)
	}
}

func TestConversationIsOneComponentInTheCompactView(t *testing.T) {
	s, _ := pipeOps(t)
	body := sessGet(t, s, "/s/pi-1/minion-1?embed=1", sessUser).Body.String()
	// The compact view has no pane: the conversation is a discoverable tab of the dock, exactly once.
	for _, id := range []string{`id="pipe-chat"`, `id="pipe-turns"`, `id="pipe-text"`, `id="pipe-send"`} {
		if n := count(body, id); n != 1 {
			t.Errorf("compact view: %s appears %d times, want once", id, n)
		}
	}
	mustHave(t, body, `id="tab-pipe" aria-controls="dock-pipe"`, `>Conversation</button>`, `id="dock-pipe" role="tabpanel" aria-labelledby="tab-pipe"`, `<script src="/static/pipe-chat.js" defer></script>`)
	mustNotHave(t, body, `id="session-pane"`, `pane-tab-conversation`, `id="pipe-readonly"`)
	mustHave(t, body, "hello &lt;b&gt;agent&lt;/b&gt;")
}

func TestConversationForAPersonWhoCanOnlyRead(t *testing.T) {
	s, f := pipeOps(t)
	foreign := sessionWorker()
	foreign.OwnerID, foreign.Owner = "u-carol", "carol"
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{foreign}})
	for _, path := range []string{"/s/pi-1/minion-1", "/s/pi-1/minion-1?embed=1"} {
		body := sessGet(t, s, path, sessUser).Body.String()
		// They can read the stored turns; there is no composer and no write form.
		mustHave(t, body, `id="pipe-turns"`, "hello &lt;b&gt;agent&lt;/b&gt;")
		mustNotHave(t, body, `id="pipe-text"`, `id="pipe-send"`, `id="pipe-form"`)
		if n := count(body, `id="pipe-turns"`); n != 1 {
			t.Errorf("%s: the component exists %d times", path, n)
		}
	}
	if !strings.Contains(sessGet(t, s, "/s/pi-1/minion-1?embed=1", sessUser).Body.String(), `id="pipe-readonly"`) {
		t.Error("the read-only compact view has no conversation section")
	}
}

func TestConversationSaysWhyItCannotSend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mod    func(*ops.WorkerInfo, *fakeOps)
		want   string
		absent string
	}{
		{"offline", func(w *ops.WorkerInfo, f *fakeOps) { w.Online = false }, "The Worker is offline", ""},
		{"no pipe op", func(w *ops.WorkerInfo, f *fakeOps) { w.Ops = []string{ops.OpSendText} }, "does not offer the pipe", ""},
		{"no causal feature", func(w *ops.WorkerInfo, f *fakeOps) {
			w.Ops = []string{ops.OpPipeSend}
			w.Features = []string{"answer.reviewed"}
		}, "needs an update before it can use the pipe", ""},
		{"shell session", func(w *ops.WorkerInfo, f *fakeOps) {
			f.reply(ops.OpSessionSnapshot, ops.SessionSnapshotResult{Pane: ops.PaneInfo{Session: "minion-1", ID: "%1", Kind: ops.PaneKindShell, State: "stable"}, Screen: hostileScreen()})
		}, "This is a shell session", ""},
	} {
		s, f := pipeOps(t)
		w := sessionWorker()
		tc.mod(&w, f)
		f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w}})
		body := sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
		mustHave(t, body, `id="pipe-unavailable"`, tc.want, `id="pipe-send" disabled data-unavailable="true"`)
		// Stored turns stay readable and nothing is invented.
		mustHave(t, body, "hello &lt;b&gt;agent&lt;/b&gt;")
	}
	// A capable agent session has no such notice and an enabled button.
	s, f := pipeOps(t)
	w := sessionWorker()
	w.Ops, w.Features = []string{ops.OpPipeSend, ops.OpPipeRead}, ops.RequiredFeatures(ops.OpPipeSend, []byte(`{"text":"x"}`))
	f.reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w}})
	body := sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	mustNotHave(t, body, `id="pipe-unavailable"`, `data-unavailable`)
	mustHave(t, body, `<button type="submit" id="pipe-send">Send message</button>`)
	// The page says what replies need; it never claims a reply it cannot show.
	mustHave(t, body, "tmux 3.8 or later", "Replies appear when the Worker can capture them reliably")
}

func TestConversationReadFailureIsShownAndNothingIsInvented(t *testing.T) {
	s, f := pipeOps(t)
	f.on(ops.OpPipeRead, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 502, Code: "worker_error", Message: "read failed"}
	})
	body := sessGet(t, s, "/s/pi-1/minion-1", sessUser).Body.String()
	mustHave(t, body, "The conversation could not be read just now.", `data-pipe-error`)
	mustNotHave(t, body, `data-turn=`, "Agent reply")
	rec := sessGet(t, s, "/s/pi-1/minion-1/pipe/turns", sessUser)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("the live read answered %d on a failed read, want 502 (the browser keeps the list)", rec.Code)
	}
}

func TestConversationLiveRouteEscapesAndRequiresASession(t *testing.T) {
	s, f := pipeOps(t)
	rec := sessGet(t, s, "/s/pi-1/minion-1/pipe/turns", sessUser)
	if rec.Code != http.StatusOK {
		t.Fatalf("turns: %d", rec.Code)
	}
	body := rec.Body.String()
	mustHave(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;", `data-turn="0190a1b2-c3d4-7e5f-8a6b-000000000002"`)
	mustNotHave(t, body, "<script", "<img", "<html")
	// No session: nothing is read and nothing is rendered.
	before := len(f.called(ops.OpPipeRead))
	anon := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/s/pi-1/minion-1/pipe/turns", nil)
	s.ServeHTTP(anon, req)
	if anon.Code == http.StatusOK || strings.Contains(anon.Body.String(), "hello") {
		t.Errorf("an anonymous read answered %d: %q", anon.Code, anon.Body.String())
	}
	if len(f.called(ops.OpPipeRead)) != before {
		t.Error("an anonymous request reached pipe_read")
	}
	// A session or Worker that is not the caller's: a plain 404, no read.
	rec = sessGet(t, s, "/s/other-worker/minion-1/pipe/turns", sessUser)
	if rec.Code != http.StatusNotFound {
		t.Errorf("another Worker: %d, want 404", rec.Code)
	}
}

func TestPipeSendIsOneBrokeredCallWithItsOutcome(t *testing.T) {
	s, f := pipeOps(t)
	ids := map[string]bool{}
	for i := 0; i < 2; i++ {
		rec := post(t, s, "/s/pi-1/minion-1/pipe", sessUser, url.Values{"text": {"line one\r\nline two"}})
		mustHave(t, rec.Body.String(), "Message:", "Delivered.")
		trig := rec.Header().Get("HX-Trigger")
		mustHave(t, trig, `"minion-input-sent":true`, `"minion-pipe-refresh":true`)
		mustNotHave(t, trig, "minion-input-unknown")
		a := argsOf(f, ops.OpPipeSend, i)
		tg, _ := a["target"].(map[string]any)
		if a["text"] != "line one\nline two" || tg["worker_id"] != sessWorkerID || tg["session_name"] != "minion-1" || a["force"] == true {
			t.Errorf("pipe_send args %+v", a)
		}
		id, _ := a["request_id"].(string)
		if !ops.ValidUUIDv7(id) || ids[id] {
			t.Errorf("request id %q is not a fresh UUIDv7", id)
		}
		ids[id] = true
	}
	// The pipe is not the terminal: no send_text or send_keys call, and no message_id picked by the page.
	if n := len(f.called(ops.OpSendText)) + len(f.called(ops.OpSendKeys)); n != 0 {
		t.Errorf("a pipe message made %d terminal calls", n)
	}
	if a := argsOf(f, ops.OpPipeSend, 0); a["message_id"] != nil {
		t.Errorf("the page set message_id: %+v", a)
	}
}

func TestPipeSendRefusesABlankMessageBeforeAnyCall(t *testing.T) {
	for _, blank := range []string{"", " ", "\t\n", "\r\n \r\n", "\u00a0", "\ufeff"} {
		s, f := pipeOps(t)
		rec := post(t, s, "/s/pi-1/minion-1/pipe", sessUser, url.Values{"text": {blank}})
		mustHave(t, rec.Body.String(), "Type a message first. Nothing was sent.")
		if n := len(f.called(ops.OpPipeSend)) + len(f.called(ops.OpSendText)) + len(f.called(ops.OpSendKeys)); n != 0 {
			t.Errorf("blank %q made %d calls", blank, n)
		}
		mustNotHave(t, rec.Header().Get("HX-Trigger"), "minion-input-sent", "minion-pipe-refresh")
	}
}

func TestPipeSendForceIsExplicitAndRepeatsTheOriginalMessageOnly(t *testing.T) {
	s, f := pipeOps(t)
	f.reply(ops.OpPipeSend, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "pane_busy", Forceable: true})
	rec := post(t, s, "/s/pi-1/minion-1/pipe", sessUser, url.Values{"text": {"original <message>"}})
	body := rec.Body.String()
	// A forceable refusal offers "Send anyway" as a form of the same route, with the original text only and an explicit force=1.
	mustHave(t, body, `action="/s/pi-1/minion-1/pipe"`, `name="text" value="original &lt;message&gt;"`, `name="force" value="1"`, "Send anyway to minion-1 on pi-1?", "Nothing reached the pane")
	if n := count(body, `type="hidden"`); n != 2 {
		t.Errorf("the force form carries %d hidden fields, want text and force only", n)
	}
	mustNotHave(t, rec.Header().Get("HX-Trigger"), "minion-input-sent")
	if a := argsOf(f, ops.OpPipeSend, 0); a["force"] == true {
		t.Errorf("the first attempt was forced: %+v", a)
	}
	if n := len(f.called(ops.OpPipeSend)); n != 1 {
		t.Fatalf("a refused send was followed by %d calls: nothing is retried by the page", n)
	}
	// The confirmation posts the original text with force=1: one more call, a new request id, force set.
	f.reply(ops.OpPipeSend, ops.WriteResult{Outcome: ops.OutcomeDelivered, Forced: true})
	rec = post(t, s, "/s/pi-1/minion-1/pipe", sessUser, url.Values{"text": {"original <message>"}, "force": {"1"}})
	mustHave(t, rec.Body.String(), "Delivered (sent anyway).")
	a0, a1 := argsOf(f, ops.OpPipeSend, 0), argsOf(f, ops.OpPipeSend, 1)
	if a1["force"] != true || a1["text"] != "original <message>" || a1["request_id"] == a0["request_id"] {
		t.Errorf("forced call %+v after %+v", a1, a0)
	}
	// A refusal that is not forceable offers no force form at all.
	f.reply(ops.OpPipeSend, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "pipe_budget_exceeded"})
	rec = post(t, s, "/s/pi-1/minion-1/pipe", sessUser, url.Values{"text": {"x"}})
	mustHave(t, rec.Body.String(), "Not sent")
	mustNotHave(t, rec.Body.String(), `name="force"`, "Send anyway")
}

func TestPipeSendUnknownAndPartialAreKeptByThePageNotRetried(t *testing.T) {
	for _, outcome := range []ops.Outcome{ops.OutcomeUnknown, ops.OutcomePartial} {
		s, f := pipeOps(t)
		f.reply(ops.OpPipeSend, ops.WriteResult{Outcome: outcome, Reason: "worker_lost"})
		rec := post(t, s, "/s/pi-1/minion-1/pipe", sessUser, url.Values{"text": {"keep <me>"}})
		trig := rec.Header().Get("HX-Trigger")
		mustHave(t, trig, `"minion-input-unknown":{"outcome":"`+string(outcome)+`","reason":"worker_lost"}`, `"minion-pipe-refresh":true`)
		mustNotHave(t, trig, "minion-input-sent")
		mustHave(t, rec.Body.String(), "keep &lt;me&gt;")
		if n := len(f.called(ops.OpPipeSend)); n != 1 {
			t.Errorf("%s: %d calls, want exactly one: unknown is never retried", outcome, n)
		}
	}
	// A transport failure of the call is shown as a failure and carries no "delivered" signal.
	s, f := pipeOps(t)
	f.on(ops.OpPipeSend, func(map[string]any) (any, error) {
		return nil, &OpError{Status: 502, Code: "worker_offline", Message: "The Worker is offline."}
	})
	rec := post(t, s, "/s/pi-1/minion-1/pipe", sessUser, url.Values{"text": {"x"}})
	mustHave(t, rec.Body.String(), "result-danger")
	mustNotHave(t, rec.Header().Get("HX-Trigger"), "minion-input-sent")
}

func TestPipeSendWithoutASessionOrAnotherWorkersSessionMakesNoCall(t *testing.T) {
	s, f := pipeOps(t)
	rec := post(t, s, "/s/pi-1/minion-1/pipe", "", url.Values{"text": {"x"}})
	if len(f.called(ops.OpPipeSend)) != 0 {
		t.Errorf("an anonymous post reached pipe_send (status %d)", rec.Code)
	}
	rec = post(t, s, "/s/other/minion-1/pipe", sessUser, url.Values{"text": {"x"}})
	if len(f.called(ops.OpPipeSend)) != 0 {
		t.Errorf("a post to a Worker that is not the caller's reached pipe_send (status %d)", rec.Code)
	}
}

// The conversation view script runs against a DOM stub with real scroll geometry (testdata/pipe_chat_test.js): it follows new turns only
// at the bottom, keeps a reader who scrolled up where they are, never takes the focus, and reads nothing while hidden or paused.
func TestPipeChatScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script, _ := filepath.Abs(filepath.Join("testdata", "pipe_chat_test.js"))
	src, _ := filepath.Abs(filepath.Join("static", "pipe-chat.js"))
	out, err := exec.Command(node, script, src).CombinedOutput()
	if err != nil {
		t.Fatalf("pipe-chat.js: %v\n%s", err, out)
	}
}

// ---- S7.5 review 1 (S75-R1-1): a long history is read in bounded steps, every page is authorised, nothing private is returned on a failed page ----

func uuidN(i int) string {
	return "0190a1b2-c3d4-7e5f-8a6b-" + strings.Repeat("0", 12-len(itoa(i))) + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// endlessPipe answers every page with a full page of turns that continue the cursor: a history that never ends within the bound.
func endlessPipe(f *fakeOps, failPage int) {
	n := 0
	f.on(ops.OpPipeRead, func(args map[string]any) (any, error) {
		n++
		if failPage > 0 && n == failPage {
			return nil, &OpError{Status: 403, Code: "forbidden", Message: "not allowed"}
		}
		start := 0
		if a, _ := args["after_turn_id"].(string); a != "" {
			start = atoiTail(a)
		}
		out := make([]ops.PipeTurn, 0, 200)
		for i := 1; i <= 200; i++ {
			out = append(out, ops.PipeTurn{TurnID: uuidN(start + i), Session: "minion-1", Direction: ops.PipeReceived, Text: "PRIVATE-" + itoa(start+i)})
		}
		return ops.PipeReadResult{Turns: out}, nil
	})
}

func atoiTail(id string) int {
	v := 0
	for _, c := range id[len(id)-12:] {
		v = v*10 + int(c-'0')
	}
	return v
}

func TestConversationReadIsBoundedAndSaysItIsBehind(t *testing.T) {
	s, f := sessionOps(t)
	endlessPipe(f, 0)
	for _, path := range []string{"/s/pi-1/minion-1", "/s/pi-1/minion-1?embed=1", "/s/pi-1/minion-1/pipe/turns"} {
		before := len(f.called(ops.OpPipeRead))
		body := sessGet(t, s, path, sessUser).Body.String()
		if n := len(f.called(ops.OpPipeRead)) - before; n != pipeMaxPages {
			t.Errorf("%s: %d pipe_read calls for one render, want the bound %d", path, n, pipeMaxPages)
		}
		mustHave(t, body, `data-pipe-behind="1"`, `data-pipe-note="behind"`, "Older turns exist and are not shown.")
		if n := count(body, `data-turn="`); n != pipeShownTurns {
			t.Errorf("%s: %d turns shown, want the window of %d", path, n, pipeShownTurns)
		}
		// The window is the newest the walk saw and the cursor goes on from the turn before it: 1000 read, 800 is the anchor.
		mustHave(t, body, `data-pipe-walk="`+uuidN(800)+`"`, "PRIVATE-1000")
		mustNotHave(t, body, "PRIVATE-800<", "PRIVATE-1001")
	}
	// The next step goes on from that cursor, from the page's own session only.
	before := len(f.called(ops.OpPipeRead))
	sessGet(t, s, "/s/pi-1/minion-1/pipe/turns?walk="+uuidN(800), sessUser)
	if a := argsOf(f, ops.OpPipeRead, before); a["after_turn_id"] != uuidN(800) {
		t.Errorf("the step did not start at the cursor: %+v", a)
	}
}

func TestConversationFailedPageReturnsNothingPrivateAndKeepsTheBrowsersList(t *testing.T) {
	for _, failPage := range []int{1, 2, 4} {
		s, f := sessionOps(t)
		endlessPipe(f, failPage)
		for _, path := range []string{"/s/pi-1/minion-1", "/s/pi-1/minion-1?embed=1"} {
			f.mu.Lock()
			f.calls = nil
			f.mu.Unlock()
			endlessPipe(f, failPage)
			body := sessGet(t, s, path, sessUser).Body.String()
			mustHave(t, body, `data-pipe-error`)
			mustNotHave(t, body, "PRIVATE-", `data-turn=`)
		}
		f.mu.Lock()
		f.calls = nil
		f.mu.Unlock()
		endlessPipe(f, failPage)
		rec := sessGet(t, s, "/s/pi-1/minion-1/pipe/turns", sessUser)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "PRIVATE-") {
			t.Errorf("page %d fails: the live read answered %d with %q", failPage, rec.Code, rec.Body.String())
		}
	}
}

func TestConversationCursorThatIsGoneStartsAgainFromTheStartAndARefusalIsNotRetried(t *testing.T) {
	s, f := sessionOps(t)
	f.on(ops.OpPipeRead, func(args map[string]any) (any, error) {
		a, _ := args["after_turn_id"].(string)
		switch a {
		case "":
			return ops.PipeReadResult{Turns: pipeTurns()}, nil
		case pipeTurns()[1].TurnID:
			return ops.PipeReadResult{Turns: []ops.PipeTurn{}}, nil
		}
		return nil, &OpError{Status: 404, Code: "not_found", Message: "no such turn"}
	})
	rec := sessGet(t, s, "/s/pi-1/minion-1/pipe/turns?walk="+uuidN(5), sessUser)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `data-turn="0190a1b2-c3d4-7e5f-8a6b-000000000002"`) {
		t.Errorf("a cursor that is gone did not fall back to the plain read: %d %s", rec.Code, rec.Body.String())
	}
	// A real refusal fails the same way from the start and nothing is shown.
	s, f = sessionOps(t)
	f.on(ops.OpPipeRead, func(map[string]any) (any, error) { return nil, &OpError{Status: 403, Code: "forbidden", Message: "no"} })
	rec = sessGet(t, s, "/s/pi-1/minion-1/pipe/turns?walk="+uuidN(5), sessUser)
	if rec.Code == 200 || len(f.called(ops.OpPipeRead)) != 1 {
		t.Errorf("a refusal answered %d after %d calls", rec.Code, len(f.called(ops.OpPipeRead)))
	}
}
