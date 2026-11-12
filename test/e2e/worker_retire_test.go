//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// Story 4.7 integration test (blocker 2 of the round-2 review): the Worker's
// Retirer (production dialer, production wire) against the REAL Hub endpoint
// (internal/hub/workers, the ops-catalog op worker_retire, amendment A-16).
// After `minion worker pair --new-code`, the old key must be refused by the Hub.
func TestRetirerAgainstTheRealHubRemovesTheOldKeyAndTheNextConnectGetsASignedRevoke(t *testing.T) {
	h := newConnHub(t)
	oldKey, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	row := h.enrol(oldKey.Public(), "pi-old")
	otherKey, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	h.enrol(otherKey.Public(), "pi-other")

	rs := wagent.NewRetiredStore(t.TempDir())
	if err := rs.Add(wagent.RetiredIdentity{Key: oldKey, HubURL: h.url(), Pin: h.pin(), WorkerID: row.ID, RetiredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ret, err := wagent.NewRetirer(wagent.RetirerConfig{Store: rs, Dialer: wagent.NewHubDialer(), Name: "pi"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	remaining, err := ret.RunOnce(ctx)
	if err != nil || remaining != 0 {
		t.Fatalf("RunOnce = %d, %v; the real Hub must confirm the retirement", remaining, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the retirement took %s against a Hub that answers", took)
	}
	if p, _ := rs.List(); len(p) != 0 {
		t.Fatalf("the confirmed record must be deleted: %d left", len(p))
	}

	// The Hub removed the enrollment of exactly that key and left a tombstone.
	if _, err := h.st.WorkerByPub(context.Background(), oldKey.Public()); err == nil {
		t.Fatal("the old enrollment still exists at the Hub")
	}
	tomb, err := h.st.TombstoneByPub(context.Background(), oldKey.Public())
	if err != nil || tomb.WorkerID != row.ID {
		t.Fatalf("tombstone = %+v, %v", tomb, err)
	}
	if _, err := h.st.WorkerByPub(context.Background(), otherKey.Public()); err != nil {
		t.Fatalf("another Worker was affected: %v", err)
	}
	eventually(t, "the old identity to be offline", func() bool { return !h.service().Registry().IsOnline(row.ID) })

	// A retained copy of the old key is refused: the next connect gets a revoke
	// that the pinned Hub key signed for that connection, and no welcome.
	att, err := wagent.NewHubDialer().Connect(ctx, wagent.ConnectParams{HubURL: h.url(), Key: oldKey, Name: "copy", Pin: h.pin()})
	if err != nil {
		t.Fatal(err)
	}
	if att.Reply.Type != proto.TypeRevoke || att.Session != nil {
		t.Fatalf("the old key got %q after the retirement, want revoke", att.Reply.Type)
	}
	if err := identity.Verify(att.OpPub, identity.PurposeRevoke, att.TranscriptHash, att.Reply.RevokeSig); err != nil {
		t.Fatalf("the revoke must be signed by the Hub: %v", err)
	}
	if h.service().Registry().IsOnline(row.ID) {
		t.Fatal("the old key is online after the retirement")
	}

	// A second run (a restarted service with a stale record) is satisfied by the
	// revoke itself.
	if err := rs.Add(wagent.RetiredIdentity{Key: oldKey, HubURL: h.url(), Pin: h.pin(), WorkerID: row.ID, RetiredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if remaining, err := ret.RunOnce(ctx); err != nil || remaining != 0 {
		t.Fatalf("second RunOnce = %d, %v", remaining, err)
	}
}

// The old identity is online at the Hub only for the length of one retirement,
// and the retirer stops at once when its context ends, whatever the Hub does.
func TestRetirerRunAgainstTheRealHubRevokesThenStopsPromptly(t *testing.T) {
	h := newConnHub(t)
	oldKey, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	row := h.enrol(oldKey.Public(), "pi-old")
	rs := wagent.NewRetiredStore(t.TempDir())
	if err := rs.Add(wagent.RetiredIdentity{Key: oldKey, HubURL: h.url(), Pin: h.pin(), WorkerID: row.ID, RetiredAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	ret, err := wagent.NewRetirer(wagent.RetirerConfig{
		Store: rs, Dialer: wagent.NewHubDialer(), Name: "pi",
		Session: transport.SessionOptions{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); ret.Run(ctx) }()
	eventually(t, "the record to be revoked", func() bool { p, _ := rs.List(); return len(p) == 0 })
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(wagent.DefaultStopTimeout):
		t.Fatal("Run did not stop within the stop bound")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Run took %s to stop", took)
	}
}
