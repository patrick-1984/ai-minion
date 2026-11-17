package web

// v0.10.0: the session host of a Worker, the disabled New session button when
// a new session would be hidden, and the Sessions card of the account page.

import (
	"bytes"
	"net/url"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

const refusedText = "New sessions on asus would be hidden (ConPTY inside the Worker: psmux is not installed (winget install psmux)). Install psmux (winget install psmux) and restart the Worker with `minion worker up --foreground`, or allow hidden sessions in your settings (Account > Sessions)."

var (
	wHidden = WorkerRow{ID: "w7", Name: "asus", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true, Version: "3.0",
		SessionHost: "conpty", SessionsHidden: true, SessionHostReason: "ConPTY inside the Worker: psmux is not installed (winget install psmux)",
		NewSessionRefused: refusedText}
	wVisible = WorkerRow{ID: "w8", Name: "desk", Owner: "alice", Kind: ops.WorkerKindPrivate, Online: true, Version: "3.0",
		SessionHost: "psmux", SessionHostReason: "the default psmux namespace of alice (psmux ls shows them)"}
)

func TestSessionHostText(t *testing.T) {
	for _, tc := range []struct {
		w    WorkerRow
		want string
	}{
		{wHidden, "ConPTY inside the Worker (hidden): psmux is not installed (winget install psmux)"},
		{wVisible, "psmux (visible): the default psmux namespace of alice (psmux ls shows them)"},
		{WorkerRow{SessionHost: "tmux", SessionsHidden: true, SessionHostReason: "private tmux socket /x/y"}, "tmux (hidden): private tmux socket /x/y"},
		{WorkerRow{SessionHost: "tmux"}, "tmux (visible)"},
		{WorkerRow{}, ""},
	} {
		if got := tc.w.SessionHostText(); got != tc.want {
			t.Errorf("SessionHostText(%+v) = %q, want %q", tc.w, got, tc.want)
		}
	}
}

func TestWorkersListDisablesNewSessionWhereItWouldBeHidden(t *testing.T) {
	v := newDashboardView(Dashboard{Workers: []WorkerRow{wHidden, wVisible}}, false)
	var buf bytes.Buffer
	if err := fragmentTemplates.ExecuteTemplate(&buf, "workers_results", v); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	mustHave(t, body, "Sessions: ConPTY inside the Worker (hidden): psmux is not installed",
		"Sessions: psmux (visible)", "data-hidden-refused", `disabled aria-describedby="nsr-w7"`,
		"New sessions on asus would be hidden", "Account &gt; Sessions", `hx-post="/w/desk/sessions"`)
	mustNotHave(t, body, `hx-post="/w/asus/sessions"`)
}

func TestWorkerPageShowsTheSessionHostAndTheRefusal(t *testing.T) {
	d := Dashboard{Workers: []WorkerRow{wHidden, wVisible}}
	out, err := renderWorkerFragment(newWorkerView(d, false, "w7", true))
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, out, "<dt>Sessions</dt>", "ConPTY inside the Worker (hidden): psmux is not installed",
		"data-hidden-refused", "winget install psmux", "minion worker up --foreground")
	mustNotHave(t, out, `hx-post="/w/w7/sessions"`)
	out, err = renderWorkerFragment(newWorkerView(d, false, "w8", true))
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, out, "psmux (visible)", `hx-post="/w/w8/sessions"`)
	mustNotHave(t, out, "data-hidden-refused")
	// An older Worker does not report its host.
	old := Dashboard{Workers: []WorkerRow{wPrivate}}
	out, _ = renderWorkerFragment(newWorkerView(old, false, "w1", true))
	mustHave(t, out, "not reported (Worker older than v0.10.0)")
}

func TestAccountSessionsCardReadsAndSavesThePreference(t *testing.T) {
	fake := newFakeOps().
		reply(ops.OpAccountPrefs, ops.AccountPrefsResult{HiddenSessions: config.HiddenSessionsUserControlled}).
		reply(ops.OpAccountPrefsSet, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, fake)
	s := adminServer(t)
	body := get(t, s, "/account", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `<h2 id="sessions-h">Sessions</h2>`, `name="allow_hidden_sessions"`, `hx-post="/account/sessions"`,
		"New sessions that would be hidden are refused")
	mustNotHave(t, body, `id="allow-hidden-sessions" checked`)

	mustHave(t, body, `name="for_user" value="u-bob"`)

	// A form rendered for another effective user (an act-as that started or
	// ended since) is refused, nothing is saved.
	rec := post(t, s, "/account/sessions", userHeader, url.Values{"allow_hidden_sessions": {"on"}, "for_user": {"u-alice"}})
	if !strings.Contains(rec.Body.String(), "opened for another account") || len(fake.called(ops.OpAccountPrefsSet)) != 0 {
		t.Fatalf("a post for another account = %d %s", rec.Code, rec.Body.String())
	}
	rec = post(t, s, "/account/sessions", userHeader, url.Values{"allow_hidden_sessions": {"on"}, "for_user": {"u-bob"}})
	if rec.Code != 200 {
		t.Fatalf("POST /account/sessions = %d %s", rec.Code, rec.Body.String())
	}
	calls := fake.called(ops.OpAccountPrefsSet)
	if len(calls) != 1 || calls[0].Args["allow_hidden_sessions"] != true {
		t.Fatalf("account_prefs_set calls = %+v", calls)
	}
	post(t, s, "/account/sessions", userHeader, url.Values{"for_user": {"u-bob"}})
	if calls := fake.called(ops.OpAccountPrefsSet); len(calls) != 2 || calls[1].Args["allow_hidden_sessions"] != false {
		t.Fatalf("an unchecked box must store false: %+v", calls)
	}

	// The Hub setting decides for everybody: the card says so.
	fake.reply(ops.OpAccountPrefs, ops.AccountPrefsResult{AllowHiddenSessions: true, HiddenSessions: config.HiddenSessionsDisabled})
	body = get(t, s, "/account", "X-Test-Session", userHeader).Body.String()
	mustHave(t, body, `id="allow-hidden-sessions" checked`, "has disabled hidden sessions for everybody")
	if !strings.Contains(body, "hidden_sessions") {
		t.Error("the card does not name the Hub setting")
	}
}

// While an admin acts as someone, the card shows that person's choice
// without a form, and a post (from a tab opened before) is refused.
func TestAccountSessionsCardIsReadOnlyWhileActingAs(t *testing.T) {
	fake := newFakeOps().
		reply(ops.OpAccountPrefs, ops.AccountPrefsResult{HiddenSessions: config.HiddenSessionsUserControlled}).
		reply(ops.OpAccountPrefsSet, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, fake)
	s := adminServer(t)
	const actAs = "root:admin:actas-bob"
	body := get(t, s, "/account", "X-Test-Session", actAs).Body.String()
	mustHave(t, body, "data-prefs-readonly", "this choice cannot be changed")
	mustNotHave(t, body, `name="allow_hidden_sessions"`, `name="for_user"`)
	rec := post(t, s, "/account/sessions", actAs, url.Values{"allow_hidden_sessions": {"on"}, "for_user": {"u-bob"}})
	if !strings.Contains(rec.Body.String(), "act as another user") || len(fake.called(ops.OpAccountPrefsSet)) != 0 {
		t.Fatalf("a post while acting as = %d %s", rec.Code, rec.Body.String())
	}
}

// An online Worker without sessions says why: no list yet, a failed listing,
// or an empty list.
func TestWorkerPageExplainsAnEmptySessionList(t *testing.T) {
	for _, tc := range []struct {
		name   string
		w      WorkerRow
		reason string
		want   string
	}{
		{"waiting", WorkerRow{ID: "w1", Name: "a", Online: true}, "waiting", "Waiting for the first session list"},
		{"failed", WorkerRow{ID: "w1", Name: "a", Online: true, SessionsListError: "psmux: server not running"}, "failed", "As of its last connection to the Hub, the worker could not list its sessions: psmux: server not running"},
		{"none", WorkerRow{ID: "w1", Name: "a", Online: true, SessionsSynced: true, SessionsListError: "stale"}, "none", "No visible sessions on this worker"},
		{"offline", WorkerRow{ID: "w1", Name: "a"}, "", "The worker is offline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newWorkerView(Dashboard{Workers: []WorkerRow{tc.w}}, false, "w1", true)
			if v.EmptyReason() != tc.reason {
				t.Fatalf("EmptyReason = %q, want %q", v.EmptyReason(), tc.reason)
			}
			out, err := renderWorkerFragment(v)
			if err != nil {
				t.Fatal(err)
			}
			mustHave(t, out, tc.want)
		})
	}
}
