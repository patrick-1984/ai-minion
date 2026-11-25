package store

import (
	"context"
	"errors"
	"testing"
)

// A change that states what its caller saw is stored only while that is still
// what the Hub has: a state another administrator changed is never put back.
func TestControlChangesCheckWhatTheCallerSaw(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	oA, oB := "alice", "bob"
	for _, w := range []WorkerRow{worker("wA", 4, oA, "a"), worker("wB", 5, oB, "b")} {
		if err := s.InsertWorker(ctx, nil, w); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		t.Helper()
		rows, err := s.ListAccepts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case len(rows) == 0:
			return AcceptStateAbsent
		case rows[0].Sessions == nil:
			return AcceptStateAll
		case len(rows[0].Sessions) == 0:
			return AcceptStateNone
		}
		return AcceptStateLimited
	}
	whole := AcceptRow{TargetWorkerID: "wB", SourceWorkerID: "wA", CreatedBy: "u"}
	// Seen absent, stored absent: the tick is stored.
	if err := s.PutAcceptIfState(ctx, whole, oB, oA, AcceptStateAbsent); err != nil || state() != AcceptStateAll {
		t.Fatalf("tick on no row = %v, state %s", err, state())
	}
	// Another administrator narrows it; a tick, an Undo and an untick made on
	// the old view are refused and change nothing.
	if err := s.PutAccept(ctx, AcceptRow{TargetWorkerID: "wB", SourceWorkerID: "wA", Sessions: []string{"ci"}, CreatedBy: "v"}); err != nil {
		t.Fatal(err)
	}
	for _, seen := range []string{AcceptStateAbsent, AcceptStateAll, AcceptStateNone} {
		if err := s.PutAcceptIfState(ctx, whole, oB, oA, seen); !errors.Is(err, ErrControlChanged) {
			t.Errorf("put seen %s on a limited row = %v, want ErrControlChanged", seen, err)
		}
		if ok, err := s.RevokeAcceptIfState(ctx, whole, oB, oA, seen); !errors.Is(err, ErrControlChanged) || ok {
			t.Errorf("revoke seen %s on a limited row = %t, %v, want ErrControlChanged", seen, ok, err)
		}
	}
	if state() != AcceptStateLimited {
		t.Fatalf("state after the refused changes = %s, want limited", state())
	}
	if err := s.PutAcceptIfState(ctx, whole, oB, oA, "sometimes"); err == nil || errors.Is(err, ErrControlChanged) {
		t.Errorf("an unknown state = %v, want a plain error", err)
	}
	// The owner fence still comes first, and the matching view is stored.
	if _, err := s.RevokeAcceptIfState(ctx, whole, "someone-else", oA, AcceptStateLimited); !errors.Is(err, ErrWorkerOwnerChanged) {
		t.Errorf("revoke for another owner = %v", err)
	}
	if ok, err := s.RevokeAcceptIfState(ctx, whole, oB, oA, AcceptStateLimited); err != nil || !ok || state() != AcceptStateAbsent {
		t.Fatalf("revoke on the seen state = %t, %v, state %s", ok, err, state())
	}
	// A blocked row (accept_all on) is "none".
	on, off := true, false
	if err := s.SetWorkerControlIfSeen(ctx, "wB", oB, nil, &on, nil, &off); err != nil {
		t.Fatalf("accept_all on, seen off = %v", err)
	}
	if err := s.SetWorkerControlIfSeen(ctx, "wB", oB, nil, &on, nil, &off); !errors.Is(err, ErrControlChanged) {
		t.Errorf("accept_all seen off while on = %v, want ErrControlChanged", err)
	}
	if err := s.SetWorkerControlIfSeen(ctx, "wB", oB, &off, nil, &off, nil); !errors.Is(err, ErrControlChanged) {
		t.Errorf("user_managed seen off while it is the default on = %v, want ErrControlChanged", err)
	}
	if c, err := s.WorkerControl(ctx, "wB"); err != nil || !c.UserManaged || !c.AcceptAll {
		t.Fatalf("switches after the refused changes = %+v, %v", c, err)
	}
	if _, err := s.RevokeAcceptIfState(ctx, whole, oB, oA, AcceptStateAbsent); err != nil || state() != AcceptStateNone {
		t.Fatalf("revoke on accept_all = %v, state %s; want the row that allows nothing", err, state())
	}
	if err := s.PutAcceptIfState(ctx, whole, oB, oA, AcceptStateAbsent); !errors.Is(err, ErrControlChanged) || state() != AcceptStateNone {
		t.Errorf("a tick that saw no row on a blocked source = %v, state %s", err, state())
	}

	// Nominations: seen as a slave / as a master.
	boss := MasterRow{WorkerID: "wA", SessionName: "boss", CreatedBy: "u"}
	if err := s.PutMasterSessionIfSeen(ctx, boss, oA, &on); !errors.Is(err, ErrControlChanged) {
		t.Errorf("nominate seen as master while it is none = %v", err)
	}
	if err := s.PutMasterSessionIfSeen(ctx, boss, oA, &off); err != nil {
		t.Fatal(err)
	}
	if err := s.PutMasterSessionIfSeen(ctx, boss, oA, &off); !errors.Is(err, ErrControlChanged) {
		t.Errorf("nominate seen as slave while it is a master = %v", err)
	}
	if ok, err := s.DeleteMasterSessionIfSeen(ctx, "wA", "boss", oA, &off); !errors.Is(err, ErrControlChanged) || ok {
		t.Errorf("end seen as slave while it is a master = %t, %v", ok, err)
	}
	if ok, err := s.DeleteMasterSessionIfSeen(ctx, "wA", "boss", oA, &on); err != nil || !ok {
		t.Fatalf("end seen as master = %t, %v", ok, err)
	}
}

// The name-only revoke refuses an active session-bound master inside the
// transaction of its delete, on every path that reaches master_session.
func TestNameOnlyDeleteRefusesAnActiveBoundMasterInItsTransaction(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.InsertWorker(ctx, nil, worker("wA", 4, "alice", "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO bound_master_session
		(worker_id,session_name,incarnation_id,owner_id,worker_pub,generation,active,changed_at) VALUES ('wA','boss','incarnation-one','alice',?,1,1,1)`, pub(4)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteMasterSession(ctx, "wA", "boss"); !errors.Is(err, ErrBoundMasterSlot) {
		t.Errorf("DeleteMasterSession = %v, want ErrBoundMasterSlot", err)
	}
	if _, err := s.DeleteSessionControl(ctx, "wA", "boss"); !errors.Is(err, ErrBoundMasterSlot) {
		t.Errorf("DeleteSessionControl = %v, want ErrBoundMasterSlot", err)
	}
	if _, err := s.DeleteMasterSessionIfOwner(ctx, "wA", "boss", "alice"); !errors.Is(err, ErrBoundMasterSlot) {
		t.Errorf("DeleteMasterSessionIfOwner = %v, want ErrBoundMasterSlot", err)
	}
	// The owner fence is checked before it.
	if _, err := s.DeleteMasterSessionIfOwner(ctx, "wA", "boss", "bob"); !errors.Is(err, ErrWorkerOwnerChanged) {
		t.Errorf("for another owner = %v, want ErrWorkerOwnerChanged", err)
	}
	// Another session of the Worker, and a revoked bound nomination, are free.
	if _, err := s.DeleteMasterSession(ctx, "wA", "plain"); err != nil {
		t.Errorf("plain name = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE bound_master_session SET active=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteMasterSessionIfOwner(ctx, "wA", "boss", "alice"); err != nil {
		t.Errorf("after the bound revoke = %v", err)
	}
}
