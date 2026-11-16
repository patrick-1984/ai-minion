package web

import (
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestGrantReaches(t *testing.T) {
	w, other := "w1", "w2"
	for _, tc := range []struct {
		name    string
		targets []ops.KeyResource
		want    bool
	}{
		{"everything", []ops.KeyResource{{Kind: ops.KeyResourceEverything}}, true},
		{"whole worker", []ops.KeyResource{{Kind: ops.KeyResourceWorker, WorkerID: w}}, true},
		{"that session", []ops.KeyResource{{Kind: ops.KeyResourceSession, WorkerID: w, SessionName: "build"}}, true},
		{"other session", []ops.KeyResource{{Kind: ops.KeyResourceSession, WorkerID: w, SessionName: "x"}}, false},
		{"other worker", []ops.KeyResource{{Kind: ops.KeyResourceWorker, WorkerID: other}}, false},
		{"same session name on another worker", []ops.KeyResource{{Kind: ops.KeyResourceSession, WorkerID: other, SessionName: "build"}}, false},
		{"none", nil, false},
	} {
		if got := grantReaches(tc.targets, w, "build"); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An admin sees, per session, the controller sessions that may control it; a
// user sees neither the column nor a session_control_list call.
func TestSharePanelShowsControllersToAdminsOnly(t *testing.T) {
	grants := ops.SessionControlListResult{Grants: []ops.SessionControlInfo{
		{WorkerID: "w1", SessionName: "boss", Targets: []ops.KeyResource{{Kind: ops.KeyResourceSession, WorkerID: "w1", SessionName: "shared-2"}}},
		{WorkerID: "w1", SessionName: "ops", Targets: []ops.KeyResource{{Kind: ops.KeyResourceEverything}}},
	}}
	s, f := shareServer(t, someRule, func(f *fakeOps) { f.reply(ops.OpSessionControlList, grants) })
	body := get(t, s, "/w/w1/share", "X-Test-Session", "root:admin").Body.String()
	mustHave(t, body, "Controllable from", `data-share-bulk="all"`, `data-share-bulk="none"`)
	row := body[strings.Index(body, `<tr data-session="shared-2">`):]
	row = row[:strings.Index(row, "</tr>")]
	if !strings.Contains(row, ": boss") || !strings.Contains(row, ": ops") {
		t.Errorf("shared-2 row lacks its controllers: %s", row)
	}
	row1 := body[strings.Index(body, `<tr data-session="shared-1">`):]
	row1 = row1[:strings.Index(row1, "</tr>")]
	if strings.Contains(row1, ": boss") || !strings.Contains(row1, ": ops") {
		t.Errorf("shared-1 row has the wrong controllers: %s", row1)
	}

	calls := len(f.called(ops.OpSessionControlList))
	body = get(t, s, "/w/w1/share", "X-Test-Session", "alice:user").Body.String()
	mustNotHave(t, body, "Controllable from")
	if len(f.called(ops.OpSessionControlList)) != calls {
		t.Fatal("a user's panel read the session-control grants")
	}
}
