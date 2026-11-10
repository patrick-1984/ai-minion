package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Story 7.5 against the real Hub: the web session view, with real session cookies, drives the real pipe broker (the store, the
// trust and budget checks, the routing to a Worker connected over the real handshake) through the real /s/... routes. The fake is
// only the Worker's side of the wire. Nothing in the page is read off a screen: every turn on it is a stored pipe turn.

// webDo sends a browser form (or a plain GET) as the given session and returns the status, body and headers.
func (ph *pipeHub) webDo(t *testing.T, method, path string, form url.Values, as map[string]string) (int, string, http.Header) {
	t.Helper()
	var rd io.Reader
	if form != nil {
		rd = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, ph.hc.base+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("HX-Request", "true")
	for k, v := range as {
		req.Header.Set(k, v)
	}
	resp, err := ph.hc.c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func count(s, sub string) int { return strings.Count(s, sub) }

func TestWebConversationSendsAndShowsStoredTurnsThroughTheRealBroker(t *testing.T) {
	ph := startPipeHub(t, nil, nil)
	ctx := context.Background()
	pi1 := ph.connectShared(t, "pi-1")
	peer := pipe.Peer{WorkerID: pi1.id, SessionName: "minion-1"}
	base := "/s/" + pi1.id + "/minion-1"

	// The page: one conversation component in the full page and one in the compact view, nothing read off the screen.
	code, page, _ := ph.webDo(t, "GET", base, nil, ph.asAdmin)
	if code != 200 {
		t.Fatalf("session page = %d", code)
	}
	for _, id := range []string{`id="pipe-turns"`, `id="pipe-text"`, `id="pipe-send"`} {
		if n := count(page, id); n != 1 {
			t.Errorf("full page: %s appears %d times", id, n)
		}
	}
	if !strings.Contains(page, `id="pane-tab-conversation"`) || strings.Contains(page, `id="dock-pipe"`) || !strings.Contains(page, "No messages yet.") {
		t.Errorf("full page does not hold the conversation as a pane view with the empty state")
	}
	_, compact, _ := ph.webDo(t, "GET", base+"?embed=1", nil, ph.asAdmin)
	if n := count(compact, `id="pipe-turns"`); n != 1 || !strings.Contains(compact, `id="tab-pipe"`) || strings.Contains(compact, `pane-tab-conversation`) {
		t.Errorf("compact view: component count %d, tab present %v", n, strings.Contains(compact, `id="tab-pipe"`))
	}

	// Send: one pipe_send through the broker; the message is stored and the Worker got the Hub's message_id.
	hostile := "run <script>alert(1)</script> & \"quote\"\nsecond line"
	code, out, hdr := ph.webDo(t, "POST", base+"/pipe", url.Values{"text": {hostile}}, ph.asAdmin)
	if code != 200 || !strings.Contains(out, "Delivered.") {
		t.Fatalf("send = %d %s", code, out)
	}
	for _, want := range []string{`"minion-input-sent":true`, `"minion-pipe-refresh":true`} {
		if !strings.Contains(hdr.Get("HX-Trigger"), want) {
			t.Errorf("HX-Trigger %q lacks %s", hdr.Get("HX-Trigger"), want)
		}
	}
	msgs, _ := ph.st.Pipe().Messages(ctx, peer, 0, 0)
	if len(msgs) != 1 || msgs[0].Text != hostile {
		t.Fatalf("stored messages = %+v", msgs)
	}
	var sends int
	for _, rq := range pi1.requests() {
		if rq.Op == ops.OpPipeSend {
			sends++
		}
		if rq.Op == ops.OpSendText || rq.Op == ops.OpSendKeys {
			t.Errorf("a pipe message reached the terminal ops: %s", rq.Op)
		}
	}
	if sends != 1 {
		t.Fatalf("the Worker received %d pipe_send requests, want 1", sends)
	}

	// The Worker's reply turn (answering that delivery) is stored by the broker; the live read shows both, escaped.
	if ack := pi1.emit(t, "minion-1", msgs[0].ID, "<b>done</b> & ok"); ack.Status != pipe.AckStored {
		t.Fatalf("ack = %+v", ack)
	}
	code, turns, _ := ph.webDo(t, "GET", base+"/pipe/turns", nil, ph.asAdmin)
	if code != 200 {
		t.Fatalf("turns = %d %s", code, turns)
	}
	for _, want := range []string{`data-dir="sent"`, `data-dir="received"`, "run &lt;script&gt;alert(1)&lt;/script&gt; &amp; &#34;quote&#34;", "&lt;b&gt;done&lt;/b&gt; &amp; ok", "Your message to the agent", "Agent reply"} {
		if !strings.Contains(turns, want) {
			t.Errorf("turns lack %q:\n%s", want, turns)
		}
	}
	if strings.Contains(turns, "<script") || strings.Contains(turns, "<b>done") {
		t.Errorf("a turn was rendered as markup: %s", turns)
	}
	if strings.Index(turns, `data-dir="sent"`) > strings.Index(turns, `data-dir="received"`) {
		t.Error("turns are not in commit order")
	}
	// The same turns are in a fresh render of the page (initial state), once.
	_, page, _ = ph.webDo(t, "GET", base, nil, ph.asAdmin)
	if !strings.Contains(page, "&lt;b&gt;done&lt;/b&gt; &amp; ok") || count(page, `id="pipe-turns"`) != 1 {
		t.Error("the page does not show the stored reply exactly once")
	}

	// A blank message makes no Worker call and no history row.
	before := len(pi1.requests())
	code, out, _ = ph.webDo(t, "POST", base+"/pipe", url.Values{"text": {" \n\t "}}, ph.asAdmin)
	if code != 200 || !strings.Contains(out, "Type a message first.") || len(pi1.requests()) != before {
		t.Errorf("blank message: %d %s, new requests %d", code, out, len(pi1.requests())-before)
	}
}

func TestWebConversationOutcomesBudgetForceAndUnknownThroughTheRealBroker(t *testing.T) {
	ph := startPipeHub(t, nil, nil)
	pi1 := ph.connectShared(t, "pi-1")
	base := "/s/pi-1/minion-1"
	sends := func() int {
		n := 0
		for _, rq := range pi1.requests() {
			if rq.Op == ops.OpPipeSend {
				n++
			}
		}
		return n
	}

	// A refusal the Worker says can be forced is offered as "Send anyway" with the ORIGINAL text only; nothing is retried by the page.
	pi1.setAnswer(ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "pane_busy", Forceable: true})
	code, out, hdr := ph.webDo(t, "POST", base+"/pipe", url.Values{"text": {"original message"}}, ph.asAdmin)
	if code != 200 || !strings.Contains(out, "Not sent") || !strings.Contains(out, `name="force" value="1"`) || !strings.Contains(out, `name="text" value="original message"`) {
		t.Fatalf("forceable refusal = %d %s", code, out)
	}
	if strings.Contains(hdr.Get("HX-Trigger"), "minion-input-sent") || sends() != 1 {
		t.Errorf("a refusal was treated as delivered or retried: trigger=%q sends=%d", hdr.Get("HX-Trigger"), sends())
	}
	time.Sleep(1100 * time.Millisecond) // the pipe's rate limit
	// The person confirms: the same message, force=1, one more call.
	pi1.setAnswer(ops.WriteResult{Outcome: ops.OutcomeDelivered, Forced: true})
	code, out, _ = ph.webDo(t, "POST", base+"/pipe", url.Values{"text": {"original message"}, "force": {"1"}}, ph.asAdmin)
	if code != 200 || !strings.Contains(out, "Delivered (sent anyway).") || sends() != 2 {
		t.Fatalf("forced send = %d %s sends=%d", code, out, sends())
	}
	last := pi1.requests()[len(pi1.requests())-1]
	if last.Op != ops.OpPipeSend || !strings.Contains(string(last.Args), `"force":true`) || !strings.Contains(string(last.Args), "original message") {
		t.Errorf("the forced request was %s %s", last.Op, last.Args)
	}

	// An unknown outcome of the Worker is passed on for the page to keep; it is not retried.
	time.Sleep(1100 * time.Millisecond)
	pi1.setAnswer(ops.WriteResult{Outcome: ops.OutcomeUnknown, Reason: "worker_lost"})
	n := sends()
	code, out, hdr = ph.webDo(t, "POST", base+"/pipe", url.Values{"text": {"keep me"}}, ph.asAdmin)
	if code != 200 || !strings.Contains(hdr.Get("HX-Trigger"), `"minion-input-unknown":{"outcome":"unknown","reason":"worker_lost"}`) || !strings.Contains(out, "keep me") || sends() != n+1 {
		t.Fatalf("unknown = %d trigger=%q sends=%d (was %d)", code, hdr.Get("HX-Trigger"), sends(), n)
	}

	// The budget (a live setting) is the broker's: the page shows the refusal and nothing reaches the Worker.
	pi1.setAnswer(ops.WriteResult{})
	if _, err := ph.settings.Set(context.Background(), SettingsActor{ID: ph.admin.ID},
		ops.SettingsSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Key: config.KeyPipeBudgetPerDay, Value: "1"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	n = sends()
	code, out, hdr = ph.webDo(t, "POST", base+"/pipe", url.Values{"text": {"one too many"}}, ph.asAdmin)
	if strings.Contains(hdr.Get("HX-Trigger"), "minion-input-sent") || sends() != n || !strings.Contains(out, "result-danger") {
		t.Fatalf("over the budget = %d %s trigger=%q sends=%d (was %d)", code, out, hdr.Get("HX-Trigger"), sends(), n)
	}
	if !strings.Contains(out, string(proto.CodePipeBudgetExceeded)) && !strings.Contains(strings.ToLower(out), "budget") {
		t.Errorf("the refusal does not say why: %s", out)
	}
}

func TestWebConversationAuthorizationAndCanonicalTarget(t *testing.T) {
	ph := startPipeHub(t, nil, nil)
	pi1 := ph.connectShared(t, "pi-1")
	peer := pipe.Peer{WorkerID: pi1.id, SessionName: "minion-1"}
	ctx := context.Background()
	if _, err := ph.st.Pipe().AppendMessage(ctx, pipe.Message{Peer: peer, Text: "private to bob", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// A second user who does not own pi-1: no read, no send, no pipe_send call, whatever the address.
	id := store.NewID()
	if _, err := ph.st.DB().Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, ?, 1, 1)`, id, "mallory"); err != nil {
		t.Fatal(err)
	}
	row, err := ph.st.UserByIdentifier(ctx, "mallory")
	if err != nil {
		t.Fatal(err)
	}
	is, err := ph.mgr.Create(ctx, row, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mal := asSession(is.Token, is.CSRFToken)
	before := len(pi1.requests())
	for _, path := range []string{"/s/pi-1/minion-1/pipe/turns", "/s/" + pi1.id + "/minion-1/pipe/turns", "/s/" + pi1.id + "/minion-1"} {
		code, body, _ := ph.webDo(t, "GET", path, nil, mal)
		if code == 200 || strings.Contains(body, "private to bob") {
			t.Errorf("mallory read %s: %d %s", path, code, body)
		}
	}
	for _, path := range []string{"/s/pi-1/minion-1/pipe", "/s/" + pi1.id + "/minion-1/pipe"} {
		_, body, _ := ph.webDo(t, "POST", path, url.Values{"text": {"hijack"}}, mal)
		if strings.Contains(body, "Delivered") {
			t.Errorf("mallory sent through %s: %s", path, body)
		}
	}
	if len(pi1.requests()) != before {
		t.Errorf("a request of a stranger reached the Worker: %+v", pi1.requests()[before:])
	}
	// No session at all.
	// (An htmx request without a session is answered with a redirect header to sign in and no body.)
	if code, body, hdr := ph.webDo(t, "GET", "/s/pi-1/minion-1/pipe/turns", nil, nil); strings.Contains(body, "private to bob") || strings.Contains(body, "data-turn") ||
		(code == 200 && hdr.Get("HX-Redirect") == "") {
		t.Errorf("anonymous read: %d %s (HX-Redirect %q)", code, body, hdr.Get("HX-Redirect"))
	}
	if code, _, _ := ph.webDo(t, "POST", "/s/pi-1/minion-1/pipe", url.Values{"text": {"x"}}, nil); code == 200 && len(pi1.requests()) != before {
		t.Error("an anonymous post reached the Worker")
	}
	// The owner reads it, and a send names the canonical Worker id and session, never a display name.
	if _, body, _ := ph.webDo(t, "GET", "/s/pi-1/minion-1/pipe/turns", nil, ph.asAdmin); !strings.Contains(body, "private to bob") {
		t.Errorf("the owner cannot read their own conversation: %s", body)
	}
	ph.webDo(t, "POST", "/s/pi-1/minion-1/pipe", url.Values{"text": {"canonical"}}, ph.asAdmin)
	var found bool
	for _, rq := range pi1.requests() {
		if rq.Op == ops.OpPipeSend && strings.Contains(string(rq.Args), `"session_name":"minion-1"`) && strings.Contains(string(rq.Args), pi1.id) {
			found = true
		}
	}
	if !found {
		t.Errorf("no pipe_send with the canonical target: %+v", pi1.requests())
	}
}

// A person in the browser reads the stored turns of the session they own, including a reply whose source a model would not be shown: the
// page has no trust control of its own and does not make up a conversation, it shows what the broker returns to a person.
func TestWebConversationShowsStoredTurnsToThePersonWhoOwnsTheSession(t *testing.T) {
	ph := startPipeHub(t, nil, nil)
	pi1 := ph.connectShared(t, "pi-1")
	peer := pipe.Peer{WorkerID: pi1.id, SessionName: "minion-1"}
	ctx := context.Background()
	m, err := ph.st.Pipe().AppendMessage(ctx, pipe.Message{Peer: peer, Text: "ask", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	pi1.emit(t, "minion-1", m.ID, "a reply")
	_, body, _ := ph.webDo(t, "GET", "/s/pi-1/minion-1/pipe/turns", nil, ph.asAdmin)
	if !strings.Contains(body, "a reply") || !strings.Contains(body, "ask") || strings.Contains(body, "withheld") {
		t.Errorf("the person cannot read the stored turns: %s", body)
	}
}

// ---- S7.5 review 1 closures, against the real Hub and the real broker (only the Worker side of the wire is a fake) ----

var walkMarker = regexp.MustCompile(`data-pipe-walk="([^"]*)"( data-pipe-behind="1")?`)

// followToNewest does what the browser component does with a long history: it reads the route, and while the answer says it is
// behind it asks again with the cursor the answer names. It returns the answers in order. Every answer is checked to say honestly
// whether it is the newest.
func (ph *pipeHub) followToNewest(t *testing.T, path string, first string) []string {
	t.Helper()
	var answers []string
	body := first
	for step := 0; ; step++ {
		answers = append(answers, body)
		m := walkMarker.FindStringSubmatch(body)
		behind := m != nil && m[2] != ""
		if behind != strings.Contains(body, `data-pipe-note="behind"`) {
			t.Fatalf("step %d: the behind marker and the behind notice disagree", step)
		}
		if !behind {
			return answers
		}
		if step > 20 || m[1] == "" {
			t.Fatalf("step %d: the read is behind and does not move forward (cursor %q)", step, m[1])
		}
		code, next, _ := ph.webDo(t, "GET", path+"?walk="+url.QueryEscape(m[1]), nil, ph.asAdmin)
		if code != 200 {
			t.Fatalf("step %d: %d %s", step, code, next)
		}
		body = next
	}
}

// S75-R1-1: the newest messages of a history longer than one render's page bound are shown, eventually and honestly, on
// the full page, the compact view and the live route; the shown turns are the newest, bounded, in commit order.
func TestWebConversationShowsTheNewestTurnsOfALongHistory(t *testing.T) {
	ph := startPipeHub(t, nil, nil)
	pi1 := ph.connectShared(t, "pi-1")
	peer := pipe.Peer{WorkerID: pi1.id, SessionName: "minion-1"}
	ctx := context.Background()
	const total = 2500 // more than two renders' worth of pages
	for i := 1; i <= total; i++ {
		if _, err := ph.st.Pipe().AppendMessage(ctx, pipe.Message{Peer: peer, Text: fmt.Sprintf("MSG-%05d", i), At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/s/pi-1/minion-1", "/s/pi-1/minion-1?embed=1", "/s/pi-1/minion-1/pipe/turns"} {
		code, body, _ := ph.webDo(t, "GET", path, nil, ph.asAdmin)
		if code != 200 {
			t.Fatalf("%s: %d", path, code)
		}
		if !strings.Contains(body, `data-pipe-behind="1"`) || !strings.Contains(body, "The turns shown are not the newest yet") {
			t.Errorf("%s: the first answer does not say it is behind (it stopped at the page bound)", path)
		}
		if strings.Contains(body, fmt.Sprintf("MSG-%05d", total)) {
			t.Errorf("%s: the first bounded answer already holds the newest; the test does not exercise the steps", path)
		}
		route := "/s/pi-1/minion-1/pipe/turns"
		answers := ph.followToNewest(t, route, body)
		if len(answers) < 2 || len(answers) > 6 {
			t.Errorf("%s: %d steps for %d turns", path, len(answers), total)
		}
		final := answers[len(answers)-1]
		if !strings.Contains(final, fmt.Sprintf("MSG-%05d", total)) {
			t.Errorf("%s: the newest message is never shown (final answer after %d steps)", path, len(answers))
		}
		if n := strings.Count(final, `data-turn="`); n > 200 || n < 150 {
			t.Errorf("%s: the final answer holds %d turns, want a bounded window of at most 200", path, n)
		}
		if !strings.Contains(final, "Older turns exist and are not shown.") || strings.Contains(final, `data-pipe-note="behind"`) {
			t.Errorf("%s: the final answer does not state what it omits", path)
		}
		// Commit order and no gap: the shown messages are consecutive and ascending.
		idx := regexp.MustCompile(`MSG-(\d{5})`).FindAllStringSubmatch(final, -1)
		for i := 1; i < len(idx); i++ {
			a, _ := strconv.Atoi(idx[i-1][1])
			b, _ := strconv.Atoi(idx[i][1])
			if b != a+1 {
				t.Errorf("%s: the shown turns are not consecutive in commit order: %d then %d", path, a, b)
				break
			}
		}
	}
	// The cheap cursor of a complete read reads exactly the shown turns and what is newer, and a later message shows at once.
	_, first, _ := ph.webDo(t, "GET", "/s/pi-1/minion-1/pipe/turns", nil, ph.asAdmin)
	final := ph.followToNewest(t, "/s/pi-1/minion-1/pipe/turns", first)
	m := walkMarker.FindStringSubmatch(final[len(final)-1])
	if m == nil || m[1] == "" {
		t.Fatalf("a complete read of a long history gives no cursor for the next one")
	}
	if _, err := ph.st.Pipe().AppendMessage(ctx, pipe.Message{Peer: peer, Text: "MSG-NEWER", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, cheap, _ := ph.webDo(t, "GET", "/s/pi-1/minion-1/pipe/turns?walk="+url.QueryEscape(m[1]), nil, ph.asAdmin)
	if !strings.Contains(cheap, "MSG-NEWER") || !strings.Contains(cheap, fmt.Sprintf("MSG-%05d", total)) || strings.Contains(cheap, `data-pipe-behind`) {
		t.Errorf("the cheap read from the cursor is not complete: %s", cheap)
	}
	// A cursor that is not valid, or that belongs to nothing, falls back to the plain read; it never errors the view.
	for _, bad := range []string{"not-an-id", "01a1c74f-e887-7d73-8000-0000deadbeef"} {
		if code, body, _ := ph.webDo(t, "GET", "/s/pi-1/minion-1/pipe/turns?walk="+bad, nil, ph.asAdmin); code != 200 || !strings.Contains(body, `data-turn="`) {
			t.Errorf("cursor %q: %d", bad, code)
		}
	}
	// A cursor of another session is nothing here either.
	pi2 := ph.connectShared(t, "pi-2")
	other := pipe.Peer{WorkerID: pi2.id, SessionName: "minion-1"}
	om, err := ph.st.Pipe().AppendMessage(ctx, pipe.Message{Peer: other, Text: "OTHER-SESSION-SECRET", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, body, _ := ph.webDo(t, "GET", "/s/pi-1/minion-1/pipe/turns", nil, ph.asAdmin); strings.Contains(body, "OTHER-SESSION-SECRET") || om.ID == 0 {
		t.Error("another session's turn is shown")
	}
}

// S75-R1-2: a message the broker kept with an unknown or partial outcome is shown in the history without any claim of delivery, in
// the live read, in a fresh render of the page and after a reload; the unknown result itself stays outside the list and the message
// is not sent again by showing it.
func TestWebConversationUnknownOutcomeIsNeverShownAsDelivered(t *testing.T) {
	for _, outcome := range []ops.Outcome{ops.OutcomeUnknown, ops.OutcomePartial} {
		ph := startPipeHub(t, nil, nil)
		pi1 := ph.connectShared(t, "pi-1")
		pi1.setAnswer(ops.WriteResult{Outcome: outcome, Reason: "worker_lost"})
		code, out, hdr := ph.webDo(t, "POST", "/s/pi-1/minion-1/pipe", url.Values{"text": {"UNKNOWN-REVIEW"}}, ph.asAdmin)
		if code != 200 || strings.Contains(out, "Delivered") || !strings.Contains(hdr.Get("HX-Trigger"), `"minion-input-unknown"`) {
			t.Fatalf("%s send: %d %s %v", outcome, code, out, hdr)
		}
		sends := func() int {
			n := 0
			for _, rq := range pi1.requests() {
				if rq.Op == ops.OpPipeSend {
					n++
				}
			}
			return n
		}
		for _, path := range []string{"/s/pi-1/minion-1/pipe/turns", "/s/pi-1/minion-1", "/s/pi-1/minion-1?embed=1"} {
			for round := 0; round < 2; round++ { // and again: a reload, a poll
				_, body, _ := ph.webDo(t, "GET", path, nil, ph.asAdmin)
				if !strings.Contains(body, "UNKNOWN-REVIEW") {
					t.Fatalf("%s %s: the kept message is missing: %s", outcome, path, body)
				}
				for _, bad := range []string{"sent to the agent", "Delivered", "Message sent", "confirmed"} {
					if strings.Contains(body, bad) {
						t.Errorf("%s %s: a delivery is claimed (%q): %s", outcome, path, bad, body)
					}
				}
				if !strings.Contains(body, "Your message to the agent") {
					t.Errorf("%s %s: the message is not labelled neutrally", outcome, path)
				}
			}
		}
		if n := sends(); n != 1 {
			t.Errorf("%s: showing the history made %d pipe_send calls, want exactly the one", outcome, n)
		}
	}
}

// S75 live closure. The browser conversation is woken by the view's one authenticated event stream (GET /api/v1/events, the
// same EventSource that follows the screen), not by a stream of its own: pipe_wait is turns-only, so it never saw a message that
// another client committed. The earlier test of this place (S75-R1-3) drove a REST pipe_wait NDJSON stream; the transport
// expectation changed because that stream was the wrong implementation: it could not carry another client's message, and it was
// a second watcher per page. What the test still asserts of the earlier one: a committed reply reaches the browser within 2 s
// (the poll fallback is 30 s), the page's read afterwards shows exactly the stored reply, a person who may not read the pipe
// gets nothing of it, and nothing of the conversation travels on the wake-up.

// sseEvent is one event of a server-sent events stream.
type sseEvent struct{ name, data string }

// sseStream is an open GET /api/v1/events of one person; every event arrives on ch, and raw keeps every byte read.
type sseStream struct {
	ch   chan sseEvent
	mu   sync.Mutex
	raw  strings.Builder
	stop context.CancelFunc
}

func (s *sseStream) rawText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.raw.String()
}

// openSSE opens the event stream as the given session and returns after its snapshot event.
func (ph *pipeHub) openSSE(t *testing.T, as map[string]string, query string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", ph.hc.base+"/api/v1/events"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range as {
		req.Header.Set(k, v)
	}
	resp, err := ph.hc.c.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		cancel()
		t.Fatalf("event stream: %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	s := &sseStream{ch: make(chan sseEvent, 64), stop: cancel}
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			s.mu.Lock()
			s.raw.WriteString(line + "\n")
			s.mu.Unlock()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.name != "":
				s.ch <- ev
				ev = sseEvent{}
			}
		}
	}()
	t.Cleanup(cancel)
	s.expect(t, "snapshot", 5*time.Second)
	return s
}

// expect waits for an event of the given name (others are skipped) and returns its data.
func (s *sseStream) expect(t *testing.T, name string, within time.Duration) string {
	t.Helper()
	timer := time.After(within)
	for {
		select {
		case ev := <-s.ch:
			if ev.name == name {
				return ev.data
			}
		case <-timer:
			t.Fatalf("no %s event within %s; stream so far:\n%s", name, within, s.rawText())
		}
	}
}

// none asserts that no event of the given name arrives within d.
func (s *sseStream) none(t *testing.T, name string, d time.Duration) {
	t.Helper()
	timer := time.After(d)
	for {
		select {
		case ev := <-s.ch:
			if ev.name == name {
				t.Fatalf("unexpected %s event: %s", name, ev.data)
			}
		case <-timer:
			return
		}
	}
}

// checkPipeChanged asserts that data is the metadata-only wake-up of the session and nothing else.
func checkPipeChanged(t *testing.T, data, workerID, session string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatalf("pipe_changed data is not JSON: %q", data)
	}
	if m["type"] != "pipe_changed" || m["worker_id"] != workerID || m["session"] != session {
		t.Errorf("pipe_changed = %v", m)
	}
	for k := range m {
		switch k {
		case "type", "worker_id", "session", "time":
		case "worker": // the event type has the member; the wake-up leaves it empty
			if m[k] != "" {
				t.Errorf("pipe_changed names the Worker: %s", data)
			}
		default:
			t.Errorf("pipe_changed carries the member %q: %s", k, data)
		}
	}
}

// Another client's committed message and a committed reply wake the browser's own event stream at once; a replayed request is
// not announced again; a delivery that was refused before it reached the session is not announced; nothing of the text, the
// turn ids or the policy travels; the read that follows is the authorised one.
func TestWebConversationOtherClientsMessageAndReplyReachTheBrowsersEventStream(t *testing.T) {
	ph := startPipeHub(t, nil, nil)
	pi1 := ph.connectShared(t, "pi-1")
	base := "/s/" + pi1.id + "/minion-1"

	// The page names its read route only: no stream of its own and no pipe_wait.
	_, page, _ := ph.webDo(t, "GET", base, nil, ph.asAdmin)
	if !strings.Contains(page, `data-pipe-read="`) {
		t.Errorf("the page has no read route")
	}
	for _, bad := range []string{"data-pipe-live", "pipe_wait", "data-pipe-worker"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page still names its own live wait: %s", bad)
		}
	}
	sse := ph.openSSE(t, ph.asAdmin, "")

	// 1. A message committed by ANOTHER client (the REST API, as a script or an agent would send it).
	start := time.Now()
	code, out := ph.rest(t, "pipe_send", map[string]any{"target": map[string]any{"worker_id": pi1.id, "session_name": "minion-1"},
		"text": "OTHER-CLIENT-SECRET-MESSAGE", "request_id": ops.NewUUIDv7()})
	if code != 200 {
		t.Fatalf("pipe_send = %d %s", code, out)
	}
	data := sse.expect(t, "pipe_changed", 3*time.Second)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the message took %s to wake the stream (the poll fallback is 30 s)", d)
	}
	checkPipeChanged(t, data, pi1.id, "minion-1")
	if _, body, _ := ph.webDo(t, "GET", base+"/pipe/turns", nil, ph.asAdmin); !strings.Contains(body, "OTHER-CLIENT-SECRET-MESSAGE") {
		t.Errorf("the read after the wake-up lacks the other client's message: %s", body)
	}
	sse.none(t, "pipe_changed", 300*time.Millisecond) // exactly one for one message

	// 2. The same request again (an idempotent replay) commits nothing and announces nothing.
	rid := ops.NewUUIDv7()
	send := func() int {
		c, _ := ph.rest(t, "pipe_send", map[string]any{"target": map[string]any{"worker_id": pi1.id, "session_name": "minion-1"}, "text": "once", "request_id": rid})
		return c
	}
	if send() != 200 {
		t.Fatal("the send failed")
	}
	sse.expect(t, "pipe_changed", 3*time.Second)
	if send() != 200 {
		t.Fatal("the replay failed")
	}
	sse.none(t, "pipe_changed", 500*time.Millisecond)

	// 3. A delivery the Worker reports as not sent leaves nothing behind and announces nothing.
	pi1.setAnswer(ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: string(proto.CodeInternal)})
	ph.rest(t, "pipe_send", map[string]any{"target": map[string]any{"worker_id": pi1.id, "session_name": "minion-1"}, "text": "NEVER-STORED", "request_id": ops.NewUUIDv7()})
	sse.none(t, "pipe_changed", 500*time.Millisecond)
	pi1.setAnswer(ops.WriteResult{})

	// 4. A new reply commits: the stream is woken again, with nothing of the reply.
	msgs, _ := ph.st.Pipe().Messages(context.Background(), pipe.Peer{WorkerID: pi1.id, SessionName: "minion-1"}, 0, 0)
	if len(msgs) == 0 {
		t.Fatal("no stored message to answer")
	}
	start = time.Now()
	if ack := pi1.emit(t, "minion-1", msgs[0].ID, "LIVE-REPLY-SECRET"); ack.Status != pipe.AckStored {
		t.Fatalf("ack = %+v", ack)
	}
	data = sse.expect(t, "pipe_changed", 3*time.Second)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the committed reply took %s to wake the stream", d)
	}
	checkPipeChanged(t, data, pi1.id, "minion-1")
	if _, body, _ := ph.webDo(t, "GET", base+"/pipe/turns", nil, ph.asAdmin); !strings.Contains(body, "LIVE-REPLY-SECRET") || strings.Contains(body, "NEVER-STORED") {
		t.Errorf("the read after the reply is not the stored conversation: %s", body)
	}
	sse.none(t, "pipe_changed", 300*time.Millisecond)

	raw := sse.rawText()
	for _, leak := range []string{"OTHER-CLIENT-SECRET-MESSAGE", "LIVE-REPLY-SECRET", "NEVER-STORED", "turn_id", "message_id", "\"text\"", "rule", "unshared"} {
		if strings.Contains(raw, leak) {
			t.Errorf("the event stream carries %q:\n%s", leak, raw)
		}
	}
}

// The wake-up follows the current share authority: a session the Worker does not share (rule not known, then taken away) is
// announced to nobody, and neither is its name; a person who does not own the session gets no pipe_changed and not its name.
func TestWebConversationPipeChangedFailsClosedOnPolicyAndAuthority(t *testing.T) {
	ph := startPipeHub(t, nil, nil)
	ctx := context.Background()
	shared := ph.connectShared(t, "pi-1")
	unknown := ph.connect(t, "pi-2") // declares no rule: the Hub does not know what it shares
	sse := ph.openSSE(t, ph.asAdmin, "")

	// A stranger (a user who owns no Worker) watches the same stream.
	id := store.NewID()
	if _, err := ph.st.DB().Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, ?, 1, 1)`, id, "mallory"); err != nil {
		t.Fatal(err)
	}
	row, err := ph.st.UserByIdentifier(ctx, "mallory")
	if err != nil {
		t.Fatal(err)
	}
	is, err := ph.mgr.Create(ctx, row, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	stranger := ph.openSSE(t, asSession(is.Token, is.CSRFToken), "")

	reply := func(pw *pipeWorker, text string) {
		t.Helper()
		m, err := ph.st.Pipe().AppendMessage(ctx, pipe.Message{Peer: pipe.Peer{WorkerID: pw.id, SessionName: "minion-1"}, Text: "ask " + text, At: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		if ack := pw.emit(t, "minion-1", m.ID, text); ack.Status != pipe.AckStored {
			t.Fatalf("ack = %+v", ack)
		}
	}

	// Allowed: the owner sees the wake-up, the stranger sees nothing of it.
	reply(shared, "r1")
	checkPipeChanged(t, sse.expect(t, "pipe_changed", 3*time.Second), shared.id, "minion-1")
	stranger.none(t, "pipe_changed", 500*time.Millisecond)

	// A Worker whose rule is not known: the reply is stored, nothing is announced (fail closed).
	reply(unknown, "r-unknown")
	sse.none(t, "pipe_changed", 500*time.Millisecond)

	// The owner takes the session away from the share: the next reply is stored and announced to nobody.
	ph.events.ApplyShare(shared.id, []string{"minion-1"})
	reply(shared, "r-after-unshare")
	sse.none(t, "pipe_changed", 700*time.Millisecond)

	for name, st := range map[string]*sseStream{"owner": sse, "stranger": stranger} {
		raw := st.rawText()
		for _, leak := range []string{"r-unknown", "r-after-unshare", "turn_id", `"text"`} {
			if strings.Contains(raw, leak) {
				t.Errorf("%s: the stream carries %q:\n%s", name, leak, raw)
			}
		}
		if strings.Count(raw, "event: pipe_changed") > 1 {
			t.Errorf("%s: more than the one allowed wake-up:\n%s", name, raw)
		}
	}
	if strings.Contains(stranger.rawText(), shared.id) {
		t.Errorf("the stranger's stream names the Worker: %s", stranger.rawText())
	}
}

// S75-R2-1: valid messages whose JSON form is large (8 KiB of '<' expands sixfold) make the real broker's byte budget, not its
// entry limit, cut every page (20 per page). The bounded walk must never replace the shown history with an empty one when it
// reaches the end, whatever the count is relative to the five-page boundary, on every route, and a later refresh is stable.
func TestWebConversationByteBudgetPagesKeepTheNewestAtThePageBoundary(t *testing.T) {
	for _, total := range []int{99, 100, 101, 120, 250} {
		ph := startPipeHub(t, nil, nil)
		pw := ph.connectShared(t, "pi-1")
		peer := pipe.Peer{WorkerID: pw.id, SessionName: "minion-1"}
		for i := 1; i <= total; i++ {
			text := fmt.Sprintf("BYTE-%03d-", i) + strings.Repeat("<", 8180)
			if _, err := ph.st.Pipe().AppendMessage(context.Background(), pipe.Message{Peer: peer, Text: text, At: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
		newest := fmt.Sprintf("BYTE-%03d-", total)
		route := "/s/pi-1/minion-1/pipe/turns"
		for _, path := range []string{"/s/pi-1/minion-1", "/s/pi-1/minion-1?embed=1", route} {
			code, first, _ := ph.webDo(t, "GET", path, nil, ph.asAdmin)
			if code != 200 {
				t.Fatalf("%d %s: %d", total, path, code)
			}
			answers := ph.followToNewest(t, route, first)
			if len(answers) > 6 {
				t.Errorf("%d %s: %d steps", total, path, len(answers))
			}
			for i, body := range answers {
				if strings.Contains(body, "No messages yet.") || strings.Count(body, `data-turn="`) == 0 {
					t.Errorf("%d %s step %d: the shown history was replaced by an empty one", total, path, i)
				}
			}
			final := answers[len(answers)-1]
			if !strings.Contains(final, newest) || strings.Contains(final, `data-pipe-behind`) {
				t.Errorf("%d %s: the final answer does not show %s as the newest", total, path, newest)
			}
			// The shown turns are consecutive and ascending: no gap, no duplicate.
			idx := regexp.MustCompile(`BYTE-(\d{3})-`).FindAllStringSubmatch(final, -1)
			for i := 1; i < len(idx); i++ {
				a, _ := strconv.Atoi(idx[i-1][1])
				b, _ := strconv.Atoi(idx[i][1])
				if b != a+1 {
					t.Errorf("%d %s: turns not consecutive: %d then %d", total, path, a, b)
					break
				}
			}
			// A refresh from the final cursor is stable and a new message shows at once.
			m := walkMarker.FindStringSubmatch(final)
			if m == nil || m[1] == "" {
				t.Fatalf("%d %s: no cursor for the next refresh", total, path)
			}
			_, again, _ := ph.webDo(t, "GET", route+"?walk="+url.QueryEscape(m[1]), nil, ph.asAdmin)
			if !strings.Contains(again, newest) || strings.Contains(again, "No messages yet.") {
				t.Errorf("%d %s: the refresh is not stable", total, path)
			}
		}
		// A cursor at the very newest turn (nothing follows) never answers with an empty conversation.
		_, full, _ := ph.webDo(t, "GET", route, nil, ph.asAdmin)
		ids := regexp.MustCompile(`data-turn="([^"]+)"`).FindAllStringSubmatch(ph.followToNewest(t, route, full)[0], -1)
		last := ids[len(ids)-1][1]
		_, tail, _ := ph.webDo(t, "GET", route+"?walk="+url.QueryEscape(last), nil, ph.asAdmin)
		if strings.Contains(tail, "No messages yet.") || !strings.Contains(tail, `data-turn="`) {
			t.Errorf("%d: a cursor at the tail emptied the conversation", total)
		}
	}
}

// The genuinely empty conversation stays honest, and the newest of 1001 short messages shows with a foreign or hostile cursor ignored.
func TestWebConversationEmptyStaysHonestAndNewestOf1001WithUntrustedCursors(t *testing.T) {
	ph := startPipeHub(t, nil, nil)
	ph.connectShared(t, "pi-1")
	_, body, _ := ph.webDo(t, "GET", "/s/pi-1/minion-1/pipe/turns", nil, ph.asAdmin)
	if !strings.Contains(body, "No messages yet.") {
		t.Errorf("an empty conversation is not said to be empty: %s", body)
	}
	pw := ph.connectShared(t, "pi-3")
	peer := pipe.Peer{WorkerID: pw.id, SessionName: "minion-1"}
	for i := 1; i <= 1001; i++ {
		if _, err := ph.st.Pipe().AppendMessage(context.Background(), pipe.Message{Peer: peer, Text: fmt.Sprintf("LATEST-%04d", i), At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	other := ph.connectShared(t, "pi-2")
	if _, err := ph.st.Pipe().AppendMessage(context.Background(), pipe.Message{Peer: pipe.Peer{WorkerID: other.id, SessionName: "minion-1"}, Text: "OTHER-PEER-PRIVATE", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, otherBody, _ := ph.webDo(t, "GET", "/s/pi-2/minion-1/pipe/turns", nil, ph.asAdmin)
	tid := regexp.MustCompile(`data-turn="([^"]+)"`).FindStringSubmatch(otherBody)
	if len(tid) != 2 {
		t.Fatal("foreign fixture missing id")
	}
	path := "/s/pi-3/minion-1/pipe/turns"
	for _, suffix := range []string{"", "?walk=" + url.QueryEscape(tid[1]), "?walk=" + url.QueryEscape(`<script>FAKE-TRAFFIC</script>`)} {
		code, b, _ := ph.webDo(t, "GET", path+suffix, nil, ph.asAdmin)
		if code != 200 {
			t.Fatalf("code=%d", code)
		}
		answers := ph.followToNewest(t, path, b)
		for _, out := range answers {
			if strings.Contains(out, "OTHER-PEER-PRIVATE") || strings.Contains(out, "FAKE-TRAFFIC") {
				t.Fatal("untrusted cursor disclosed or injected content")
			}
		}
		if !strings.Contains(answers[len(answers)-1], "LATEST-1001") {
			t.Fatal("latest 1001 missing")
		}
	}
}
