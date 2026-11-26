package web

import (
	"net/url"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The Hidden sessions panel: the owner sees and changes the Worker's own
// choice; an administrator changes it on another user's Worker; anybody else
// is refused without a call; the account default and a Hub policy that
// decides for everybody are named.
func TestWorkerHiddenPanel(t *testing.T) {
	d := Dashboard{Workers: []WorkerRow{wPrivate}}
	w := wAliceInfo
	w.SessionsHidden = true
	f := newFakeOps().
		reply(ops.OpWorkersList, ops.WorkersListResult{Workers: []ops.WorkerInfo{w, wBobInfo}}).
		reply(ops.OpAccountPrefs, ops.AccountPrefsResult{HiddenSessions: config.HiddenSessionsUserControlled, AllowHiddenSessions: true}).
		reply(ops.OpWorkerHiddenSessionsSet, ops.WriteResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	s, _ := listServer(t, d, d)

	rec := get(t, s, "/w/w1/hidden-sessions", "X-Test-Session", "alice:user")
	body := rec.Body.String()
	mustHave(t, body, `hx-post="/w/w1/hidden-sessions"`, `value="default" checked`, "for you: allowed by your account setting", "would be hidden")
	if len(f.called(ops.OpWorkerHiddenSessionsSet)) != 0 {
		t.Fatal("a GET changed the choice")
	}

	rec = post(t, s, "/w/w1/hidden-sessions", "alice:user", url.Values{"hidden_sessions": {"refuse"}})
	mustHave(t, rec.Body.String(), "refused on this worker")
	calls := f.called(ops.OpWorkerHiddenSessionsSet)
	if len(calls) != 1 || calls[0].Args["worker_id"] != w.ID || calls[0].Args["hidden_sessions"] != "refuse" || calls[0].Args["request_id"] == "" {
		t.Fatalf("calls = %+v", calls)
	}

	// Bob's Worker: alice may not change it, nothing is called.
	rec = post(t, s, "/w/w2/hidden-sessions", "alice:user", url.Values{"hidden_sessions": {"allow"}})
	mustHave(t, rec.Body.String(), "Only the owner of this worker, or an administrator")
	if len(f.called(ops.OpWorkerHiddenSessionsSet)) != 1 {
		t.Fatal("worker_hidden_sessions_set was called for a person who may not change it")
	}
	// An administrator may.
	rec = post(t, s, "/w/w2/hidden-sessions", "root:admin", url.Values{"hidden_sessions": {"allow"}})
	mustHave(t, rec.Body.String(), "allowed on this worker")
	if calls := f.called(ops.OpWorkerHiddenSessionsSet); len(calls) != 2 || calls[1].Args["worker_id"] != wBobInfo.ID || calls[1].User != "root" {
		t.Fatalf("calls = %+v", calls)
	}

	// A Hub that decides for everybody says so.
	f.reply(ops.OpAccountPrefs, ops.AccountPrefsResult{HiddenSessions: config.HiddenSessionsDisabled})
	rec = get(t, s, "/w/w1/hidden-sessions", "X-Test-Session", "alice:user")
	mustHave(t, rec.Body.String(), "data-hidden-hub-note", "disabled hidden sessions for everybody")
}
