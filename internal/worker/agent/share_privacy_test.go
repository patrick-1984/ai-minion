package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Privacy of the share operations on the Worker: what a read that was already
// running does when the rule changes under it, what an answer tells the Hub
// when the terminal cannot be read, how answers are ordered, and which Worker an
// operation may name. The Executor, the policy, the store and (for the event
// stream) the publisher are the real ones; only the terminal host is the
// existing in-memory fake, with a gate where a test needs to hold a read.

// gatedHost holds the first Snapshot of the target until the test releases it,
// and counts the Snapshots it was asked for.
type gatedHost struct {
	term.SessionHost
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	reads   atomic.Int32
}

func newGatedHost(h term.SessionHost) *gatedHost {
	return &gatedHost{SessionHost: h, entered: make(chan struct{}), release: make(chan struct{})}
}

func (h *gatedHost) Snapshot(ctx context.Context, p term.PaneRef) (ops.Screen, error) {
	h.reads.Add(1)
	h.once.Do(func() { close(h.entered); <-h.release })
	return h.SessionHost.Snapshot(ctx, p)
}

func (h *gatedHost) Capture(ctx context.Context, p term.PaneRef, lines int) (ops.Screen, error) {
	return h.SessionHost.Snapshot(ctx, p)
}

// A read that was already past its first check when share_set completed does
// not deliver what the pane held after that: the content of the screen it
// collects is discarded and the read is answered against the rule in force, for
// a session that is no longer shared the not_shared refusal.
func TestReadStartedBeforeShareSetNeverDeliversContentAfterIt(t *testing.T) {
	for _, op := range []string{ops.OpSessionSnapshot, ops.OpSessionCapture, ops.OpSessionsList} {
		t.Run(op, func(t *testing.T) {
			e := shareEnv(t, store.ShareSelection{Mode: store.ShareAll}, "private")
			ref, err := e.host.ResolvePane(context.Background(), target("private"))
			if err != nil {
				t.Fatal(err)
			}
			h := newGatedHost(e.host)
			e.ex.host = h
			var args any = ops.SessionSnapshotArgs{Target: target("private")}
			switch op {
			case ops.OpSessionCapture:
				args = ops.SessionCaptureArgs{Target: target("private")}
			case ops.OpSessionsList:
				args = ops.SessionsListArgs{}
			}
			type answer struct {
				raw string
				err error
			}
			done := make(chan answer, 1)
			go func() {
				b, err := e.ex.Execute(context.Background(), Request{Op: op, Actions: allActions, Args: mustJSON(t, args)})
				done <- answer{string(b), err}
			}()
			<-h.entered
			e.mustShare(ops.OpShareSet, shareSetArgs("none"))
			e.host.setScreen(ref, "SYNTHETIC-SECRET-AFTER-UNSHARE")
			close(h.release)
			got := <-done
			if got.err == nil && strings.Contains(got.raw, "SYNTHETIC-SECRET-AFTER-UNSHARE") {
				t.Fatalf("%s started while shared delivered content written after share_set returned: %s", op, got.raw)
			}
			if got.err != nil && got.raw != "" {
				t.Fatalf("an error carried data: %s %v", got.raw, got.err)
			}
			if op != ops.OpSessionsList {
				wantAPIError(t, got.err, "forbidden", "not_shared")
			} else if got.err != nil || strings.Contains(got.raw, "private") {
				t.Fatalf("sessions_list = %s %v, want a list without the session that is no longer shared", got.raw, got.err)
			}
		})
	}
}

// A rule that changed and changed back while a read ran does not make the
// first pass deliverable: it may have read the pane while it was private. The
// read is made again, against the rule that is in force, and delivers that.
func TestAReadThatSawTheRuleChangeIsMadeAgain(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareAll}, "private")
	h := newGatedHost(e.host)
	e.ex.host = h
	type answer struct {
		raw string
		err error
	}
	done := make(chan answer, 1)
	go func() {
		b, err := e.ex.Execute(context.Background(), Request{Op: ops.OpSessionSnapshot, Actions: allActions,
			Args: mustJSON(t, ops.SessionSnapshotArgs{Target: target("private")})})
		done <- answer{string(b), err}
	}()
	<-h.entered
	e.mustShare(ops.OpShareSet, shareSetArgs("none"))
	e.mustShare(ops.OpShareSet, shareSetArgs("all"))
	close(h.release)
	got := <-done
	if got.err != nil {
		t.Fatalf("the read that is shared again after the change was refused: %v", got.err)
	}
	if n := h.reads.Load(); n != 2 {
		t.Fatalf("the host was read %d times, want 2: the first pass must be discarded and the read made again", n)
	}
}

// A read that fails returns the failure and nothing else.
func TestAFailedReadCarriesNoData(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareAll}, "private")
	e.host.mu.Lock()
	e.host.listErr = errors.New("no server")
	e.host.mu.Unlock()
	for _, op := range []string{ops.OpSessionSnapshot, ops.OpSessionCapture, ops.OpSessionsList} {
		var args any = ops.SessionSnapshotArgs{Target: target("private")}
		switch op {
		case ops.OpSessionCapture:
			args = ops.SessionCaptureArgs{Target: target("private")}
		case ops.OpSessionsList:
			args = ops.SessionsListArgs{}
		}
		raw, err := e.ex.Execute(context.Background(), Request{Op: op, Actions: allActions, Args: mustJSON(t, args)})
		if op == ops.OpSessionsList && err == nil {
			t.Fatalf("%s with a failing terminal answered %s", op, raw)
		}
		if err != nil && len(raw) != 0 {
			t.Fatalf("%s returned data with an error: %s", op, raw)
		}
	}
}

// failingList is a host whose listing fails, as a tmux that went away does.
type failingList struct{ term.SessionHost }

func (failingList) ListSessions(context.Context) ([]term.Session, error) {
	return nil, errors.New("synthetic host list failure")
}

// A share_set whose change is made is answered with the rule, also when the
// terminal cannot be listed afterwards: the caller is not told "failed" for a
// change that stands. A failure before the change is an error and changes
// nothing; a share_get has nothing to show without the names and fails.
func TestShareSetThatCommitsIsNeverAnErrorBecauseTheListingFailed(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareAll}, "private")
	e.ex.host = failingList{e.host}
	args := shareSetArgs("none")
	args.WithRevision = true
	raw, err := e.call(ops.OpShareSet, operateActions, args)
	if err != nil {
		t.Fatalf("share_set after a committed change returned %v: the caller would be told it failed while the rule changed", err)
	}
	var res ops.ShareResult
	if err := json.Unmarshal(raw, &res); err != nil || res.Rule.Mode != "none" {
		t.Fatalf("answer = %s (%v), want the rule none", raw, err)
	}
	if e.saved().Mode != store.ShareNone {
		t.Fatal("setup: the change was not committed")
	}
	st, err := e.st.LoadShareStateContext(context.Background())
	if err != nil || res.RevisionNumber() != st.Revision || st.Revision < 2 {
		t.Fatalf("answer revision %q, stored %d (%v)", res.Revision, st.Revision, err)
	}
	if _, err := e.call(ops.OpShareGet, operateActions, shareGetArgs()); err == nil {
		t.Fatal("share_get answered without being able to list the sessions")
	}

	// Before the change: an invalid request and a refusal change nothing.
	bad := shareSetArgs("select")
	if _, err := e.call(ops.OpShareSet, operateActions, bad); err == nil || e.saved().Mode != store.ShareNone {
		t.Fatalf("an invalid share_set: %v, saved %+v", err, e.saved())
	}
}

// delayList holds the first listing, as a share_get that has read the rule and
// is not finished does.
type delayList struct {
	term.SessionHost
	started atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (h *delayList) ListSessions(ctx context.Context) ([]term.Session, error) {
	if h.started.CompareAndSwap(false, true) {
		close(h.entered)
		<-h.release
	}
	return h.SessionHost.ListSessions(ctx)
}

// An answer carries the durable revision of the rule it reports, so that a
// share_get that was delayed past a share_set can be told from the current rule
// by whoever applies it.
func TestAnAnswerCarriesTheRevisionSoALateOneCanBeOrdered(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareAll}, "private")
	h := &delayList{SessionHost: e.host, entered: make(chan struct{}), release: make(chan struct{})}
	e.ex.host = h
	get := shareGetArgs()
	get.WithRevision = true
	done := make(chan ops.ShareResult, 1)
	go func() {
		r, err := e.share(ops.OpShareGet, operateActions, get)
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	<-h.entered
	set := shareSetArgs("none")
	set.WithRevision = true
	newer := e.mustShare(ops.OpShareSet, set)
	close(h.release)
	older := <-done
	if older.Rule.Mode != "all" || newer.Rule.Mode != "none" {
		t.Fatalf("older %+v newer %+v", older, newer)
	}
	if older.RevisionNumber() == 0 || older.RevisionNumber() >= newer.RevisionNumber() {
		t.Fatalf("the delayed share_get has revision %q, the share_set that finished before it %q: an applier cannot tell them apart", older.Revision, newer.Revision)
	}
	// Without the Hub's request the answer is what it always was.
	plain, err := e.share(ops.OpShareGet, operateActions, shareGetArgs())
	if err != nil || plain.Revision != "" {
		t.Fatalf("a caller that is not the Hub got a revision: %+v %v", plain, err)
	}
}

// The rule is declared to the Hub on the event stream: in the first frame, on
// the first event of a look in which it changed, and alone when the look has no
// event; a failing terminal does not keep a change of the rule from the Hub.
func TestTheEventStreamDeclaresTheShareRule(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"open", "secret"} {
		ref := e.host.addSession(n, "claude")
		e.host.setScreen(ref, "content of "+n)
	}
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"open"}}); err != nil {
		t.Fatal(err)
	}
	obs, err := NewHostObserver(HostObserverConfig{Host: e.host, Classifier: e.cls, Policy: e.policy})
	if err != nil {
		t.Fatal(err)
	}
	clk := newManualClock()
	pub, _ := NewEventPublisher(EventConfig{Observer: obs, Interval: time.Second, Clock: clk})
	st := newFakeEvStream()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pub.serve(ctx, func(context.Context) (EventStream, error) { return st, nil })
		close(done)
	}()
	defer func() { cancel(); <-done }()

	snap := st.snapshot(t)
	first, _ := e.st.LoadShareStateContext(context.Background())
	if snap.Share == nil || snap.Share.Rule.Mode != "select" || !slices.Equal(snap.Share.Rule.Names, []string{"open"}) || snap.Share.Revision != first.Revision {
		t.Fatalf("snapshot rule = %+v, stored %+v", snap.Share, first)
	}

	// A change that adds an event: the rule is on that event, ahead of it.
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareAll}); err != nil {
		t.Fatal(err)
	}
	clk.tick(t, time.Second)
	ev := st.event(t)
	if ev.Event != EventSessionAdded || ev.Session != "secret" || ev.Share == nil || ev.Share.Rule.Mode != "all" || ev.Share.Revision <= first.Revision {
		t.Fatalf("event = %+v (share %+v), want session_added secret carrying rule all", ev, ev.Share)
	}
	st.none(t)

	// A change with no event to ride on (closed session names): a frame of its own.
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"open", "secret", "closed-one"}}); err != nil {
		t.Fatal(err)
	}
	clk.tick(t, time.Second)
	ev = st.event(t)
	if ev.Event != EventShareChanged || ev.Session != "" || ev.Share == nil || ev.Share.Rule.Mode != "select" || !slices.Contains(ev.Share.Rule.Names, "closed-one") {
		t.Fatalf("event = %+v", ev)
	}

	// The sessions cannot be listed: the rule still reaches the Hub.
	e.host.mu.Lock()
	e.host.listErr = errors.New("no server")
	e.host.mu.Unlock()
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareNone}); err != nil {
		t.Fatal(err)
	}
	clk.tick(t, time.Second)
	ev = st.event(t)
	if ev.Event != EventShareChanged || ev.Share == nil || ev.Share.Rule.Mode != "none" {
		t.Fatalf("event with a failing terminal = %+v", ev)
	}
	// Nothing is repeated while nothing changes.
	clk.tick(t, time.Second)
	st.none(t)
}

// An operation that names another Worker is refused and changes nothing, whoever
// asks; an operation that names this Worker works.
func TestShareOperationsAreHeldToTheWorkerThatRunsThem(t *testing.T) {
	const foreign = "01900000-0000-7000-8000-0000000000ff"
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareNone}, "private")
	for _, op := range []string{ops.OpShareGet, ops.OpShareSet} {
		var args any = ops.ShareGetArgs{WorkerID: foreign}
		if op == ops.OpShareSet {
			args = ops.ShareSetArgs{WorkerID: foreign, Mode: "all"}
		}
		_, err := e.call(op, operateActions, args)
		wantAPIError(t, err, string(proto.CodeForbidden), "")
		if e.saved().Mode != store.ShareNone {
			t.Fatalf("%s naming another Worker changed this one: %+v", op, e.saved())
		}
	}
	if _, err := e.call(ops.OpShareSet, operateActions, shareSetArgs("all")); err != nil || e.saved().Mode != store.ShareAll {
		t.Fatalf("share_set naming this Worker: %v, saved %+v", err, e.saved())
	}

	// A Worker that has no Hub identity cannot confirm any id: it refuses all.
	e.policy.SetWorkerID("")
	_, err := e.call(ops.OpShareSet, operateActions, shareSetArgs("none"))
	wantAPIError(t, err, string(proto.CodeForbidden), "")
	if e.saved().Mode != store.ShareAll {
		t.Fatalf("a Worker without an identity changed its sharing: %+v", e.saved())
	}
}
