package workers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// shellGrant is a Control grant of a session on wAlicePi2 on the session
// "sh" of wAlicePi1 that marks it as a command shell (A116).
func shellGrant(id string, level int, flag bool) authz.DirectGrant {
	g := originGrant(id, wAlicePi1, level)
	g.TargetSession, g.CommandShell = "sh", flag
	return g
}

// routeShellSend routes a typed input of p into session of wAlicePi1 as the
// session control call path does and returns the origin sent and the grant
// the caller was told.
func routeShellSend(t *testing.T, p authz.Principal, op, session string, admit func(context.Context, Online) (authz.Principal, error)) (proto.Origin, string, error) {
	t.Helper()
	r := newRig(t, nil)
	st := newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered}))
	r.script(st)
	target := ops.Target{WorkerID: wAlicePi1.ID, SessionName: session}
	var args any = ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target, Text: "ls"}
	if op == ops.OpSendKeys {
		args = ops.SendKeysArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target, Keys: []string{"Enter"}}
	}
	told := "unset"
	_, err := r.rt.Call(context.Background(), CallRequest{Principal: p, Op: op, WorkerID: wAlicePi1.ID, Session: session,
		Args: args, granted: func([]string) {}, commandShell: func(id string) { told = id }, admit: admit})
	if err != nil {
		return proto.Origin{}, told, err
	}
	o := st.request(t).Origin
	if err := o.Validate(); err != nil {
		t.Fatalf("origin %+v: %v", o, err)
	}
	return o, told, nil
}

// The Hub sets shell_ok only for typed input of the source session into the
// session its Control grant marks as a command shell, and names that grant.
func TestRouterSetsShellOKOnlyForTheCommandShellPair(t *testing.T) {
	g1, g2 := ops.NewUUIDv7(), ops.NewUUIDv7()
	session := func(direct ...authz.DirectGrant) authz.Principal {
		return authz.NewWorkerSessionPrincipal(wAlicePi2.ID, "master", nil, direct)
	}
	for _, tc := range []struct {
		name    string
		p       authz.Principal
		op      string
		session string
		want    string
	}{
		{"send_text into the command shell", session(shellGrant(g1, authz.LevelControl, true)), ops.OpSendText, "sh", g1},
		{"send_keys into the command shell", session(shellGrant(g1, authz.LevelControl, true)), ops.OpSendKeys, "sh", g1},
		{"a Control grant without the flag", session(shellGrant(g1, authz.LevelControl, false)), ops.OpSendText, "sh", ""},
		{"the flag and a second grant on the whole Worker", session(originGrant(g2, wAlicePi1, authz.LevelControl), shellGrant(g1, authz.LevelControl, true)),
			ops.OpSendText, "sh", g1},
		{"another session of the target Worker", session(shellGrant(g1, authz.LevelControl, true), originGrant(g2, wAlicePi1, authz.LevelControl)),
			ops.OpSendText, "other", ""},
		{"a session-control grant, no direct grant", authz.NewWorkerSessionPrincipal(wAlicePi2.ID, "master", allScope(), nil), ops.OpSendText, "sh", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, told, err := routeShellSend(t, tc.p, tc.op, tc.session, nil)
			if err != nil {
				t.Fatal(err)
			}
			if o.ShellOK != (tc.want != "") || tc.want != "" && o.GrantID != tc.want || told != tc.want {
				t.Fatalf("origin %+v, told %q; want shell grant %q", o, told, tc.want)
			}
		})
	}
}

// A grant that is changed or revoked before the final admission no longer
// lets the request skip the guard: the decision is the dispatch-time one.
func TestShellOKFollowsTheFinalAdmission(t *testing.T) {
	g1 := ops.NewUUIDv7()
	first := authz.NewWorkerSessionPrincipal(wAlicePi2.ID, "master", nil, []authz.DirectGrant{shellGrant(g1, authz.LevelControl, true)})
	cleared := authz.NewWorkerSessionPrincipal(wAlicePi2.ID, "master", nil, []authz.DirectGrant{shellGrant(g1, authz.LevelControl, false)})
	o, told, err := routeShellSend(t, first, ops.OpSendText, "sh", func(context.Context, Online) (authz.Principal, error) { return cleared, nil })
	if err != nil {
		t.Fatal(err)
	}
	if o.ShellOK || told != "" {
		t.Fatalf("origin %+v, told %q; want no shell_ok after the flag was cleared", o, told)
	}
	// A revoked grant: the final admission refuses the call outright.
	gone := authz.NewWorkerSessionPrincipal(wAlicePi2.ID, "master", nil, nil)
	if _, _, err := routeShellSend(t, first, ops.OpSendText, "sh", func(context.Context, Online) (authz.Principal, error) { return gone, nil }); err == nil {
		t.Fatal("a call whose grant was revoked was sent")
	}
}

// On the call stream path, the write into a command shell carries shell_ok
// and its audit event names the grant with detail command_shell; clearing the
// flag or revoking the grant takes effect on the next call.
func TestCommandShellOnTheCallStreamAndItsAudit(t *testing.T) {
	r := newSCRig(t)
	g := aliceGrant(authz.LevelControl)
	g.ID, g.CommandShell = ops.NewUUIDv7(), true
	r.grants.setDirect(wAlicePi1.ID, "agent", g)
	send := func() ([]json.RawMessage, *fakeStream) {
		return r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
			ops.WriteResult{Outcome: ops.OutcomeDelivered})
	}
	frames, sent := send()
	var wr ops.WriteResult
	resultOf(t, frames, &wr)
	if o := sent.request(t).Origin; !o.ShellOK || o.GrantID != g.ID {
		t.Fatalf("origin = %+v, want shell_ok with the grant", o)
	}
	evs := r.audit.all()
	if len(evs) != 1 || evs[0].GrantID != g.ID || evs[0].Detail != "command_shell="+g.ID {
		t.Fatalf("audit = %+v, want the command shell grant", evs)
	}
	// The owner clears the flag: the next send is an ordinary one.
	g.CommandShell, g.Revision = false, g.Revision+1
	r.grants.setDirect(wAlicePi1.ID, "agent", g)
	frames, sent = send()
	resultOf(t, frames, &wr)
	if o := sent.request(t).Origin; o.ShellOK {
		t.Fatalf("origin after the flag was cleared = %+v", o)
	}
	if evs = r.audit.all(); len(evs) != 2 || evs[1].Detail != "" {
		t.Fatalf("audit after the flag was cleared = %+v", evs)
	}
	// Revoked: refused before anything reaches the Worker.
	r.grants.setDirect(wAlicePi1.ID, "agent")
	frames, _ = send()
	if c := errCodeOf(t, frames); c == "" {
		t.Fatal("a send after the revoke was allowed")
	}
}
