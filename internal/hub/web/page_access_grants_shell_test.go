package web

import (
	"net/url"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// shellFixture is grantFixture with g1 a Control grant that marks wb-3 as a
// command shell of wb-4 (A116).
func shellFixture(marked bool) ops.GrantListResult {
	f := grantFixture()
	f.Grants[0].Level, f.Grants[0].CommandShell = ops.GrantLevelControl, marked
	return f
}

func TestAccessGrantRowShowsAndTogglesTheCommandShell(t *testing.T) {
	connect(t, newFakeOps().reply(ops.OpGrantList, shellFixture(true)))
	body := get(t, adminServer(t), pathAccessGrants, "X-Test-Session", ownerHeader).Body.String()
	mustHave(t, body, `id="grant-g1-shell"`, ">Command shell</span>",
		"Command shell: typing here from wb-4 is allowed without force.",
		`hx-post="/access/grants/set-shell"`, `name="command_shell" value="0"`, "Unmark command shell")
	// A grant on every session of a Worker offers no mark.
	mustNotHave(t, body, `id="grant-g2-shell-toggle"`)

	f := newFakeOps().reply(ops.OpGrantList, shellFixture(false)).reply(ops.OpGrantSet, ops.GrantSetResult{Outcome: ops.OutcomeDelivered})
	connect(t, f)
	rec := post(t, adminServer(t), pathAccessGrants+"/set-shell", ownerHeader, url.Values{"id": {"g1"}, "if_revision": {"3"}, "command_shell": {"1"}})
	calls := f.called(ops.OpGrantSet)
	if len(calls) != 1 || calls[0].Args["id"] != "g1" || calls[0].Args["command_shell"] != true || calls[0].Args["level"] != "control" ||
		calls[0].Args["target_session"] != "wb-3" || calls[0].Args["if_revision"] != float64(3) {
		t.Fatalf("calls = %+v", calls)
	}
	mustHave(t, rec.Body.String(), `<tr id="grant-g1"`, `role="status">Saved</p>`)

	// A View grant cannot become a command shell: nothing is sent.
	f = newFakeOps().reply(ops.OpGrantList, grantFixture())
	connect(t, f)
	rec = post(t, adminServer(t), pathAccessGrants+"/set-shell", ownerHeader, url.Values{"id": {"g1"}, "if_revision": {"3"}, "command_shell": {"1"}})
	if len(f.called(ops.OpGrantSet)) != 0 {
		t.Fatal("a View grant was marked")
	}
	mustHave(t, rec.Body.String(), "only a Control grant on one named session can mark a command shell")
}
