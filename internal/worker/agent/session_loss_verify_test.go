package agent

import "testing"

// Verification of "a lost session loses its permission" (owner question,
// v0.22.0). The Worker reports sessions by name only: a tmux session that is
// killed and recreated under the same name between two polls (default 1s,
// configurable up to 1m), including a tmux server restart that recreates the
// name, produces no session_removed / session_added pair. The Hub then never
// sees a new generation of the name, so its first-seen fence and its purge of
// name-only (Claimed) master nominations and grants do not fire.
// term.Session.Instance (boot id, server pid and start time, session id)
// identifies the terminal but is not part of SessionState. This test asserts
// the current, unwanted behaviour (Gap): a fix makes it fail.
func TestVerifyLossRecreateWithinOnePollIsInvisibleGap(t *testing.T) {
	old := View{"boss": {Name: "boss", Kind: "agent:claude", State: "idle", Task: "t"}}
	recreated := View{"boss": {Name: "boss", Kind: "agent:claude", State: "idle", Task: "t"}}
	if ch := Diff(old, recreated); len(ch) != 0 {
		t.Fatalf("a recreated session is now reported (gap fixed?): %+v", ch)
	}
}
