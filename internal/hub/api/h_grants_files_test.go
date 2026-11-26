package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// files.v1: the file transfer flags of a grant are refused on a create (a new
// grant is claimed) and on a claimed grant, set on a bound grant by whoever
// may change it, kept by a change that does not name them, files_push
// cleared by a lower level, and audited.
func TestGrantFileFlags(t *testing.T) {
	k := newGrantStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	k.as(k.u1)
	create := grantArgs(a1, a2, ops.GrantLevelControl)
	create["files_pull"] = true
	if code, body := k.call(ops.OpGrantSet, create); kindErrCode(t, body) != proto.CodeInvalidArgument {
		t.Fatalf("flag on a create = %d %s", code, body)
	}
	low := grantArgs(a1, a2, ops.GrantLevelMessage)
	low["files_push"] = true
	if code, body := k.call(ops.OpGrantSet, low); kindErrCode(t, body) != proto.CodeInvalidArgument {
		t.Fatalf("push flag below Control = %d %s", code, body)
	}
	g := k.setGrant(grantArgs(a1, a2, ops.GrantLevelControl))
	if g.FilesPull || g.FilesPush {
		t.Fatalf("created = %+v", g)
	}
	// Still claimed: the flags are refused.
	claimed := grantArgs(a1, a2, ops.GrantLevelControl)
	claimed["id"], claimed["if_revision"], claimed["files_pull"] = g.ID, g.Revision, true
	if code, body := k.call(ops.OpGrantSet, claimed); kindErrCode(t, body) != proto.CodeInvalidArgument {
		t.Fatalf("flag on a claimed grant = %d %s", code, body)
	}
	bound, err := k.st.BindSessionGrant(context.Background(), g.ID, "inc-1", g.Revision, store.GrantWrite{ActorID: k.u1})
	if err != nil {
		t.Fatal(err)
	}
	set := grantArgs(a1, a2, ops.GrantLevelControl)
	set["id"], set["if_revision"], set["files_pull"], set["files_push"] = g.ID, bound.Revision, true, true
	g2 := k.setGrant(set)
	if !g2.FilesPull || !g2.FilesPush {
		t.Fatalf("set on a bound grant = %+v", g2)
	}
	if ev := k.grantEvents(AuditActionGrantUpdate); len(ev) == 0 || !strings.Contains(ev[len(ev)-1].Detail, "files_pull=true") ||
		!strings.Contains(ev[len(ev)-1].Detail, "files_push=true") {
		t.Fatalf("update audit = %+v", ev)
	}
	keep := grantArgs(a1, a2, ops.GrantLevelControl)
	keep["id"], keep["if_revision"], keep["note"] = g.ID, g2.Revision, "files"
	g3 := k.setGrant(keep)
	if !g3.FilesPull || !g3.FilesPush {
		t.Fatalf("kept = %+v", g3)
	}
	down := grantArgs(a1, a2, ops.GrantLevelView)
	down["id"], down["if_revision"] = g.ID, g3.Revision
	g4 := k.setGrant(down)
	if !g4.FilesPull || g4.FilesPush || g4.Level != ops.GrantLevelView {
		t.Fatalf("lowered = %+v", g4)
	}
	if l := k.listGrants(); len(l) != 1 || !l[0].FilesPull || l[0].FilesPush {
		t.Fatalf("listed = %+v", l)
	}
	// A stranger cannot set them.
	k.as(k.u2)
	other := grantArgs(a1, a2, ops.GrantLevelView)
	other["id"], other["if_revision"], other["files_pull"] = g.ID, g4.Revision, false
	if code, _ := k.call(ops.OpGrantSet, other); code < 400 {
		t.Fatalf("a stranger changed the flags = %d", code)
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
