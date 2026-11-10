package workers

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// These tests use the real EventHub with the real Hub database as its share
// store and the real Pipe over the same database: nothing of the privacy
// decision is replaced by a fake. They cover the share rule the Hub keeps of
// every Worker (events_share.go): history of a session that is not shared stays
// hidden across reconnects, restarts, local changes and answers that do not
// list it, and nothing a late, replayed or lost answer says can lift it.

const privSess = "beta"

// authRig is a Pipe whose "is this session still shown" decision is the
// EventHub's own, over the Hub database.
type authRig struct {
	*pipeRig
	hub atomic.Pointer[EventHub]
}

func newAuthRig(t *testing.T) *authRig {
	t.Helper()
	a := &authRig{}
	a.pipeRig = newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Shared = func(w, s string) bool { return !a.hub.Load().Fenced(w, s) }
	})
	a.hub.Store(NewEventHub(EventHubOptions{Share: a.db}))
	return a
}

// restart is a Hub restart: a new EventHub and a new Pipe over the same
// database. Nothing is carried over in memory.
func (a *authRig) restart(t *testing.T) {
	t.Helper()
	a.hub.Store(NewEventHub(EventHubOptions{Share: a.db}))
	p, err := NewPipe(PipeConfig{Store: a.st, Authz: a.az, Router: a.rt, Trust: a.trust, Now: a.clock.Now,
		Shared: func(w, s string) bool { return !a.hub.Load().Fenced(w, s) }})
	if err != nil {
		t.Fatal(err)
	}
	a.p = p
}

func rule(mode string, names ...string) ops.ShareRule { return ops.ShareRule{Mode: mode, Names: names} }

func decl(rev uint64, mode string, names ...string) *ops.ShareDecl {
	return &ops.ShareDecl{Rule: rule(mode, names...), Revision: rev}
}

// connect is a new connection of the Worker that declares d in its snapshot.
func (a *authRig) connect(t *testing.T, d *ops.ShareDecl, live ...string) (*Link, *Feed) {
	t.Helper()
	l, f := newFeed(t, a.hub.Load(), wAlicePi1.ID)
	var ss []SessionState
	for _, n := range live {
		ss = append(ss, SessionState{Name: n})
	}
	if err := f.SnapshotShare(0, ss, d); err != nil {
		t.Fatal(err)
	}
	return l, f
}

func (a *authRig) readable(t *testing.T, session string) bool {
	t.Helper()
	got, err := a.p.Read(context.Background(), authz.NewSessionPrincipal(idAlice), ops.PipeReadArgs{Target: target(wAlicePi1, session)})
	if err != nil {
		wantNotShared(t, err, "read of "+session)
		return false
	}
	return len(got.Turns) > 0
}

// answer settles a share_set that asked for next and was answered with rev.
func (a *authRig) answer(t *testing.T, rev uint64, next ops.ShareRule, unshared ...string) {
	t.Helper()
	call := a.hub.Load().BeginShare(wAlicePi1.ID, &next)
	call.Applied(ops.ShareResult{Rule: next, Unshared: unshared, Revision: ops.FormatShareRevision(rev)})
}

// The history of a session the Worker does not share stays hidden whatever the
// Worker lists afterwards, whatever a later answer names, after a restart of the
// Hub, and after a change that was made on the Worker's machine; it is served
// again only when the Worker's rule shares the session. This is what the first
// review reproduced against the version that fenced by the live list.
func TestPrivateHistoryFollowsTheWorkersRuleNotTheLiveList(t *testing.T) {
	setup := func(t *testing.T) *authRig {
		a := newAuthRig(t)
		a.connect(t, decl(1, "all"), pipeSess, privSess)
		a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
		a.commit(t, wAlicePi1, pipeSess, "shared history")
		if !a.readable(t, privSess) || !a.readable(t, pipeSess) {
			t.Fatal("both sessions are shared at the start")
		}
		a.answer(t, 2, rule("select", pipeSess), privSess)
		if a.readable(t, privSess) || !a.readable(t, pipeSess) {
			t.Fatal("share_set select did not hide exactly the unshared session")
		}
		return a
	}

	t.Run("reconnect-with-the-session-omitted", func(t *testing.T) {
		a := setup(t)
		a.connect(t, decl(2, "select", pipeSess), pipeSess) // a new connection: its snapshot omits beta
		if a.readable(t, privSess) || !a.readable(t, pipeSess) {
			t.Fatal("a reconnect that omitted the session lifted its fence")
		}
	})
	t.Run("reconnect-before-the-rule-is-declared", func(t *testing.T) {
		a := newAuthRig(t)
		a.connect(t, decl(2, "select", pipeSess), pipeSess)
		a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
		a.commit(t, wAlicePi1, pipeSess, "shared history")
		a.hub.Load().Connect(wAlicePi1.ID, "pi-1") // the connection is new; the Worker has not spoken yet
		if a.readable(t, privSess) || a.readable(t, pipeSess) {
			t.Fatal("history was served while the connection had not declared its rule: the policy is unknown, so nothing is shown")
		}
	})
	t.Run("hub-restart", func(t *testing.T) {
		a := setup(t)
		a.restart(t) // nothing in memory; the Worker is not connected yet
		if a.readable(t, privSess) {
			t.Fatal("a restarted Hub served the history of an unshared session")
		}
		if !a.readable(t, pipeSess) {
			t.Fatal("a restarted Hub lost the rule that shares the other session (offline history stays readable)")
		}
		a.connect(t, decl(2, "select", pipeSess), pipeSess)
		if a.readable(t, privSess) || !a.readable(t, pipeSess) {
			t.Fatal("the Worker reconnecting after the restart changed what is shown")
		}
	})
	t.Run("session-closed-on-the-worker", func(t *testing.T) {
		a := setup(t)
		_, f := a.connect(t, decl(2, "select", pipeSess), pipeSess)
		if err := f.Apply(Change{Seq: 1, Event: EventSessionRemoved, Session: SessionState{Name: privSess}}); err != nil {
			t.Fatal(err)
		}
		if a.readable(t, privSess) {
			t.Fatal("the closing of an unshared session lifted its fence")
		}
	})
	t.Run("a-later-answer-does-not-list-the-closed-session", func(t *testing.T) {
		a := setup(t)
		// share_get answers the same rule; the session is closed, so the names
		// it lists as unshared leave it out. The rule decides, not the names.
		a.hub.Load().BeginShare(wAlicePi1.ID, nil).Applied(ops.ShareResult{Rule: rule("select", pipeSess), Revision: "2"})
		if a.readable(t, privSess) {
			t.Fatal("an answer that did not list the closed session lifted its fence")
		}
	})
	t.Run("changed-on-the-workers-machine", func(t *testing.T) {
		a := newAuthRig(t)
		_, f := a.connect(t, decl(1, "all"), pipeSess, privSess)
		a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
		// The owner changes the rule on the Worker: no Hub call, the Worker tells
		// the Hub on its stream, ahead of the events the new rule shows.
		if err := f.Apply(Change{Seq: 1, Event: EventShareChanged, Share: decl(2, "select", pipeSess)}); err != nil {
			t.Fatal(err)
		}
		if a.readable(t, privSess) {
			t.Fatal("a change made on the Worker did not reach the Hub")
		}
		if ws, _ := a.hub.Load().Sessions(wAlicePi1.ID); names(ws) != pipeSess {
			t.Fatalf("live sessions = %q, want only %s", names(ws), pipeSess)
		}
		// A frame that carries the rule rides on the first event of the look.
		if err := f.Apply(Change{Seq: 2, Event: EventSessionAdded, Session: SessionState{Name: privSess}, Share: decl(3, "all")}); err != nil {
			t.Fatal(err)
		}
		if !a.readable(t, privSess) {
			t.Fatal("the Worker shares the session again (all): its stored history is served again")
		}
	})
}

// An answer that is older than the rule the Hub holds, or that comes from a
// connection that is gone, changes nothing: the order of rules is the Worker's
// revision, not the order in which answers arrive.
func TestAnOlderOrForeignAnswerCannotUndoANewerRule(t *testing.T) {
	t.Run("delayed-get-after-set", func(t *testing.T) {
		a := newAuthRig(t)
		a.connect(t, decl(1, "all"), privSess)
		a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
		old := a.hub.Load().BeginShare(wAlicePi1.ID, nil) // share_get dispatched, read mode all...
		a.answer(t, 2, rule("none"), privSess)            // ...share_set none completes meanwhile...
		old.Applied(ops.ShareResult{Rule: rule("all"), Revision: "1"})
		if a.readable(t, privSess) {
			t.Fatal("a delayed share_get undid the newer share_set")
		}
	})
	t.Run("get-answered-from-a-replaced-connection", func(t *testing.T) {
		a := newAuthRig(t)
		a.connect(t, decl(1, "all"), privSess)
		a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
		old := a.hub.Load().BeginShare(wAlicePi1.ID, nil)
		// The Worker reconnects with its database replaced: revision 1 again,
		// nothing shared. An answer of the old connection with a HIGHER number
		// must not be taken for newer.
		a.hub.Load().Forget(wAlicePi1.ID)
		a.connect(t, decl(1, "none"))
		old.Applied(ops.ShareResult{Rule: rule("all"), Revision: "9"})
		if a.readable(t, privSess) {
			t.Fatal("an answer from a replaced connection was applied")
		}
		if d, _ := a.hub.Load().ShareDeclOf(wAlicePi1.ID); d.Rule.Mode != "none" {
			t.Fatalf("held rule = %+v", d)
		}
	})
	t.Run("an-older-declaration-on-the-same-connection", func(t *testing.T) {
		a := newAuthRig(t)
		_, f := a.connect(t, decl(5, "none"), privSess)
		_ = f.Apply(Change{Seq: 1, Event: EventShareChanged, Share: decl(3, "all")})
		if d, _ := a.hub.Load().ShareDeclOf(wAlicePi1.ID); d.Rule.Mode != "none" || d.Revision != 5 {
			t.Fatalf("held rule = %+v, an older declaration replaced a newer rule", d)
		}
	})
}

// A restriction that was requested is in force from before the request is
// dispatched; a refusal that happened before anything could change lifts it; a
// call that ends without a usable answer keeps it, also across a connection that
// is replaced, until the Worker declares its rule.
func TestARequestedRestrictionIsSettledOnlyByItsOutcome(t *testing.T) {
	setup := func(t *testing.T) (*authRig, *Feed) {
		a := newAuthRig(t)
		_, f := a.connect(t, decl(1, "all"), pipeSess, privSess)
		a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
		a.commit(t, wAlicePi1, pipeSess, "shared history")
		if !a.readable(t, privSess) || !a.readable(t, pipeSess) {
			t.Fatal("setup: both sessions are shared")
		}
		return a, f
	}
	none := rule("none")

	t.Run("in-flight-hides-history-at-once-and-refusal-gives-it-back", func(t *testing.T) {
		a, _ := setup(t)
		call := a.hub.Load().BeginShare(wAlicePi1.ID, &none)
		if a.readable(t, privSess) {
			t.Fatal("history was readable while a restriction was being dispatched")
		}
		if ws, _ := a.hub.Load().Sessions(wAlicePi1.ID); names(ws) != pipeSess+","+privSess && names(ws) != privSess+","+pipeSess {
			t.Fatalf("a restriction that is only in flight emptied the live list: %q", names(ws))
		}
		call.Refused()
		if !a.readable(t, privSess) {
			t.Fatal("a refusal before anything changed did not give the history back")
		}
	})
	t.Run("unknown-outcome-keeps-the-restriction", func(t *testing.T) {
		a, f := setup(t)
		call := a.hub.Load().BeginShare(wAlicePi1.ID, &none)
		call.Unknown() // the connection was lost after the request went out
		if a.readable(t, privSess) || a.readable(t, pipeSess) {
			t.Fatal("history was readable after a restriction of unknown outcome")
		}
		if ws, _ := a.hub.Load().Sessions(wAlicePi1.ID); len(ws.Sessions) != 0 {
			t.Fatalf("live sessions after an unknown restriction = %q: their metadata would outlive the unshare", names(ws))
		}
		// An answer of an unrelated share_get with a higher revision proves
		// nothing about the restriction (it may be older than the change whose
		// outcome is unknown): the restriction stays.
		a.hub.Load().BeginShare(wAlicePi1.ID, nil).Applied(ops.ShareResult{Rule: rule("select", pipeSess), Revision: "2"})
		if a.readable(t, privSess) || a.readable(t, pipeSess) {
			t.Fatal("an unrelated answer with a higher revision settled a restriction of unknown outcome")
		}
		// The Worker's answer to a probe that raised its barrier to the sequence
		// number of the change is the proof: the rule in it is the rule in force.
		l, _ := a.connect(t, decl(2, "select", pipeSess), pipeSess, privSess)
		l.SetShareProbe(func(_ context.Context, barrier uint64) (ops.ShareProbe, error) {
			return ops.ShareProbe{Rule: rule("select", pipeSess), Revision: "2", OpBarrier: strconv.FormatUint(barrier, 10)}, nil
		})
		if err := a.hub.Load().ConfirmShare(context.Background(), wAlicePi1.ID); err != nil {
			t.Fatal(err)
		}
		if a.readable(t, privSess) || !a.readable(t, pipeSess) {
			t.Fatal("the proof of the Worker did not settle the restriction with its rule")
		}
		_ = f
	})
	t.Run("unknown-outcome-survives-the-lost-connection-and-settles-by-proof-on-the-next-one", func(t *testing.T) {
		a, _ := setup(t)
		a.hub.Load().BeginShare(wAlicePi1.ID, &none).Unknown()
		a.hub.Load().Connect(wAlicePi1.ID, "pi-1") // reconnecting
		if a.readable(t, privSess) {
			t.Fatal("history readable between the connection and its declaration")
		}
		l, _ := a.connect(t, decl(1, "all"), pipeSess, privSess) // its declaration alone proves nothing
		if a.readable(t, privSess) {
			t.Fatal("a new connection's declaration settled a restriction of unknown outcome without proof")
		}
		l.SetShareProbe(func(_ context.Context, barrier uint64) (ops.ShareProbe, error) {
			return ops.ShareProbe{Rule: rule("all"), Revision: "1", OpBarrier: strconv.FormatUint(barrier, 10)}, nil
		})
		if err := a.hub.Load().ConfirmShare(context.Background(), wAlicePi1.ID); err != nil {
			t.Fatal(err)
		}
		if !a.readable(t, privSess) {
			t.Fatal("the Worker proved it never applied the change (rule all) and the history was not given back")
		}
	})
	t.Run("an-answer-from-a-replaced-connection-does-not-settle", func(t *testing.T) {
		a, _ := setup(t)
		call := a.hub.Load().BeginShare(wAlicePi1.ID, &none)
		a.hub.Load().Connect(wAlicePi1.ID, "pi-1")
		call.Applied(ops.ShareResult{Rule: none, Revision: "2"})
		if a.readable(t, privSess) {
			t.Fatal("history was readable after the call whose connection was replaced")
		}
	})
	t.Run("a-successful-answer-applies-its-rule-before-it-returns", func(t *testing.T) {
		a, _ := setup(t)
		a.answer(t, 2, none, pipeSess, privSess)
		if a.readable(t, privSess) || a.readable(t, pipeSess) {
			t.Fatal("history readable after share_set none returned")
		}
		d, ok := a.hub.Load().ShareDeclOf(wAlicePi1.ID)
		if !ok || d.Rule.Mode != "none" || d.Revision != 2 {
			t.Fatalf("held rule = %+v %v", d, ok)
		}
	})
}

// A rule the Hub could not make durable hides the Worker's history until it can.
type failingShares struct {
	ShareStore
	fail atomic.Bool
}

func (f *failingShares) SaveWorkerShare(ctx context.Context, id string, d ops.ShareDecl) error {
	if f.fail.Load() {
		return errors.New("disk full")
	}
	return f.ShareStore.SaveWorkerShare(ctx, id, d)
}

func TestARuleThatCannotBeWrittenHidesTheHistoryUntilItCan(t *testing.T) {
	a := newAuthRig(t)
	fs := &failingShares{ShareStore: a.db}
	a.hub.Store(NewEventHub(EventHubOptions{Share: fs}))
	a.connect(t, decl(1, "all"), privSess)
	a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
	if !a.readable(t, privSess) {
		t.Fatal("setup")
	}
	fs.fail.Store(true)
	a.answer(t, 2, rule("all"), nil...)
	if a.readable(t, privSess) {
		t.Fatal("history was served while the rule in force was not durable: a restart would lose it")
	}
	fs.fail.Store(false)
	if !a.readable(t, privSess) {
		t.Fatal("history stayed hidden after the rule could be written")
	}
	if m, err := a.db.LoadWorkerShares(context.Background()); err != nil || m[wAlicePi1.ID].Revision != 2 {
		t.Fatalf("stored rules = %+v, %v", m, err)
	}
}

// A declaration that is behind what the Hub holds (the Worker's database was
// restored) is accepted only if it is no wider than the rule the Hub holds.
func TestARollbackOfTheWorkersStateCannotWidenWhatIsShown(t *testing.T) {
	t.Run("wider-and-older-is-refused-until-the-owner-sets-a-rule", func(t *testing.T) {
		a := newAuthRig(t)
		a.connect(t, decl(5, "select", pipeSess), pipeSess)
		a.commit(t, wAlicePi1, privSess, "SYNTHETIC-PRIVATE-HISTORY")
		a.connect(t, decl(2, "all"), pipeSess, privSess) // restored from a backup: older and wider
		if a.readable(t, privSess) {
			t.Fatal("a Worker rolled back to an older, wider rule and the Hub showed history it had hidden")
		}
		// The owner's explicit share_set through the Hub is the way back.
		a.answer(t, 3, rule("all"))
		if !a.readable(t, privSess) {
			t.Fatal("the owner's share_set did not replace the rule after a refused rollback")
		}
	})
	t.Run("older-but-narrower-is-accepted", func(t *testing.T) {
		a := newAuthRig(t)
		a.connect(t, decl(5, "select", pipeSess), pipeSess)
		a.connect(t, decl(1, "none"))
		if d, _ := a.hub.Load().ShareDeclOf(wAlicePi1.ID); d.Rule.Mode != "none" || d.Revision != 1 {
			t.Fatalf("held rule = %+v: a Worker that shares less is believed", d)
		}
	})
}

// With a share store the Hub does not know what a Worker that declared no rule
// shares: its history is not shown. Without one (a Hub that keeps nothing) the
// older behaviour stays: what the Worker lists is shared.
func TestHistoryOfAWorkerWithoutADeclaredRuleIsNotShown(t *testing.T) {
	a := newAuthRig(t)
	l, f := newFeed(t, a.hub.Load(), wAlicePi1.ID)
	_ = l
	if err := f.Snapshot(0, []SessionState{{Name: privSess}}); err != nil { // an older Worker: no rule
		t.Fatal(err)
	}
	a.commit(t, wAlicePi1, privSess, "history")
	if a.readable(t, privSess) {
		t.Fatal("history was shown for a Worker whose rule is unknown")
	}
	if ws, _ := a.hub.Load().Sessions(wAlicePi1.ID); names(ws) != privSess {
		t.Fatalf("the live list of an older Worker = %q: it only ever reports shared sessions", names(ws))
	}
	if a.hub.Load().LiveFenced(wAlicePi1.ID, privSess) {
		t.Fatal("events of an older Worker were hidden")
	}

	plain := NewEventHub(EventHubOptions{})
	if plain.Fenced(wAlicePi1.ID, privSess) {
		t.Fatal("a hub without a share store hid history of a Worker that declared nothing")
	}
}

// A pipe_wait that has fetched a batch checks the rule before each turn it
// emits, not once for the batch: the emitter of the first turn can block while
// share_set completes, and the next turns are not delivered.
func TestPipeWaitChecksTheRuleBeforeEveryTurnOfABatch(t *testing.T) {
	a := newAuthRig(t)
	a.connect(t, decl(1, "all"), privSess)
	anchor := a.commit(t, wAlicePi1, privSess, "anchor")
	a.commit(t, wAlicePi1, privSess, "first")
	a.commit(t, wAlicePi1, privSess, "SYNTHETIC-POST-REVOCATION")
	n := 0
	err := a.p.Wait(context.Background(), authz.NewSessionPrincipal(idAlice),
		ops.PipeWaitArgs{Target: target(wAlicePi1, privSess), AfterTurnID: anchor, TimeoutMS: 50},
		func(turn ops.PipeTurn) error {
			n++
			if turn.Text == "SYNTHETIC-POST-REVOCATION" {
				t.Errorf("a turn was emitted after the revocation completed: %+v", turn)
			}
			if n == 1 {
				a.answer(t, 2, rule("none"), privSess) // share_set completes while the emitter is blocked
			}
			return nil
		})
	wantNotShared(t, err, "the wait that was revoked in the middle of its batch")
	if n != 1 {
		t.Fatalf("emitted %d turns, want exactly the one before the revocation", n)
	}
}
