package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// A116: the command shell flag of a grant is set in grant_set by the owner of
// both Workers (or an administrator), shown by grant_list, kept by a change
// that does not name it, cleared by a lower level, and audited.
func TestGrantCommandShellFlag(t *testing.T) {
	k := newGrantStack(t)
	a1, a2 := k.worker(k.u1, "a1", store.WorkerKindPrivate), k.worker(k.u1, "a2", store.WorkerKindPrivate)
	b := k.worker(k.u2, "b", store.WorkerKindPrivate)
	k.as(k.u1)
	// Shape: the flag needs Control and one named target session.
	bad := grantArgs(a1, a2, ops.GrantLevelMessage)
	bad["command_shell"] = true
	if code, body := k.call(ops.OpGrantSet, bad); kindErrCode(t, body) != proto.CodeInvalidArgument {
		t.Fatalf("flag on Message = %d %s", code, body)
	}
	wide := grantArgs(a1, a2, ops.GrantLevelControl)
	wide["command_shell"], wide["target_session"] = true, ""
	if code, body := k.call(ops.OpGrantSet, wide); kindErrCode(t, body) != proto.CodeInvalidArgument {
		t.Fatalf("flag on a whole-Worker grant = %d %s", code, body)
	}
	set := grantArgs(a1, a2, ops.GrantLevelControl)
	set["command_shell"] = true
	g := k.setGrant(set)
	if !g.CommandShell || g.Level != ops.GrantLevelControl {
		t.Fatalf("created = %+v", g)
	}
	if l := k.listGrants(); len(l) != 1 || !l[0].CommandShell {
		t.Fatalf("listed = %+v", l)
	}
	if ev := k.grantEvents(AuditActionGrantCreate); len(ev) == 0 || !strings.Contains(ev[len(ev)-1].Detail, "command_shell=true") {
		t.Fatalf("create audit = %+v", ev)
	}
	// A change that does not name the flag keeps it.
	keep := grantArgs(a1, a2, ops.GrantLevelControl)
	keep["id"], keep["if_revision"], keep["note"] = g.ID, g.Revision, "shell"
	g2 := k.setGrant(keep)
	if !g2.CommandShell || g2.Note != "shell" {
		t.Fatalf("kept = %+v", g2)
	}
	// Another user (not the owner of the target) cannot set or clear it.
	k.as(k.u2)
	other := grantArgs(a1, a2, ops.GrantLevelControl)
	other["id"], other["if_revision"], other["command_shell"] = g2.ID, g2.Revision, false
	if code, body := k.call(ops.OpGrantSet, other); code != http.StatusForbidden && kindErrCode(t, body) != proto.CodeNotFound {
		t.Fatalf("a stranger clears the flag = %d %s", code, body)
	}
	// Nor mark a session of a Worker of someone else as a command shell.
	k.as(k.u2)
	foreign := grantArgs(b, a1, ops.GrantLevelControl)
	foreign["command_shell"] = true
	if code, body := k.call(ops.OpGrantSet, foreign); code != http.StatusForbidden {
		t.Fatalf("a shell on another owner's Worker = %d %s, want forbidden", code, body)
	}
	// A lower level clears the flag; the audit says so.
	k.as(k.u1)
	down := grantArgs(a1, a2, ops.GrantLevelMessage)
	down["id"], down["if_revision"] = g2.ID, g2.Revision
	g3 := k.setGrant(down)
	if g3.CommandShell || g3.Level != ops.GrantLevelMessage {
		t.Fatalf("lowered = %+v", g3)
	}
	if ev := k.grantEvents(AuditActionGrantUpdate); len(ev) == 0 || !strings.Contains(ev[len(ev)-1].Detail, "command_shell=false") {
		t.Fatalf("update audit = %+v", ev)
	}
	// An administrator may set it on a grant between two owners.
	k.as(k.admin)
	adm := grantArgs(a1, b, ops.GrantLevelControl)
	adm["command_shell"] = true
	if g4 := k.setGrant(adm); !g4.CommandShell || g4.Authority != ops.GrantAuthorityAdmin {
		t.Fatalf("administrator grant = %+v", g4)
	}
}

// The matrix marks the cell of a command shell grant, and only that cell.
func TestAccessMatrixShowsTheCommandShell(t *testing.T) {
	f := grantFixture()
	shell := store.SessionGrantRow{ID: "g-shell", SourceWorkerID: "B", SourceSession: "b1", TargetWorkerID: "C", TargetSession: "c3",
		Level: 3, Authority: store.GrantAuthorityOwner, GrantedBy: "u-alice", SourceOwnerID: "u-alice", TargetOwnerID: "u-alice", CommandShell: true}
	f.Grants = append(f.Grants, shell)
	res := AccessMatrix(f, "u-admin", true, "")
	marked := 0
	for _, c := range res.Cells {
		s, tg := res.Sources[c.Source], res.Targets[c.Target]
		if c.CommandShell {
			marked++
			if s.WorkerID != "B" || s.Session != "b1" || tg.WorkerID != "C" || tg.Session != "c3" || c.Via != "g-shell" {
				t.Errorf("marked cell %+v of %+v > %+v", c, s, tg)
			}
		}
	}
	if marked != 1 {
		t.Fatalf("%d cells marked, want 1", marked)
	}
}
