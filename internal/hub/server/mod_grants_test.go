package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// callFrom is one forwarded call of the Worker's session source: a call
// stream with one op.request that names the source session. It returns the
// type and the body of the answer.
func (pw *pipeWorker) callFrom(t *testing.T, source, op string, args map[string]any) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := pw.sess.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Reset()
	_ = st.SetDeadline(time.Now().Add(10 * time.Second))
	if err := st.WriteFrame(map[string]any{"type": proto.TypeOpRequest, "op": op, "request_id": ops.NewUUIDv7(),
		"args": args, "source_session": source}); err != nil {
		t.Fatal(err)
	}
	for {
		b, err := st.ReadFrame()
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if typ, _ := proto.FrameType(b); typ != proto.TypeOpProgress {
			return typ, string(b)
		}
	}
}

// On the composed Hub: an owner grants one session of a Worker a view of a
// session of another Worker through grant_set; the forwarded call of that
// session reads the granted session and nothing else, until grant_revoke. An
// API key cannot write a grant. Between users only an administrator grants;
// when that administrator is demoted the grant is suspended and gives nothing
// until another administrator approves it.
func TestSessionGrantsOnTheRealHub(t *testing.T) {
	// A module before the pipe module stops after it and before the store
	// closes: its hook reads what the stopping Hub wrote of the grant usage.
	var (
		probe      *store.Store
		usedAtStop map[string]store.SessionGrantUse
		probeErr   error
		probeGrant string
	)
	ph := startPipeHub(t, nil, func(r *Registry) {
		if err := r.Register("grant_use_probe", pipeModuleOrder-1, func(_ context.Context, e *Env) error {
			var err error
			if probe, err = Require[*store.Store](e); err != nil {
				return err
			}
			e.OnStop(func(ctx context.Context) error {
				usedAtStop, probeErr = probe.SessionGrantUsage(ctx, probeGrant)
				return nil
			})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	ctx := context.Background()
	usage := func(what, id string, want int64) {
		t.Helper()
		if err := ph.use.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		u, err := ph.st.SessionGrantUsage(ctx, id)
		if err != nil || u[id].UseCount != want || (want > 0) != (u[id].LastUsedAt != nil) {
			t.Fatalf("%s: usage of the grant = %+v (%v), want %d uses", what, u[id], err, want)
		}
	}
	user := func(name, role string) (store.UserRow, map[string]string) {
		t.Helper()
		u := store.UserRow{ID: store.NewID(), Username: name, PasswordHash: "x", CredentialVersion: 1, CreatedAt: 1, UpdatedAt: 1}
		if err := ph.st.CreateUser(ctx, store.CreateUserParams{User: u, Role: role}); err != nil {
			t.Fatal(err)
		}
		row, err := ph.st.UserByIdentifier(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		is, err := ph.mgr.Create(ctx, row, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return row, asSession(is.Token, is.CSRFToken)
	}
	alice, asAlice := user("alice", "user")
	bob, _ := user("bob", "user")
	root2, asRoot2 := user("root2", "admin")
	// pipeHub.connect enrolls for ph.admin: lend it the owner for the call.
	connect := func(name string, owner store.UserRow, shared bool) *pipeWorker {
		t.Helper()
		admin := ph.admin
		ph.admin = owner
		defer func() { ph.admin = admin }()
		if shared {
			return ph.connectShared(t, name)
		}
		return ph.connect(t, name)
	}
	a1, a2, b := connect("a1", alice, false), connect("a2", alice, true), connect("b", bob, true)

	op := func(as map[string]string, name string, args map[string]any) (int, string) {
		t.Helper()
		if name != ops.OpGrantList {
			args["request_id"] = ops.NewUUIDv7()
		}
		code, body, _ := ph.hc.do("POST", "/api/v1/ops/"+name, args, as)
		return code, body
	}
	grant := func(as map[string]string, src, tgt *pipeWorker) (int, ops.Grant, string) {
		t.Helper()
		code, body := op(as, ops.OpGrantSet, map[string]any{"source_worker_id": src.id, "source_session": "agent",
			"target_worker_id": tgt.id, "target_session": "minion-1", "level": ops.GrantLevelView})
		var res ops.GrantSetResult
		_ = json.Unmarshal([]byte(body), &res)
		return code, res.Grant, body
	}
	snapshot := func(tgt *pipeWorker, session string) (string, string) {
		t.Helper()
		return a1.callFrom(t, "agent", ops.OpSessionSnapshot, map[string]any{"target": ops.Target{WorkerID: tgt.id, SessionName: session}})
	}
	reads := func(what string, tgt *pipeWorker) {
		t.Helper()
		if typ, body := snapshot(tgt, "minion-1"); typ != proto.TypeOpResult || !strings.Contains(body, "ready") {
			t.Fatalf("%s: snapshot = %s", what, body)
		}
	}
	refused := func(what string, tgt *pipeWorker, session string) {
		t.Helper()
		typ, body := snapshot(tgt, session)
		if typ == proto.TypeOpResult || (!strings.Contains(body, string(proto.CodeForbidden)) && !strings.Contains(body, string(proto.CodeNotFound))) {
			t.Fatalf("%s: snapshot = %s, want a refusal", what, body)
		}
	}

	// Without a grant nothing is reached.
	refused("no grant", a2, "minion-1")
	// The owner grants between two Workers of their own.
	code, g, body := grant(asAlice, a1, a2)
	if code != 200 || g.ID == "" || g.Authority != ops.GrantAuthorityOwner {
		t.Fatalf("grant_set by the owner = %d %s", code, body)
	}
	usage("before the first call", g.ID, 0)
	reads("owner grant", a2)
	refused("another session of the target", a2, "other")
	refused("another Worker", b, "minion-1")
	// The allowed call counts as one use of the grant, the refused ones as none.
	usage("one allowed call and two refused", g.ID, 1)
	reads("owner grant, again", a2)
	usage("two allowed calls", g.ID, 2)

	// An API key cannot call grant_set (SessionOnly).
	code, body = op(ph.asAdmin, "apikey_create", map[string]any{"name": "k", "resources": workerGrant(a2.id), "actions": []string{"read", "operate"}})
	var key ops.APIKeyCreateResult
	if code != 200 || json.Unmarshal([]byte(body), &key) != nil || key.Key == "" {
		t.Fatalf("apikey_create = %d %s", code, body)
	}
	if code, _, body := grant(asKey(key.Key), a2, a1); code != 403 {
		t.Fatalf("grant_set by an API key = %d %s, want 403", code, body)
	}

	// After grant_revoke the next call is refused.
	if code, body := op(asAlice, ops.OpGrantRevoke, map[string]any{"id": g.ID, "if_revision": g.Revision}); code != 200 {
		t.Fatalf("grant_revoke = %d %s", code, body)
	}
	refused("revoked", a2, "minion-1")

	// Between users: a user cannot grant, an administrator can.
	if code, _, body := grant(asAlice, a1, b); code != 403 {
		t.Fatalf("grant_set between users by a user = %d %s, want 403", code, body)
	}
	code, g, body = grant(asRoot2, a1, b)
	if code != 200 || g.Authority != ops.GrantAuthorityAdmin || g.GrantedBy != root2.ID {
		t.Fatalf("grant_set between users by an administrator = %d %s", code, body)
	}
	reads("administrator grant", b)

	// The granting administrator is demoted: suspended, nothing is reached.
	if _, err := ph.st.DB().ExecContext(ctx, `DELETE FROM user_roles WHERE user_id = ? AND role = 'admin'`, root2.ID); err != nil {
		t.Fatal(err)
	}
	refused("grantor demoted", b, "minion-1")
	code, body = op(ph.asAdmin, ops.OpGrantList, map[string]any{})
	var list ops.GrantListResult
	if code != 200 || json.Unmarshal([]byte(body), &list) != nil || len(list.Grants) != 1 ||
		!list.Grants[0].Suspended || list.Grants[0].State != ops.GrantStateSuspended {
		t.Fatalf("grant_list after the demotion = %d %s", code, body)
	}
	// Another administrator approves it: it works again.
	if code, body := op(ph.asAdmin, ops.OpGrantApprove, map[string]any{"id": g.ID, "if_revision": list.Grants[0].Revision}); code != 200 {
		t.Fatalf("grant_approve = %d %s", code, body)
	}
	reads("approved", b)

	// The audit names the grants: create (twice), revoke, approve.
	rows, err := ph.st.DB().QueryContext(ctx, `SELECT action, source, detail FROM audit_events WHERE grant_id <> '' ORDER BY ts, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]int{}
	for rows.Next() {
		var action, source, detail string
		if err := rows.Scan(&action, &source, &detail); err != nil {
			t.Fatal(err)
		}
		if source != a1.id+":agent" || detail == "" {
			t.Fatalf("audit row %s: source %q detail %q", action, source, detail)
		}
		seen[action]++
	}
	if seen["grant.create"] != 2 || seen["grant.revoke"] != 1 || seen["grant.approve"] != 1 {
		t.Fatalf("grant audit rows = %v", seen)
	}
	_ = rows.Close()

	// The Hub stops: the uses that were noted and not yet written (the
	// administrator's grant allowed two calls, the refused one none) reach
	// the store before it closes.
	probeGrant = g.ID
	ph.h.stop(t)
	if probeErr != nil || usedAtStop[g.ID].UseCount != 2 || usedAtStop[g.ID].LastUsedAt == nil {
		t.Fatalf("usage written at shutdown = %+v (%v), want 2 uses", usedAtStop[g.ID], probeErr)
	}
}

type fakeExpiredGrants struct{ calls chan int64 }

func (f fakeExpiredGrants) DeleteExpiredSessionGrants(_ context.Context, before int64) (int64, error) {
	f.calls <- before
	return 0, nil
}

// The sweep deletes at start and on every tick what expired 30 days ago, and
// ends with its context.
func TestExpiredGrantSweepRunsAndStops(t *testing.T) {
	f := fakeExpiredGrants{calls: make(chan int64, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sweepExpiredGrants(ctx, f, 5*time.Millisecond) }()
	for i := 0; i < 2; i++ {
		select {
		case before := <-f.calls:
			want := time.Now().Add(-grantExpiredKeep).UnixMilli()
			if before > want || before < want-60_000 {
				t.Fatalf("sweep before = %d, want about %d", before, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the sweep did not run")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep did not stop")
	}
}
