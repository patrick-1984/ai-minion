//go:build e2e && unix

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

// Story 4.15 residual privacy findings of the second review, composed: the
// default HTTPS Hub (strict, SQLite), a real Worker over a private tmux, the real
// op server and event publisher. The event interval is held at one hour in the
// tests that must not be helped by a poll.

func startShareResidual(t *testing.T, interval time.Duration, hook func(*agent.Executor) agent.OpExecutor) *shareE2E {
	t.Helper()
	e := &shareE2E{t: t, targets: map[string]string{}}
	e.ph = startShareHub(t)
	e.ownerID, e.owner = e.user("olivia")
	_, e.foreign = e.user("fred")

	// The Worker: state, key, enrollment, tmux, executor, op server, events.
	dir := t.TempDir()
	st, err := wstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.wstore = st
	if err := st.SaveShare(wstore.ShareSelection{Mode: wstore.ShareAll}); err != nil {
		t.Fatal(err)
	}
	enr := agent.NewEnrollments(st, dir)
	key, err := enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := enr.Commit(context.Background(), agent.Enrollment{Pin: identity.Pin{Pub: e.ph.wsv.Identity().Public()}}); err != nil {
		t.Fatal(err)
	}
	e.wid = store.NewID()
	if err := e.ph.st.InsertWorker(context.Background(), nil, store.WorkerRow{ID: e.wid, Pub: key.Public(), Name: "pi", OwnerID: e.ownerID,
		Kind: store.WorkerKindPrivate, CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	e.tmux = tmuxtest.New(t)
	for _, n := range []string{"alpha", "beta"} {
		e.targets[n] = e.tmux.NewSession(n, "cat")
	}
	host, err := tmux.New(e.tmux.Socket())
	if err != nil {
		t.Fatal(err)
	}
	e.policy = agent.NewPolicy(st)
	ex, err := agent.NewExecutor(agent.Config{Host: host, Classifier: agentdetect.New(), State: st, Policy: e.policy, LockWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var executor agent.OpExecutor = ex
	if hook != nil {
		executor = hook(ex)
	}
	srv, err := agent.NewOpServer(agent.OpServerConfig{Executor: executor, Policy: e.policy})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := agent.NewHostObserver(agent.HostObserverConfig{Host: host, Classifier: agentdetect.New(), Policy: e.policy})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := agent.NewEventPublisher(agent.EventConfig{Observer: obs, Interval: interval})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := agent.NewConnector(agent.ConnectorConfig{
		HubURL: "https://" + e.ph.addr, Name: "pi", Ops: ex.Ops, Key: enr.LoadKey, Enrollment: enr, Policy: e.policy,
		Dialer:   agent.NewHubDialer(),
		Backoff:  &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		IdlePoll: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn.OnLink("op-server", srv.Serve)
	conn.OnLink("events", pub.Serve)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = srv.Stop(context.Background())
	})
	waitFor(t, "the Worker online", func() bool { return e.ph.wsv.Registry().IsOnline(e.wid) })
	waitFor(t, "both sessions in the Hub", func() bool {
		ws, ok := e.ph.events.Sessions(e.wid)
		return ok && len(ws.Sessions) == 2
	})
	return e
}

// R2-03: a connected Worker's local unshare is honoured by the very next read of
// retained history, with no event poll in between (the interval is one hour).
func TestResidualLocalUnshareIsHonouredBeforeAnyEventPoll(t *testing.T) {
	e := startShareResidual(t, time.Hour, nil)
	e.putTurn("beta", privateReply+"-beta")
	if !e.historyShown("beta") {
		t.Fatal("setup: beta history is shared")
	}
	if err := e.wstore.SaveShare(wstore.ShareSelection{Mode: wstore.ShareSelect, Sessions: []string{"alpha"}}); err != nil {
		t.Fatal(err)
	}
	native, nb := e.read(e.owner, ops.OpSessionSnapshot, "beta")
	if native != 403 {
		t.Fatalf("native setup %d %s", native, nb)
	}
	code, body := e.read(e.owner, ops.OpPipeRead, "beta")
	if code == 200 || strings.Contains(body, privateReply) {
		t.Fatalf("LOCAL REVOCATION WINDOW: history served after the local SaveShare returned: %d %s", code, body)
	}
	if code, _ := e.read(e.owner, ops.OpPipeRead, "alpha"); code != 200 {
		t.Fatalf("the session that is still shared became unreadable: %d", code)
	}
}

// residualExecutor wraps the real executor with hooks around share_set and an
// optional answer to the Hub's internal share probe (the op server asks the
// executor for it).
type residualExecutor struct {
	inner *agent.Executor
	// before runs ahead of the real share_set; a non-nil error is returned
	// without the executor having run (the change is not applied).
	before func(mode string) error
	// after runs once the real share_set committed; a non-nil error is returned
	// instead of its answer (the change IS applied, the answer is lost).
	after func(mode string) error
	// probeOK lets the real executor answer the probe; otherwise it fails.
	probeOK atomic.Bool
}

func (x *residualExecutor) Execute(ctx context.Context, rq agent.Request) (json.RawMessage, error) {
	if rq.Op != ops.OpShareSet {
		return x.inner.Execute(ctx, rq)
	}
	var a ops.ShareSetArgs
	_ = json.Unmarshal(rq.Args, &a)
	if x.before != nil {
		if err := x.before(a.Mode); err != nil {
			return nil, err
		}
	}
	b, err := x.inner.Execute(ctx, rq)
	if err == nil && x.after != nil {
		if aerr := x.after(a.Mode); aerr != nil {
			return nil, aerr
		}
	}
	return b, err
}

func (x *residualExecutor) ShareProbe(ctx context.Context, barrier uint64) (ops.ShareProbe, error) {
	if !x.probeOK.Load() {
		return ops.ShareProbe{}, proto.NewError(proto.CodeInternal, "synthetic: reconciliation unavailable")
	}
	return x.inner.ShareProbe(ctx, barrier)
}

func residualHook(x *residualExecutor) func(*agent.Executor) agent.OpExecutor {
	return func(ex *agent.Executor) agent.OpExecutor { x.inner = ex; return x }
}

// R2-02: an older answer released after a newer revocation whose outcome is
// unknown cannot reopen the history, although the real Worker persisted none.
func TestResidualAnOlderAnswerCannotReopenAnUnknownLaterRevocation(t *testing.T) {
	oldReady, releaseOld := make(chan struct{}), make(chan struct{})
	x := &residualExecutor{}
	x.after = func(mode string) error {
		switch mode {
		case "all":
			close(oldReady)
			<-releaseOld
		case "none":
			return proto.NewError(proto.CodeInternal, "synthetic unusable post-commit reply")
		}
		return nil
	}
	e := startShareResidual(t, time.Hour, residualHook(x))
	e.putTurn("beta", privateReply+"-beta")
	done := make(chan int, 1)
	go func() { code, _, _ := e.shareSet(e.owner, "all"); done <- code }()
	select {
	case <-oldReady:
	case <-time.After(15 * time.Second):
		t.Fatal("the older share_set never reached the Worker")
	}
	if code, _, _ := e.shareSet(e.owner, "none"); code == 200 {
		t.Fatal("setup requires an ambiguous post-commit failure")
	}
	if !e.historyHidden("beta") {
		t.Fatal("setup: the unknown restriction is not in force")
	}
	saved, _ := e.wstore.LoadShareStateContext(context.Background())
	if saved.Mode != wstore.ShareNone {
		t.Fatalf("setup: the real Worker persisted %s", saved.Mode)
	}
	close(releaseOld)
	if code := <-done; code != 200 {
		t.Fatalf("the older change answered %d", code)
	}
	if code, body := e.read(e.owner, ops.OpPipeRead, "beta"); code == 200 && strings.Contains(body, privateReply) {
		t.Fatalf("REAL STALE ANSWER LEAK: the Worker persisted none but a delayed older answer reopened history: %s", body)
	}
	// The real Worker then proves what it holds (none): beta stays private.
	x.probeOK.Store(true)
	if code, body := e.read(e.owner, ops.OpPipeRead, "beta"); code == 200 && strings.Contains(body, privateReply) {
		t.Fatalf("history served although the Worker's proven rule is none: %s", body)
	}
}

// R2-01/R2-02 settlement: a revocation whose answer was lost and which the Worker
// in fact never applied is settled by the Worker's own proof (the probe's
// barrier), not by a revision or a reconnect; before the proof the history is
// hidden, after it the Worker's real rule (all) governs again. No call is
// replayed.
func TestResidualAnUnknownRestrictionIsSettledByTheWorkersProof(t *testing.T) {
	x := &residualExecutor{}
	var sets atomic.Int32
	x.before = func(mode string) error {
		sets.Add(1)
		if mode == "none" {
			return proto.NewError(proto.CodeInternal, "synthetic: failed without saying whether it changed anything")
		}
		return nil
	}
	e := startShareResidual(t, time.Hour, residualHook(x))
	e.putTurn("beta", privateReply+"-beta")
	if code, _, _ := e.shareSet(e.owner, "none"); code == 200 {
		t.Fatal("setup requires an ambiguous failure")
	}
	if !e.historyHidden("beta") {
		t.Fatal("history served while a restriction of unknown outcome was unsettled")
	}
	x.probeOK.Store(true)
	waitFor(t, "the Worker's proof settles the restriction", func() bool { return e.historyShown("beta") })
	if n := sets.Load(); n != 1 {
		t.Fatalf("the Hub replayed the change: %d share_set calls reached the Worker", n)
	}
}
