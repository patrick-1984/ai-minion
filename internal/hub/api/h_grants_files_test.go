package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// v0.26.0: the boxes of the Add grant form are saved with the grant in one
// step, with no bound source: the two cases of the owner's report (every
// shared session of the target Worker, and one named target session), the
// install box alone, a change that does not name them keeps them, an
// explicit false clears one, a lower level clears push and install, and the
// create is audited with them.
func TestGrantFileFlags(t *testing.T) {
	k := newGrantStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.u1)
	// Screenshot 1: every shared session of the target Worker, Control,
	// receive and send files, no end, a note.
	wide := grantArgs(a1, a2, ops.GrantLevelControl)
	delete(wide, "target_session")
	wide["files_pull"], wide["files_push"], wide["note"] = true, true, "to android"
	gw := k.setGrant(wide)
	if !gw.FilesPull || !gw.FilesPush || gw.AppsInstall || gw.TargetSession != "" || gw.SourceScope != ops.GrantSourceSession {
		t.Fatalf("screenshot 1 = %+v", gw)
	}
	// Screenshot 2: the same with one named target session.
	named := grantArgs(a1, a2, ops.GrantLevelControl)
	named["files_pull"], named["files_push"], named["note"] = true, true, "to android"
	g := k.setGrant(named)
	if !g.FilesPull || !g.FilesPush || g.TargetSession != "build" {
		t.Fatalf("screenshot 2 = %+v", g)
	}
	if ev := k.grantEvents(AuditActionGrantCreate); len(ev) != 2 || !strings.Contains(ev[1].Detail, "files_pull=true") ||
		!strings.Contains(ev[1].Detail, "files_push=true") || strings.Contains(ev[1].Detail, "apps_install") {
		t.Fatalf("create audit = %+v", ev)
	}
	// The install box alone, independent of the file boxes.
	inst := grantArgs(a1, a2, ops.GrantLevelControl)
	inst["source_session"], inst["apps_install"] = "other", true
	if gi := k.setGrant(inst); !gi.AppsInstall || gi.FilesPull || gi.FilesPush {
		t.Fatalf("install alone = %+v", gi)
	}
	low := grantArgs(a1, a2, ops.GrantLevelMessage)
	low["source_session"], low["files_push"] = "third", true
	if code, body := k.call(ops.OpGrantSet, low); kindErrCode(t, body) != proto.CodeInvalidArgument {
		t.Fatalf("push flag below Control = %d %s", code, body)
	}
	// Edit: an explicit false clears one box, an absent one keeps it.
	off := grantArgs(a1, a2, ops.GrantLevelControl)
	off["id"], off["if_revision"], off["files_push"] = g.ID, g.Revision, false
	g2 := k.setGrant(off)
	if !g2.FilesPull || g2.FilesPush {
		t.Fatalf("push cleared = %+v", g2)
	}
	if ev := k.grantEvents(AuditActionGrantUpdate); len(ev) == 0 || !strings.Contains(ev[len(ev)-1].Detail, "files_push=false") {
		t.Fatalf("update audit = %+v", ev)
	}
	keep := grantArgs(a1, a2, ops.GrantLevelControl)
	keep["id"], keep["if_revision"], keep["note"], keep["apps_install"] = g.ID, g2.Revision, "files", true
	g3 := k.setGrant(keep)
	if !g3.FilesPull || g3.FilesPush || !g3.AppsInstall {
		t.Fatalf("kept = %+v", g3)
	}
	down := grantArgs(a1, a2, ops.GrantLevelView)
	down["id"], down["if_revision"] = g.ID, g3.Revision
	g4 := k.setGrant(down)
	if !g4.FilesPull || g4.FilesPush || g4.AppsInstall || g4.Level != ops.GrantLevelView {
		t.Fatalf("lowered = %+v", g4)
	}
	// A stranger cannot set them.
	k.as(k.u2)
	other := grantArgs(a1, a2, ops.GrantLevelView)
	other["id"], other["if_revision"], other["files_pull"] = g.ID, g4.Revision, false
	if code, _ := k.call(ops.OpGrantSet, other); code < 400 {
		t.Fatalf("a stranger changed the flags = %d", code)
	}
}

// v0.26.0: one grant_set with a source scope and several targets creates one
// grant per target in one step and answers their ids; a set with a refused member creates nothing;
// everyone is for administrators only; the legacy singleton answer is
// unchanged.
func TestGrantSetSelectors(t *testing.T) {
	k := newGrantStack(t)
	a1, a2, a3 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate),
		k.worker(k.u1, "a3", store.WorkerKindPrivate)
	b1 := k.worker(k.u2, "b1", store.WorkerKindPrivate)
	k.as(k.u1)
	many := map[string]any{"source_scope": "worker", "source_worker_id": a1.ID, "level": ops.GrantLevelControl,
		"targets": []map[string]any{{"worker_id": a2.ID, "session_name": "build"}, {"worker_id": a2.ID, "session_name": "test"},
			{"worker_id": a3.ID}},
		"files_pull": true}
	code, body := k.call(ops.OpGrantSet, many)
	var res ops.GrantSetResult
	if code != http.StatusOK || json.Unmarshal([]byte(body), &res) != nil || len(res.CreatedIDs) != 3 || len(res.Grants) != 3 ||
		res.Grant.ID != res.CreatedIDs[0] || res.Grants[2].TargetSession != "" || res.Grants[0].SourceScope != ops.GrantSourceWorker ||
		res.Grants[0].SourceSession != "" || !res.Grants[1].FilesPull {
		t.Fatalf("several targets = %d %s", code, body)
	}
	if l := k.listGrants(); len(l) != 3 {
		t.Fatalf("listed %d grants", len(l))
	}
	// One member across owners (not an administrator): nothing is created.
	mixed := map[string]any{"source_worker_id": a1.ID, "source_session": "agent", "level": ops.GrantLevelView,
		"targets": []map[string]any{{"worker_id": a2.ID, "session_name": "x"}, {"worker_id": b1.ID}}}
	if code, body := k.call(ops.OpGrantSet, mixed); code < 400 {
		t.Fatalf("a set with a refused member = %d %s", code, body)
	}
	if l := k.listGrants(); len(l) != 3 {
		t.Fatalf("a refused set created %d grants", len(l)-3)
	}
	// Contradictory and empty selections are refused.
	for _, bad := range []map[string]any{
		{"source_worker_id": a1.ID, "source_session": "agent", "level": "view", "target_worker_id": a2.ID,
			"targets": []map[string]any{{"worker_id": a3.ID}}},
		{"source_worker_id": a1.ID, "source_session": "agent", "level": "view", "targets": []map[string]any{}},
		{"source_scope": "worker", "source_worker_id": a1.ID, "source_session": "agent", "level": "view", "target_worker_id": a2.ID},
		{"source_scope": "everyone", "source_worker_id": a1.ID, "level": "view", "target_worker_id": a2.ID},
		{"source_scope": "all", "level": "view", "target_worker_id": a2.ID},
	} {
		if code, body := k.call(ops.OpGrantSet, bad); kindErrCode(t, body) != proto.CodeInvalidArgument {
			t.Fatalf("selection %v = %d %s", bad, code, body)
		}
	}
	// Everyone: an owner is refused, an administrator creates it.
	every := map[string]any{"source_scope": "everyone", "target_worker_id": a2.ID, "target_session": "shared", "level": "control"}
	if code, body := k.call(ops.OpGrantSet, every); code < 400 {
		t.Fatalf("everyone by an owner = %d %s", code, body)
	}
	k.as(k.admin)
	ge := k.setGrant(every)
	if ge.SourceScope != ops.GrantSourceEveryone || ge.SourceWorkerID != "" || ge.Authority != ops.GrantAuthorityAdmin || ge.FilesPull {
		t.Fatalf("everyone = %+v", ge)
	}
	if ev := k.grantEvents(AuditActionGrantCreate); len(ev) == 0 || ev[len(ev)-1].Source != "*" {
		t.Fatalf("everyone audit = %+v", ev)
	}
	// The legacy singleton request answers as before: one grant, no list.
	k.as(k.u1)
	code, body = k.call(ops.OpGrantSet, grantArgs(a1, a2, ops.GrantLevelView))
	if code != http.StatusOK || strings.Contains(body, "created_ids") || strings.Contains(body, `"grants"`) {
		t.Fatalf("legacy singleton = %d %s", code, body)
	}
}

// files.v1: control_set_accept stores the file flags of an acceptance, keeps
// them when a change does not name them, refuses them with accept false,
// shows them in control_list and audits them.
func TestControlAcceptFileFlags(t *testing.T) {
	k := newSessionControlStack(t)
	a, b := k.worker(k.u1, "a", store.WorkerKindPrivate), k.worker(k.u1, "b", store.WorkerKindPrivate)
	k.as(k.admin)
	set := func(args map[string]any) {
		t.Helper()
		if code, body := k.call(ops.OpControlSetAccept, args); code != http.StatusOK {
			t.Fatalf("control_set_accept %v = %d %s", args, code, body)
		}
	}
	flags := func() (pull, push bool) {
		t.Helper()
		rows, err := k.st.ListAccepts(context.Background())
		if err != nil || len(rows) != 1 {
			t.Fatalf("accepts = %+v, %v", rows, err)
		}
		return *rows[0].FilesPull, *rows[0].FilesPush
	}
	set(map[string]any{"worker_id": b.ID, "source_worker_id": a.ID, "accept": true})
	if pull, push := flags(); pull || push {
		t.Fatalf("new acceptance: pull=%t push=%t", pull, push)
	}
	set(map[string]any{"worker_id": b.ID, "source_worker_id": a.ID, "accept": true, "files_pull": true})
	if pull, push := flags(); !pull || push {
		t.Fatalf("pull set: pull=%t push=%t", pull, push)
	}
	set(map[string]any{"worker_id": b.ID, "source_worker_id": a.ID, "accept": true, "sessions": []string{"ci"}})
	if pull, _ := flags(); !pull {
		t.Fatal("a change without the flags cleared files_pull")
	}
	if code, body := k.call(ops.OpControlSetAccept, map[string]any{"worker_id": b.ID, "source_worker_id": a.ID,
		"accept": false, "files_push": true}); kindErrCode(t, body) != proto.CodeInvalidArgument {
		t.Fatalf("flag with accept false = %d %s", code, body)
	}
	var detail string
	for _, ev := range k.audit.all() {
		if ev.Action == ops.OpControlSetAccept && ev.Outcome == AuditOutcomeSuccess && strings.Contains(ev.Detail, "files_pull") {
			detail = ev.Detail
		}
	}
	if !strings.Contains(detail, "files_pull=true") {
		t.Errorf("audit detail = %q", detail)
	}
	code, body := k.call(ops.OpControlList, map[string]any{})
	if code != http.StatusOK || !strings.Contains(body, `"files_pull":true`) {
		t.Errorf("control_list = %d %s", code, body)
	}
	// Revoking clears the flags (no row is left without accept_all).
	set(map[string]any{"worker_id": b.ID, "source_worker_id": a.ID, "accept": false})
	if rows, err := k.st.FileAcceptances(context.Background(), a.ID); err != nil || len(rows) != 0 {
		t.Errorf("file acceptances after the revoke = %+v, %v", rows, err)
	}
}
