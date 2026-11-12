package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// newInvitationsForRootTest builds the invitation service the way the
// invitations module wires it.
var newInvitationsForRootTest = func(st *store.Store, svc *hubid.Service) (*workers.Invitations, error) {
	return workers.NewInvitations(workers.InvitationsConfig{Store: st, RootFunc: invitationRoot(svc)})
}

// RC1 release gate, finding 3: after a master-key rotation whose durability
// confirmation failed, the previous snapshot keeps serving (D3) while the
// database already records the new root. An invitation must never name that
// unconfirmed root: its creation is refused with conflict until the rotation
// is confirmed, and then names the new root.
func TestInvitationsNameOnlyTheServingRootAcrossAnUnconfirmedRotation(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// failDurable makes the durability confirmation fail right after the
	// rotation committed (not the earlier confirmation of the passphrase
	// attempt).
	var failDurable, committed atomic.Bool
	svc := hubidtest.Open(t, st, t.TempDir(), hubidtest.WithOptions(func(o *hubid.Options) {
		o.Hooks.Point = func(name string) error {
			if name == hubid.PointCommitted {
				committed.Store(true)
			}
			return nil
		}
		o.Hooks.MakeDurable = func(ctx context.Context) error {
			if failDurable.Load() && committed.Load() {
				return errors.New("injected: durability not confirmed")
			}
			return st.MakeDurable(ctx)
		}
	}))
	if _, err := st.DB().Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES ('u-alice', 'alice', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	inv, err := newInvitationsForRootTest(st, svc)
	if err != nil {
		t.Fatal(err)
	}
	caller := workers.InvitationCaller{UserID: "u-alice", Actor: "u-alice"}
	create := func() (identity.Fingerprint, error) {
		res, err := inv.Create(ctx, caller, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL4h})
		if err != nil {
			return "", err
		}
		k, err := identity.ParseInviteKey(res.Key)
		if err != nil {
			t.Fatal(err)
		}
		return k.Fingerprint, nil
	}
	snap, err := svc.Current()
	if err != nil {
		t.Fatal(err)
	}
	oldRoot, _ := identity.FingerprintOf(snap.RootPub)
	if got, err := create(); err != nil || got != oldRoot {
		t.Fatalf("before the rotation: %s %v, want %s", got, err, oldRoot)
	}

	const newPass = "another correct horse battery"
	failDurable.Store(true)
	committed.Store(false)
	if _, err := svc.RotateMaster(ctx, hubidtest.Actor, hubidtest.Passphrase, newPass, newPass, hubid.IssueOptions{}); err == nil {
		t.Fatal("the rotation was confirmed although durability failed")
	}
	st0, err := svc.Status(ctx)
	if err != nil || st0.RootFingerprint == string(oldRoot) {
		t.Fatalf("the database does not record the new root yet: %+v %v", st0, err)
	}
	if got, err := create(); proto.CodeOf(err) != proto.CodeConflict {
		t.Fatalf("an invitation during the unconfirmed rotation = %s %v, want conflict", got, err)
	}

	failDurable.Store(false)
	if _, err := svc.RotateMaster(ctx, hubidtest.Actor, hubidtest.Passphrase, newPass, newPass, hubid.IssueOptions{}); err != nil {
		t.Fatalf("repeating the rotation to confirm it: %v", err)
	}
	snap, err = svc.Current()
	if err != nil {
		t.Fatal(err)
	}
	newRoot, _ := identity.FingerprintOf(snap.RootPub)
	if newRoot == oldRoot || string(newRoot) != st0.RootFingerprint {
		t.Fatalf("serving root %s after the confirmation, recorded %s", newRoot, st0.RootFingerprint)
	}
	if got, err := create(); err != nil || got != newRoot {
		t.Fatalf("after the confirmation: %s %v, want %s", got, err, newRoot)
	}
}
