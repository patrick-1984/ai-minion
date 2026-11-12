package workers

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid/hubidtest"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

func rootRig(t *testing.T, fpr func() identity.Fingerprint) (*Invitations, *store.Store, identity.Fingerprint) {
	t.Helper()
	st := openStore(t, t.TempDir())
	svc := hubidtest.Open(t, st, t.TempDir())
	sn, err := svc.Current()
	if err != nil {
		t.Fatal(err)
	}
	real, err := identity.FingerprintOf(sn.RootPub)
	if err != nil {
		t.Fatal(err)
	}
	ensureUser(t, st, alice.UserID)
	inv, err := NewInvitations(InvitationsConfig{Store: st, FingerprintFunc: func() identity.Fingerprint { return fpr2(fpr, real) }})
	if err != nil {
		t.Fatal(err)
	}
	return inv, st, real
}

func fpr2(f func() identity.Fingerprint, real identity.Fingerprint) identity.Fingerprint {
	if f == nil {
		return real
	}
	return f()
}

// The insert is conditional on the stored root: a key bound to a root that was
// rotated away before the insert is not stored; the retry binds the new root.
func TestAKeyBoundToASupersededRootIsNotStoredAndTheRetryBindsTheCurrentRoot(t *testing.T) {
	stale := newHubFingerprint(t)
	var calls atomic.Int32
	var realFpr identity.Fingerprint
	inv, st, real := rootRig(t, func() identity.Fingerprint {
		if calls.Add(1) == 1 {
			return stale // the root was read before a rotation
		}
		return realFpr
	})
	realFpr = real
	res, err := inv.Create(context.Background(), alice, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("the root was read %d times, want 2 (one retry)", calls.Load())
	}
	key, err := identity.ParseInviteKey(res.Key)
	if err != nil || key.Fingerprint != real {
		t.Fatalf("the stored key is bound to %v (%v), want the current root", key, err)
	}
	rows, _ := st.ListInvitations(context.Background(), alice.UserID)
	if len(rows) != 1 {
		t.Fatalf("%d keys stored, want 1", len(rows))
	}
}

// A root that keeps changing is a conflict; nothing is stored.
func TestAKeyForAChangingRootIsAConflict(t *testing.T) {
	inv, st, _ := rootRig(t, func() identity.Fingerprint { return newHubFingerprint(t) })
	_, err := inv.Create(context.Background(), alice, ops.InvitationCreateArgs{Mode: ops.InvitationOneTime, TTL: ops.TTL1h})
	if codeOf(err) != proto.CodeConflict {
		t.Fatalf("err = %v, want conflict", err)
	}
	if rows, _ := st.ListInvitations(context.Background(), alice.UserID); len(rows) != 0 {
		t.Fatalf("%d keys stored for a superseded root", len(rows))
	}
}
