package agent

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// stubDialer scripts the connect dialer of a Retirer.
type stubDialer struct {
	fn func(ConnectParams) (*Attempt, error)
}

func (d stubDialer) Connect(_ context.Context, p ConnectParams) (*Attempt, error) { return d.fn(p) }

// retireExpiryRig runs one retirement whose answer arrives after the deadline
// (the clocks are moved by after(), which runs when the answer is about to be
// accepted) and returns the number of entries still pending.
func retireExpiryRig(t *testing.T, replyType string, onSessionStart func(*liveHub, func())) (remaining int, rs *RetiredStore) {
	t.Helper()
	h := newRetireHub(t)
	rec, _ := retiredFor(t, h, h.url())
	rec.MaxSerial, rec.CertSHA256, rec.RatchetSeeded = 3, bytes.Repeat([]byte{1}, 32), true
	rs = NewRetiredStore(t.TempDir())
	if err := rs.Add(rec); err != nil {
		t.Fatal(err)
	}
	wm := newFakeWM()
	exp := wm.expiryAfter(time.Hour)
	live := newLiveHub(t)
	past := func() { wm.set(time.UnixMilli(exp.WallMs).UTC(), wm.Mono()) }
	op, _ := identity.GenerateKey()
	d := stubDialer{fn: func(p ConnectParams) (*Attempt, error) {
		e := exp
		a := &Attempt{Pin: p.Pin, OpPub: op.Public(), TranscriptHash: live.hash, Version: proto.Current, Expiry: &e,
			Ratchet: p.Ratchet, Reply: Reply{Type: replyType, Welcome: okWelcome()}}
		switch replyType {
		case proto.TypeRevoke:
			sig, _ := op.Sign(identity.PurposeRevoke, live.hash)
			a.Reply.RevokeSig = sig
			past() // the answer reached the Worker after the deadline
		case proto.TypeNotEnrolled:
			past()
		case proto.TypeWelcome:
			a.Session = live.workerSess
			onSessionStart(live, past)
		}
		return a, nil
	}}
	r := newRetirer(t, rs, h.url(), func(c *RetirerConfig) { c.Dialer = d; c.WallMono = wm })
	remaining, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return remaining, rs
}

func TestRetirementIgnoresARevokeOrNotEnrolledThatArrivesAfterTheDeadline(t *testing.T) {
	for _, typ := range []string{proto.TypeRevoke, proto.TypeNotEnrolled} {
		t.Run(typ, func(t *testing.T) {
			remaining, rs := retireExpiryRig(t, typ, nil)
			if remaining != 1 {
				t.Fatalf("remaining = %d: a %s after not_after retired the entry", remaining, typ)
			}
			if l, _ := rs.List(); len(l) != 1 {
				t.Fatalf("the pending record was deleted: %d", len(l))
			}
		})
	}
}

func TestRetirementCallThatIsAnsweredAfterTheDeadlineKeepsTheEntry(t *testing.T) {
	remaining, rs := retireExpiryRig(t, proto.TypeWelcome, func(live *liveHub, past func()) {
		go func() {
			st, err := live.hubSess.Accept(context.Background())
			if err != nil {
				return
			}
			if _, err := st.ReadFrame(); err != nil {
				return
			}
			past()
			_ = st.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": ops.WriteResult{Outcome: ops.OutcomeDelivered}})
			_ = st.Close()
		}()
	})
	if remaining != 1 {
		t.Fatalf("remaining = %d: an answer after not_after retired the entry", remaining)
	}
	if l, _ := rs.List(); len(l) != 1 {
		t.Fatalf("the pending record was deleted: %d", len(l))
	}
}
