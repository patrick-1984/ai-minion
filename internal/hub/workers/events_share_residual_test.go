package workers

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Residual privacy findings of the second S4.15 review (R2-01, R2-02, R2-03),
// against the real EventHub over the real Hub database and the real Pipe.

// proofProbe answers a probe as a Worker would that holds rule r at revision
// rev: it echoes the barrier it was asked to raise.
func proofProbe(r ops.ShareRule, rev uint64, calls *atomic.Int32) func(context.Context, uint64) (ops.ShareProbe, error) {
	return func(_ context.Context, barrier uint64) (ops.ShareProbe, error) {
		if calls != nil {
			calls.Add(1)
		}
		return ops.ShareProbe{Rule: r, Revision: strconv.FormatUint(rev, 10), OpBarrier: strconv.FormatUint(barrier, 10)}, nil
	}
}

func pendingRows(t *testing.T, a *authRig) []store.PendingShare {
	t.Helper()
	p, err := a.db.LoadPendingShares(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// R2-01: the restriction is written before the request is sent, a restart keeps
// it, and only evidence ordered after it settles it.
func TestAPendingRestrictionIsDurableBeforeDispatchAndSurvivesARestart(t *testing.T) {
	a := newAuthRig(t)
	a.connect(t, decl(1, "all"), privSess)
	a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
	none := rule("none")
	call := a.hub.Load().BeginShare(wAlicePi1.ID, &none)
	if call.Err() != nil || call.Seq() == 0 {
		t.Fatalf("begin: seq=%d err=%v", call.Seq(), call.Err())
	}
	// Before anything is sent the restriction is in the database.
	if got := pendingRows(t, a); len(got) != 1 || got[0].Seq != call.Seq() || got[0].Rule.Mode != "none" || got[0].WorkerID != wAlicePi1.ID {
		t.Fatalf("pending rows before dispatch = %+v", got)
	}
	// A crash in flight: a fresh Hub over the same database, before any reconciliation.
	a.restart(t)
	if a.readable(t, privSess) {
		t.Fatal("a restart lost a restriction that was dispatched: private history is served")
	}
	// The Worker reconnecting and declaring its old rule proves nothing.
	l, _ := a.connect(t, decl(1, "all"), privSess)
	if a.readable(t, privSess) {
		t.Fatal("a declaration on a new connection settled a restriction of unknown outcome")
	}
	// A probe that raised the barrier to the sequence number does.
	l.SetShareProbe(proofProbe(rule("all"), 1, nil))
	if err := a.hub.Load().ConfirmShare(context.Background(), wAlicePi1.ID); err != nil {
		t.Fatal(err)
	}
	if !a.readable(t, privSess) {
		t.Fatal("the Worker proved it never applied the change; the history stays hidden")
	}
	if got := pendingRows(t, a); len(got) != 0 {
		t.Fatalf("a settled restriction stays in the database: %+v", got)
	}
}

func TestARestrictionThatCannotBeRecordedIsNotSent(t *testing.T) {
	a := newAuthRig(t)
	fs := &failingPending{ShareStore: a.db}
	a.hub.Store(NewEventHub(EventHubOptions{Share: fs}))
	a.connect(t, decl(1, "all"), privSess)
	a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
	fs.fail.Store(true)
	none := rule("none")
	call := a.hub.Load().BeginShare(wAlicePi1.ID, &none)
	if call.Err() == nil {
		t.Fatal("a restriction that could not be written was reported as recorded: the request would be sent")
	}
	call.Applied(ops.ShareResult{Rule: none, Revision: "9"}) // every settlement is a no-op
	call.Unknown()
	call.Refused()
	if !a.readable(t, privSess) {
		t.Fatal("a call that was never sent hid the history")
	}
	if d, _ := a.hub.Load().ShareDeclOf(wAlicePi1.ID); d.Rule.Mode != "all" || d.Revision != 1 {
		t.Fatalf("an unsent call changed the held rule: %+v", d)
	}
}

type failingPending struct {
	ShareStore
	fail atomic.Bool
}

func (f *failingPending) SavePendingShare(ctx context.Context, p store.PendingShare) error {
	if f.fail.Load() {
		return errors.New("disk full")
	}
	return f.ShareStore.SavePendingShare(ctx, p)
}

// A refusal that changed nothing lifts a restriction and removes its row; a
// known rule that shares the session with nothing pending stays readable across
// a restart while the Worker is offline.
func TestOfflineKnownHistoryStaysReadableWithoutAPendingRevocation(t *testing.T) {
	a := newAuthRig(t)
	a.connect(t, decl(1, "all"), privSess)
	a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
	none := rule("none")
	call := a.hub.Load().BeginShare(wAlicePi1.ID, &none)
	call.Refused()
	if got := pendingRows(t, a); len(got) != 0 {
		t.Fatalf("a refused restriction stays in the database: %+v", got)
	}
	a.restart(t) // the Worker is not connected
	if !a.readable(t, privSess) {
		t.Fatal("history of a genuinely offline Worker whose last rule shares it, with nothing pending, was hidden")
	}
	// With a pending revocation the same offline history is hidden.
	a.hub.Load().BeginShare(wAlicePi1.ID, &none).Unknown()
	a.restart(t)
	if a.readable(t, privSess) {
		t.Fatal("offline history was served although a revocation is pending")
	}
}

// R2-02: only evidence ordered after the restriction settles it.
func TestOnlyCausalEvidenceSettlesAnUnknownRestriction(t *testing.T) {
	none := rule("none")
	all := rule("all")
	setup := func(t *testing.T) (*authRig, ShareCall, ShareCall) {
		a := newAuthRig(t)
		a.connect(t, decl(2, "all"), privSess)
		a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
		older := a.hub.Load().BeginShare(wAlicePi1.ID, &all) // the Worker commits rev 3; the answer is held
		later := a.hub.Load().BeginShare(wAlicePi1.ID, &none)
		later.Unknown() // the Worker commits rev 4; the answer is lost
		if a.readable(t, privSess) {
			t.Fatal("setup: the unknown restriction is not in force")
		}
		return a, older, later
	}
	t.Run("a-delayed-older-answer-with-its-own-barrier", func(t *testing.T) {
		a, older, _ := setup(t)
		older.Applied(ops.ShareResult{Rule: all, Revision: "3", OpBarrier: strconv.FormatUint(older.Seq(), 10)})
		if a.readable(t, privSess) {
			t.Fatal("a delayed older answer settled the newer restriction")
		}
		if d, _ := a.hub.Load().ShareDeclOf(wAlicePi1.ID); d.Revision != 3 {
			t.Fatalf("the older answer is the Worker's rule at its time and is adopted as such: %+v", d)
		}
	})
	t.Run("a-delayed-older-answer-without-a-barrier", func(t *testing.T) {
		a, older, _ := setup(t)
		older.Applied(ops.ShareResult{Rule: all, Revision: "9"})
		if a.readable(t, privSess) {
			t.Fatal("an answer of an older call with a higher revision settled the newer restriction")
		}
	})
	t.Run("an-unrelated-share-get-event-or-reconnect", func(t *testing.T) {
		a, _, _ := setup(t)
		a.hub.Load().BeginShare(wAlicePi1.ID, nil).Applied(ops.ShareResult{Rule: all, Revision: "9", OpBarrier: "99999999999999999"})
		if a.readable(t, privSess) {
			t.Fatal("a share_get settled a restriction")
		}
		_, f := a.connect(t, decl(10, "all"), privSess)
		if a.readable(t, privSess) {
			t.Fatal("a new connection's declaration settled a restriction")
		}
		if err := f.Apply(Change{Seq: 1, Event: EventShareChanged, Share: decl(11, "all")}); err != nil {
			t.Fatal(err)
		}
		if a.readable(t, privSess) {
			t.Fatal("an event declaring a newer revision settled a restriction")
		}
	})
	t.Run("a-probe-whose-barrier-is-too-low", func(t *testing.T) {
		a, _, _ := setup(t)
		l, _ := a.connect(t, decl(2, "all"), privSess)
		l.SetShareProbe(func(context.Context, uint64) (ops.ShareProbe, error) {
			return ops.ShareProbe{Rule: all, Revision: "5", OpBarrier: "1"}, nil
		})
		if err := a.hub.Load().ConfirmShare(context.Background(), wAlicePi1.ID); err != nil {
			t.Fatal(err)
		}
		if a.readable(t, privSess) {
			t.Fatal("a probe that did not raise the barrier past the restriction settled it")
		}
	})
	t.Run("a-newer-successful-share-set-supersedes-the-uncertainty", func(t *testing.T) {
		a, older, later := setup(t)
		_ = older
		newest := rule("select", privSess)
		x := a.hub.Load().BeginShare(wAlicePi1.ID, &newest)
		if a.readable(t, privSess) {
			t.Fatal("the older uncertain restriction was lifted when a new attempt began")
		}
		x.Applied(ops.ShareResult{Rule: newest, Revision: "5", OpBarrier: strconv.FormatUint(x.Seq(), 10)})
		if !a.readable(t, privSess) {
			t.Fatal("an explicit, newer and successful share_set did not supersede the older uncertainty")
		}
		_ = later
		if got := pendingRows(t, a); len(got) != 0 {
			t.Fatalf("pending rows after the superseding answer: %+v", got)
		}
	})
	t.Run("an-answer-without-the-barrier-settles-only-itself", func(t *testing.T) {
		a, _, _ := setup(t)
		newest := rule("select", privSess)
		x := a.hub.Load().BeginShare(wAlicePi1.ID, &newest)
		x.Applied(ops.ShareResult{Rule: newest, Revision: "5"}) // a Worker that cannot prove the order
		if a.readable(t, privSess) {
			t.Fatal("an answer that proves no order settled older uncertain restrictions")
		}
	})
	t.Run("a-newer-attempt-keeps-the-intersecting-older-restriction-while-in-flight", func(t *testing.T) {
		a, _, _ := setup(t)
		wide := rule("all")
		x := a.hub.Load().BeginShare(wAlicePi1.ID, &wide)
		if a.readable(t, privSess) {
			t.Fatal("a widening attempt lifted the older restriction before it was settled")
		}
		x.Refused()
		if a.readable(t, privSess) {
			t.Fatal("refusing the new attempt lifted the older restriction")
		}
	})
	t.Run("a-probe-answered-on-a-replaced-connection-is-discarded", func(t *testing.T) {
		a, _, _ := setup(t)
		l, _ := a.connect(t, decl(2, "all"), privSess)
		release := make(chan struct{})
		l.SetShareProbe(func(_ context.Context, barrier uint64) (ops.ShareProbe, error) {
			<-release
			return ops.ShareProbe{Rule: all, Revision: "2", OpBarrier: strconv.FormatUint(barrier, 10)}, nil
		})
		errc := make(chan error, 1)
		go func() { errc <- a.hub.Load().ConfirmShare(context.Background(), wAlicePi1.ID) }()
		time.Sleep(20 * time.Millisecond)
		a.connect(t, decl(2, "all"), privSess) // the Worker reconnects meanwhile
		close(release)
		if err := <-errc; !errors.Is(err, ErrSuperseded) {
			t.Fatalf("ConfirmShare = %v, want ErrSuperseded", err)
		}
		if a.readable(t, privSess) {
			t.Fatal("the answer of a connection that is gone settled a restriction")
		}
	})
}

// R2-03: a connected Worker's retained history is served only while the Worker's
// CURRENT policy shares it, asked on the same connection, with no fallback.
func newCurrentRig(t *testing.T) *authRig {
	t.Helper()
	a := &authRig{}
	a.pipeRig = newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Shared = func(w, s string) bool { return !a.hub.Load().Fenced(w, s) }
		c.SharedNow = func(ctx context.Context, w, s string) bool { return a.hub.Load().HistoryVisible(ctx, w, s) }
	})
	a.hub.Store(NewEventHub(EventHubOptions{Share: a.db}))
	return a
}

func TestConnectedHistoryFollowsTheWorkersCurrentPolicyWithoutWaitingForAnEvent(t *testing.T) {
	setup := func(t *testing.T) (*authRig, *Link) {
		a := newCurrentRig(t)
		l, _ := a.connect(t, decl(1, "all"), pipeSess, privSess)
		a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
		a.commit(t, wAlicePi1, pipeSess, "shared history")
		return a, l
	}
	t.Run("a-local-unshare-not-yet-announced", func(t *testing.T) {
		a, l := setup(t)
		var calls atomic.Int32
		l.SetShareProbe(proofProbe(rule("select", pipeSess), 2, &calls)) // the Worker's policy now; no event yet
		if a.readable(t, privSess) {
			t.Fatal("history served although the connected Worker's current policy does not share it")
		}
		if !a.readable(t, pipeSess) {
			t.Fatal("the session the policy still shares is hidden")
		}
		if calls.Load() == 0 {
			t.Fatal("the connected Worker was not asked")
		}
	})
	t.Run("a-failing-probe-has-no-cache-fallback", func(t *testing.T) {
		a, l := setup(t)
		l.SetShareProbe(func(context.Context, uint64) (ops.ShareProbe, error) {
			return ops.ShareProbe{}, errors.New("no answer")
		})
		if a.readable(t, privSess) || a.readable(t, pipeSess) {
			t.Fatal("history served from what the Hub stored although the connected check failed")
		}
	})
	t.Run("a-connection-replaced-during-the-check", func(t *testing.T) {
		a, l := setup(t)
		started, release := make(chan struct{}), make(chan struct{})
		l.SetShareProbe(func(context.Context, uint64) (ops.ShareProbe, error) {
			close(started)
			<-release
			return ops.ShareProbe{Rule: rule("all"), Revision: "1", OpBarrier: "0"}, nil
		})
		var wg sync.WaitGroup
		wg.Add(1)
		var got atomic.Bool
		go func() { defer wg.Done(); got.Store(a.readable(t, privSess)) }()
		<-started
		a.connect(t, decl(1, "all"), pipeSess, privSess) // replaced while asking
		close(release)
		wg.Wait()
		if got.Load() {
			t.Fatal("history served on the answer of a connection that was replaced meanwhile")
		}
	})
	t.Run("offline-known-history-needs-no-connection", func(t *testing.T) {
		a, l := setup(t)
		l.Disconnect()
		if !a.readable(t, privSess) || !a.readable(t, pipeSess) {
			t.Fatal("history of an offline Worker whose last known rule shares it was hidden")
		}
	})
	t.Run("a-private-session-is-not-exposed-by-the-probe", func(t *testing.T) {
		// The probe answers a rule to the Hub only: the public Event/WorkerSessions
		// shapes carry no declaration.
		a, l := setup(t)
		l.SetShareProbe(proofProbe(rule("select", pipeSess), 2, nil))
		a.readable(t, privSess)
		ws, _ := a.hub.Load().Sessions(wAlicePi1.ID)
		for _, s := range ws.Sessions {
			if s.Name == privSess {
				t.Fatalf("the unshared session is still listed: %+v", ws)
			}
		}
	})
}

// Turns that were fetched are not delivered after the current policy stopped
// sharing them: the check is repeated before every entry that leaves.
func TestAWaitBatchIsRecheckedAgainstTheCurrentPolicyAtEmission(t *testing.T) {
	a := newCurrentRig(t)
	l, _ := a.connect(t, decl(1, "all"), privSess)
	var private atomic.Bool
	l.SetShareProbe(func(_ context.Context, barrier uint64) (ops.ShareProbe, error) {
		if private.Load() { // the Worker's owner unshares locally: no event has arrived
			return ops.ShareProbe{Rule: rule("none"), Revision: "2", OpBarrier: strconv.FormatUint(barrier, 10)}, nil
		}
		return ops.ShareProbe{Rule: rule("all"), Revision: "1", OpBarrier: strconv.FormatUint(barrier, 10)}, nil
	})
	first := a.commit(t, wAlicePi1, privSess, "turn-1")
	a.commit(t, wAlicePi1, privSess, "turn-2")
	a.commit(t, wAlicePi1, privSess, "turn-3")
	var emitted []string
	err := a.p.Wait(context.Background(), authz.NewSessionPrincipal(idAlice),
		ops.PipeWaitArgs{Target: target(wAlicePi1, privSess), AfterTurnID: first, TimeoutMS: 1000},
		func(t ops.PipeTurn) error {
			emitted = append(emitted, t.Text)
			private.Store(true) // the policy changes while the first entry is being handed over
			return nil
		})
	wantNotShared(t, err, "the rest of the batch after the policy changed")
	if len(emitted) != 1 || emitted[0] != "turn-2" {
		t.Fatalf("emitted = %q: entries left after the connected Worker's policy stopped sharing the session", emitted)
	}
}
