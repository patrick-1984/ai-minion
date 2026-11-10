package workers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// --- fakes ---

type pipeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *pipeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *pipeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakePipeRouter records the calls and answers with a scripted result.
type fakePipeRouter struct {
	mu     sync.Mutex
	calls  []CallRequest
	result any
	err    error
}

func (r *fakePipeRouter) Call(_ context.Context, req CallRequest) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req)
	if r.err != nil {
		return nil, r.err
	}
	return r.result, nil
}

func (r *fakePipeRouter) last() CallRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

func (r *fakePipeRouter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func deliveredResult() ops.WriteResult { return ops.WriteResult{Outcome: ops.OutcomeDelivered} }

// fakeTrustStore is an in-memory authz.TrustStore.
type fakeTrustStore struct {
	mu   sync.Mutex
	rows map[[3]string]bool
	dir  *fakeDir
	err  error
}

func (f *fakeTrustStore) PutTrust(_ context.Context, r, w, s string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[[3]string]bool{}
	}
	f.rows[[3]string{r, w, s}] = true
	return nil
}

func (f *fakeTrustStore) PutTrustIfOwner(ctx context.Context, r, w, s string, at int64, _ string) error {
	return f.PutTrust(ctx, r, w, s, at)
}

func (f *fakeTrustStore) WorkerOwner(ctx context.Context, w string) (string, error) {
	return f.dir.WorkerOwner(ctx, w)
}

func (f *fakeTrustStore) DeleteTrust(_ context.Context, r, w, s string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := [3]string{r, w, s}
	had := f.rows[k]
	delete(f.rows, k)
	return had, nil
}

func (f *fakeTrustStore) HasTrust(_ context.Context, r, w, s string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.rows[[3]string{r, w, s}] || f.rows[[3]string{r, w, ""}], nil
}

func (f *fakeTrustStore) ListTrust(context.Context, string) ([]authz.TrustRecord, error) {
	return nil, nil
}

// --- rig ---

const pipeSess = "minion-1"

type pipeRig struct {
	p     *Pipe
	st    *store.PipeStore
	db    *store.Store
	rt    *fakePipeRouter
	trust *authz.Trust
	ts    *fakeTrustStore
	clock *pipeClock
	az    *authz.Authorizer
}

func newPipeRig(t *testing.T, budget int) *pipeRig { return newPipeRigWith(t, budget, nil) }

// newPipeRigWith is newPipeRig with a change to the pipe's configuration.
func newPipeRigWith(t *testing.T, budget int, mutate func(*PipeConfig)) *pipeRig {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	dir := &fakeDir{recs: []WorkerRecord{wAlicePi1, wAlicePi2, wBobPi1, wBobBox, wRootLab}}
	az := authz.New(fakeUsers{
		idAlice: {Role: authz.RoleUser, Active: true},
		idBob:   {Role: authz.RoleUser, Active: true},
		idRoot:  {Role: authz.RoleAdmin, Active: true},
	}, dir)
	ts := &fakeTrustStore{dir: dir}
	trust := authz.NewTrust(ts, nil)
	clock := &pipeClock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	rt := &fakePipeRouter{result: deliveredResult()}
	cfg := PipeConfig{
		Store: s.Pipe(), Authz: az, Router: rt, Trust: trust, BudgetPerDay: budget,
		Now: clock.Now, Poll: 5 * time.Millisecond, Logger: slog.New(slog.DiscardHandler),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := NewPipe(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &pipeRig{p: p, st: s.Pipe(), db: s, rt: rt, trust: trust, ts: ts, clock: clock, az: az}
}

func target(w WorkerRecord, session string) ops.Target {
	return ops.Target{WorkerID: w.ID, SessionName: session}
}

func sendArgs(w WorkerRecord, session, text string) ops.PipeSendArgs {
	return ops.PipeSendArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()},
		Target:    target(w, session), Text: text,
	}
}

func peerOfRec(w WorkerRecord, session string) pipe.Peer {
	return pipe.Peer{WorkerID: w.ID, SessionName: session}
}

func keyWith(owner string, actions ...ops.Action) authz.Principal {
	return authz.NewAPIKeyPrincipal(owner, scopeFunc(func(authz.ResolvedResource) []ops.Action { return actions }), "")
}

func modelAlice() authz.Principal {
	return keyWith(idAlice, ops.ActionRead, ops.ActionOperate, ops.ActionMessage)
}

// delivery returns the id of the message a reply to a session of w answers:
// the newest message of the session that has no reply yet, or, when there is
// none, a new one stored the way a Hub send stores it. The reply names it.
func (r *pipeRig) delivery(t *testing.T, w WorkerRecord, session string) int64 {
	t.Helper()
	ctx := context.Background()
	peer := peerOfRec(w, session)
	msgs, err := r.st.Messages(ctx, peer, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if st, err := r.st.CheckTurn(ctx, peer, "00000000-0000-7000-8000-000000000000", msgs[i].ID); err == nil && st == store.TurnStored {
			return msgs[i].ID
		}
	}
	m, err := r.st.AppendMessage(ctx, pipe.Message{Peer: peer, Text: "a message", At: r.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// commit stores a received turn the way ingestion would, without the delivery
// and quota checks that ingestion makes (those have tests of their own): the
// tests that use it are about what is read back.
func (r *pipeRig) commit(t *testing.T, w WorkerRecord, session, text string) string {
	t.Helper()
	id, err := pipe.NewTurnID(r.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.PutTurn(context.Background(), pipe.Turn{TurnID: id, Peer: peerOfRec(w, session), Text: text, At: r.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Millisecond)
	return id
}

// commitWire is a turn through the Hub's ingestion, answering a delivery.
func (r *pipeRig) commitWire(t *testing.T, w WorkerRecord, session, text string) string {
	t.Helper()
	id, err := pipe.NewTurnID(r.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	ack, err := r.p.IngestTurn(context.Background(), w.ID, TurnFrame{
		Type: FrameTurn, TurnID: id, Session: session, MessageID: r.delivery(t, w, session), Text: text, Time: ops.Timestamp(r.clock.Now()),
	})
	if err != nil || ack.Status != pipe.AckStored {
		t.Fatalf("IngestTurn = %+v, %v", ack, err)
	}
	r.clock.Advance(time.Millisecond)
	return id
}

// --- AC 1: pipe_send is the Hub's: checked, rate limited per peer, routed ---

func TestSendIsStoredAndRoutedAsAWriteOp(t *testing.T) {
	r := newPipeRig(t, 0)
	args := sendArgs(wAlicePi1, pipeSess, "hello agent")
	res, err := r.p.Send(context.Background(), authz.NewSessionPrincipal(idAlice), args)
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("Send = %+v, %v", res, err)
	}
	if r.rt.count() != 1 {
		t.Fatalf("%d calls routed, want 1", r.rt.count())
	}
	c := r.rt.calls[0]
	got, ok := c.Args.(ops.PipeSendArgs)
	if c.Op != ops.OpPipeSend || c.WorkerID != wAlicePi1.ID || c.Session != pipeSess || !ok || got.RequestID != args.RequestID || got.Text != "hello agent" {
		t.Fatalf("routed call = %+v", c)
	}
	msgs, _ := r.st.Messages(context.Background(), peerOfRec(wAlicePi1, pipeSess), 0, 0)
	if len(msgs) != 1 || msgs[0].Text != "hello agent" {
		t.Fatalf("stored messages = %+v", msgs)
	}
}

func TestSendNeedsMessageOrOperateOnTheSession(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		p    authz.Principal
		want proto.Code // "" = allowed
	}{
		{"owner in the browser", authz.NewSessionPrincipal(idAlice), ""},
		{"admin in the browser", authz.NewSessionPrincipal(idRoot), ""},
		{"key with message only", keyWith(idAlice, ops.ActionMessage), ""},
		{"key with operate only", keyWith(idAlice, ops.ActionOperate), ""},
		{"key with read only", keyWith(idAlice, ops.ActionRead), proto.CodeForbidden},
		{"key with no scope on the session", keyWith(idAlice), proto.CodeNotFound},
		{"another user", authz.NewSessionPrincipal(idBob), proto.CodeNotFound},
		{"a Worker identity of the same owner", authz.NewWorkerPrincipal(wAlicePi2.ID), ""},
		{"a Worker identity of another owner", authz.NewWorkerPrincipal(wBobPi1.ID), proto.CodeNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newPipeRig(t, 0)
			_, err := r.p.Send(ctx, tc.p, sendArgs(wAlicePi1, pipeSess, "hi"))
			msgs, _ := r.st.Messages(ctx, peerOfRec(wAlicePi1, pipeSess), 0, 0)
			if tc.want == "" {
				if err != nil || len(msgs) != 1 || r.rt.count() != 1 {
					t.Fatalf("err = %v, stored %d, routed %d", err, len(msgs), r.rt.count())
				}
				return
			}
			wantCode(t, err, tc.want)
			if len(msgs) != 0 || r.rt.count() != 0 {
				t.Fatalf("a refused send stored %d and routed %d", len(msgs), r.rt.count())
			}
		})
	}
}

func TestSendRateLimitIsPerPeerAndNeverQueues(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 1000)
	alice := authz.NewSessionPrincipal(idAlice)
	for i := 0; i < pipe.SendBurst; i++ {
		if _, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "x")); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	_, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "x"))
	wantCode(t, err, proto.CodePipeRateLimited)
	var pe *proto.Error
	if !errors.As(err, &pe) || !strings.Contains(string(pe.Details), "retry_after_ms") {
		t.Fatalf("rate limit error has no retry advice: %+v", err)
	}
	if r.rt.count() != pipe.SendBurst {
		t.Fatalf("%d messages routed, want %d: a limited send must not be queued", r.rt.count(), pipe.SendBurst)
	}
	// Another session of the same Worker and the same session of another
	// Worker have their own budget.
	if _, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, "minion-2", "x")); err != nil {
		t.Fatalf("other session: %v", err)
	}
	if _, err := r.p.Send(ctx, alice, sendArgs(wAlicePi2, pipeSess, "x")); err != nil {
		t.Fatalf("other worker: %v", err)
	}
	// A second later one more is allowed.
	r.clock.Advance(time.Second)
	if _, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "x")); err != nil {
		t.Fatalf("after a second: %v", err)
	}
}

func TestSendSizeLimitAndTaskBudget(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 3)
	alice := authz.NewSessionPrincipal(idAlice)
	_, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, strings.Repeat("x", pipe.MaxMessageBytes+1)))
	wantCode(t, err, proto.CodeMessageTooLarge)
	if _, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, strings.Repeat("x", pipe.MaxMessageBytes))); err != nil {
		t.Fatalf("exactly 8 KiB: %v", err)
	}
	for i := 0; i < 2; i++ {
		r.clock.Advance(2 * time.Second)
		if _, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "x")); err != nil {
			t.Fatal(err)
		}
	}
	r.clock.Advance(2 * time.Second)
	_, err = r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "x"))
	wantCode(t, err, proto.CodePipeBudgetExceeded)
	if r.rt.count() != 3 {
		t.Fatalf("%d messages routed, want 3", r.rt.count())
	}
}

func TestSendOutcomesDecideWhatStaysStored(t *testing.T) {
	ctx := context.Background()
	alice := authz.NewSessionPrincipal(idAlice)
	peer := peerOfRec(wAlicePi1, pipeSess)
	tests := []struct {
		name       string
		result     any
		err        error
		wantStored int
	}{
		{"delivered", deliveredResult(), nil, 1},
		{"failed_not_sent (busy agent) is retracted", ops.Refused("busy", nil), nil, 0},
		{"unknown stays: the write may have been applied", ops.WriteResult{Outcome: ops.OutcomeUnknown, Reason: "worker_offline"}, nil, 1},
		{"partial stays", ops.WriteResult{Outcome: ops.OutcomePartial, Reason: "internal"}, nil, 1},
		{"a refusal before the send is retracted", nil, proto.NewError(proto.CodeWorkerOffline, "offline"), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newPipeRig(t, 0)
			r.rt.result, r.rt.err = tc.result, tc.err
			res, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "hi"))
			if tc.err != nil {
				wantCode(t, err, proto.CodeWorkerOffline)
			} else if err != nil {
				t.Fatal(err)
			}
			_ = res
			msgs, _ := r.st.Messages(ctx, peer, 0, 0)
			if len(msgs) != tc.wantStored {
				t.Fatalf("%d messages stored, want %d", len(msgs), tc.wantStored)
			}
			// What is not stored does not use the task budget either.
			n, _ := r.st.CountMessagesSince(ctx, peer, r.clock.Now().Add(-time.Hour))
			if n != tc.wantStored {
				t.Fatalf("budget use = %d, want %d", n, tc.wantStored)
			}
		})
	}
}

func TestSendNeedsAWorkerInTheTarget(t *testing.T) {
	r := newPipeRig(t, 0)
	_, err := r.p.Send(context.Background(), authz.NewSessionPrincipal(idAlice),
		ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{SessionName: pipeSess}, Text: "x"})
	wantCode(t, err, proto.CodeInvalidArgument)
}

// --- AC 2: turns are committed idempotently and acknowledged ---

type deadlineStream struct{ *fakeStream }

func (deadlineStream) SetReadDeadline(time.Time) error  { return nil }
func (deadlineStream) SetWriteDeadline(time.Time) error { return nil }

func turnJSON(t *testing.T, id string, msg int64, session, text string, at time.Time) readItem {
	return frame(t, TurnFrame{Type: FrameTurn, TurnID: id, Session: session, MessageID: msg, Text: text, Time: ops.Timestamp(at)})
}

func writtenAcks(t *testing.T, s *fakeStream) []TurnAckFrame {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TurnAckFrame
	for _, b := range s.written {
		var a TurnAckFrame
		if err := json.Unmarshal(b, &a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func TestTurnIsCommittedOnceAndAckedStoredEachTime(t *testing.T) {
	r := newPipeRig(t, 0)
	id, _ := pipe.NewTurnID(r.clock.Now())
	mid := r.delivery(t, wAlicePi1, pipeSess)
	fs := newFakeStream(
		turnJSON(t, id, mid, pipeSess, "the answer", r.clock.Now()),
		turnJSON(t, id, mid, pipeSess, "the answer", r.clock.Now()), // the Worker did not see the first ack
		frame(t, map[string]string{"type": "something.new"}),        // additive protocol: ignored
	)
	r.p.ServeTurns(context.Background(), wAlicePi1.ID, deadlineStream{fs})

	acks := writtenAcks(t, fs)
	if len(acks) != 2 {
		t.Fatalf("%d acks, want one per turn frame", len(acks))
	}
	for _, a := range acks {
		if a.Type != proto.TypeTurnAck || a.TurnID != id || a.Status != pipe.AckStored {
			t.Fatalf("ack = %+v", a)
		}
	}
	turns, _ := r.st.Turns(context.Background(), peerOfRec(wAlicePi1, pipeSess), "", 0)
	if len(turns) != 1 || turns[0].Text != "the answer" {
		t.Fatalf("stored turns = %+v: committed twice or lost", turns)
	}
}

func TestTurnsAreStoredUnderTheAuthenticatedWorker(t *testing.T) {
	r := newPipeRig(t, 0)
	id := r.commitWire(t, wAlicePi1, pipeSess, "from pi-1")
	// The frame has no worker field at all: a turn_id sent again by another
	// Worker, even for a delivery of its own, cannot overwrite or appear under
	// the first one, and is not told that it is stored.
	ack, err := r.p.IngestTurn(context.Background(), wBobPi1.ID, TurnFrame{TurnID: id, Session: pipeSess, MessageID: r.delivery(t, wBobPi1, pipeSess), Text: "forged", Time: ops.Timestamp(r.clock.Now())})
	if err != nil || ack.Status != pipe.AckRejected {
		t.Fatalf("ack = %+v, %v; want rejected", ack, err)
	}
	mine, _ := r.st.Turns(context.Background(), peerOfRec(wAlicePi1, pipeSess), "", 0)
	other, _ := r.st.Turns(context.Background(), peerOfRec(wBobPi1, pipeSess), "", 0)
	if len(mine) != 1 || mine[0].Text != "from pi-1" || len(other) != 0 {
		t.Fatalf("mine = %+v, other = %+v", mine, other)
	}
}

func TestInvalidTurnsAreRejectedNotStored(t *testing.T) {
	r := newPipeRig(t, 0)
	good, _ := pipe.NewTurnID(r.clock.Now())
	mid := r.delivery(t, wAlicePi1, pipeSess)
	tests := []struct {
		name string
		f    TurnFrame
	}{
		{"turn_id is not a UUIDv7", TurnFrame{TurnID: "nope", Session: pipeSess, MessageID: mid, Text: "x"}},
		{"bad session name", TurnFrame{TurnID: good, Session: "a.b", MessageID: mid, Text: "x"}},
		{"empty text", TurnFrame{TurnID: good, Session: pipeSess, MessageID: mid}},
		{"too large", TurnFrame{TurnID: good, Session: pipeSess, MessageID: mid, Text: strings.Repeat("x", MaxTurnBytes+1)}},
		{"not UTF-8", TurnFrame{TurnID: good, Session: pipeSess, MessageID: mid, Text: "\xff\xfe"}},
		{"no delivery named", TurnFrame{TurnID: good, Session: pipeSess, Text: "x"}},
		{"a delivery that does not exist", TurnFrame{TurnID: good, Session: pipeSess, MessageID: mid + 1000, Text: "x"}},
		{"a delivery of another session", TurnFrame{TurnID: good, Session: "minion-2", MessageID: mid, Text: "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ack, err := r.p.IngestTurn(context.Background(), wAlicePi1.ID, tc.f)
			if err != nil || ack.Status != pipe.AckRejected || ack.Reason == "" {
				t.Fatalf("ack = %+v, %v; want rejected with a reason", ack, err)
			}
		})
	}
	if turns, _ := r.st.Turns(context.Background(), peerOfRec(wAlicePi1, pipeSess), "", 0); len(turns) != 0 {
		t.Fatalf("rejected turns were stored: %+v", turns)
	}
}

func TestExpiredTurnsAndMessagesArePruned(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	r.commit(t, wAlicePi1, pipeSess, "old")
	if _, err := r.p.Send(ctx, authz.NewSessionPrincipal(idAlice), sendArgs(wAlicePi1, pipeSess, "old")); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(pipe.Retention + time.Hour)
	r.commit(t, wAlicePi1, pipeSess, "new")
	res, err := r.p.Broker().Prune(ctx, r.clock.Now())
	if err != nil || res.Messages != 1 || res.Turns != 1 {
		t.Fatalf("Prune = %+v, %v", res, err)
	}
	if turns, _ := r.st.Turns(ctx, peerOfRec(wAlicePi1, pipeSess), "", 0); len(turns) != 1 || turns[0].Text != "new" {
		t.Fatalf("turns after prune = %+v", turns)
	}
}

// --- AC 3: a model reads only trusted sources; people see everything ---

func texts(v PipeReadView) []string {
	var out []string
	for _, e := range v.Turns {
		out = append(out, e.Direction+":"+e.Text)
	}
	return out
}

func TestModelReadsTurnsOnlyFromTrustedSources(t *testing.T) {
	ctx := context.Background()
	readArgs := ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)}
	tests := []struct {
		name     string
		p        authz.Principal
		trust    func(*pipeRig)
		wantRecv bool
	}{
		{"human in the browser, no trust", authz.NewSessionPrincipal(idAlice), nil, true},
		{"admin acting as the user, no trust", authz.NewActAsPrincipal(idRoot, idAlice, time.Now().Add(time.Hour)), nil, true},
		{"API key, no trust", modelAlice(), nil, false},
		{"API key, trusts the session", modelAlice(), func(r *pipeRig) { _ = r.ts.PutTrust(ctx, idAlice, wAlicePi1.ID, pipeSess, 0) }, true},
		{"API key, trusts the whole Worker", modelAlice(), func(r *pipeRig) { _ = r.ts.PutTrust(ctx, idAlice, wAlicePi1.ID, "", 0) }, true},
		{"API key, trusts another session only", modelAlice(), func(r *pipeRig) { _ = r.ts.PutTrust(ctx, idAlice, wAlicePi1.ID, "minion-9", 0) }, false},
		{"API key, only another user trusts", modelAlice(), func(r *pipeRig) { _ = r.ts.PutTrust(ctx, idBob, wAlicePi1.ID, pipeSess, 0) }, false},
		{"Worker identity, owner trusts", authz.NewWorkerPrincipal(wAlicePi2.ID), func(r *pipeRig) { _ = r.ts.PutTrust(ctx, idAlice, wAlicePi1.ID, pipeSess, 0) }, true},
		{"Worker identity, no trust", authz.NewWorkerPrincipal(wAlicePi2.ID), nil, false},
		{"API key, trust lookup fails: fail closed", modelAlice(), func(r *pipeRig) {
			_ = r.ts.PutTrust(ctx, idAlice, wAlicePi1.ID, pipeSess, 0)
			r.ts.err = errors.New("db down")
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newPipeRig(t, 0)
			if _, err := r.p.Send(ctx, authz.NewSessionPrincipal(idAlice), sendArgs(wAlicePi1, pipeSess, "question")); err != nil {
				t.Fatal(err)
			}
			r.clock.Advance(time.Second)
			r.commit(t, wAlicePi1, pipeSess, "answer")
			if tc.trust != nil {
				tc.trust(r)
			}
			view, err := r.p.Read(ctx, tc.p, readArgs)
			if err != nil {
				t.Fatal(err)
			}
			got := texts(view)
			if tc.wantRecv {
				if len(got) != 2 || got[0] != "sent:question" || got[1] != "received:answer" || view.Withheld != 0 || view.Reason != "" {
					t.Fatalf("view = %+v", view)
				}
				return
			}
			if len(got) != 1 || got[0] != "sent:question" || view.Withheld != 1 || view.Reason != "untrusted_peer" {
				t.Fatalf("view = %+v, want only the sent message and 1 withheld with untrusted_peer", view)
			}
			if strings.Contains(view.Notice, "answer") {
				t.Fatalf("the notice leaks the turn: %q", view.Notice)
			}
		})
	}
}

func TestTrustChangeAppliesToTheNextRead(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	r.commit(t, wAlicePi1, pipeSess, "answer")
	args := ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)}
	read := func() PipeReadView {
		v, err := r.p.Read(ctx, modelAlice(), args)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := read(); len(v.Turns) != 0 || v.Withheld != 1 {
		t.Fatalf("before trust: %+v", v)
	}
	if err := r.trust.Set(ctx, idAlice, wAlicePi1.ID, pipeSess); err != nil {
		t.Fatal(err)
	}
	if v := read(); len(v.Turns) != 1 || v.Withheld != 0 {
		t.Fatalf("after trust: %+v", v)
	}
	if _, err := r.trust.Remove(ctx, idAlice, wAlicePi1.ID, pipeSess); err != nil {
		t.Fatal(err)
	}
	if v := read(); len(v.Turns) != 0 || v.Withheld != 1 {
		t.Fatalf("after removing trust: %+v", v)
	}
}

func TestReadIsAuthorisedAndPaged(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	first := r.commit(t, wAlicePi1, pipeSess, "one")
	r.commit(t, wAlicePi1, pipeSess, "two")
	r.commit(t, wAlicePi1, pipeSess, "three")
	alice := authz.NewSessionPrincipal(idAlice)

	_, err := r.p.Read(ctx, authz.NewSessionPrincipal(idBob), ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)})
	wantCode(t, err, proto.CodeNotFound)
	_, err = r.p.Read(ctx, keyWith(idAlice, ops.ActionMessage), ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)})
	wantCode(t, err, proto.CodeForbidden)

	v, err := r.p.Read(ctx, alice, ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess), AfterTurnID: first, Limit: 1})
	if err != nil || len(v.Turns) != 1 || v.Turns[0].Text != "two" || v.Turns[0].Session != pipeSess || !ops.ValidUUIDv7(v.Turns[0].TurnID) {
		t.Fatalf("page = %+v, %v", v, err)
	}
	_, err = r.p.Read(ctx, alice, ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess), AfterTurnID: ops.NewUUIDv7()})
	wantCode(t, err, proto.CodeNotFound)
	empty, err := r.p.Read(ctx, alice, ops.PipeReadArgs{Target: target(wAlicePi1, "minion-2")})
	if err != nil || empty.Turns == nil || len(empty.Turns) != 0 {
		t.Fatalf("empty read = %+v, %v (turns must be an empty array)", empty, err)
	}
}

func collect(t *testing.T, r *pipeRig, p authz.Principal, args ops.PipeWaitArgs) ([]ops.PipeTurn, error) {
	t.Helper()
	var got []ops.PipeTurn
	err := r.p.Wait(context.Background(), p, args, func(e ops.PipeTurn) error {
		got = append(got, e)
		return nil
	})
	return got, err
}

func TestWaitDeliversTheNextTurnAndTimesOutWithoutOne(t *testing.T) {
	r := newPipeRig(t, 0)
	r.commit(t, wAlicePi1, pipeSess, "old")
	alice := authz.NewSessionPrincipal(idAlice)
	args := ops.PipeWaitArgs{Target: target(wAlicePi1, pipeSess), TimeoutMS: 30}

	start := time.Now()
	got, err := collect(t, r, alice, args)
	if err != nil || len(got) != 0 || time.Since(start) < 25*time.Millisecond {
		t.Fatalf("timeout wait = %+v, %v after %v", got, err, time.Since(start))
	}

	args.TimeoutMS = 5000
	done := make(chan struct{})
	var out []ops.PipeTurn
	go func() {
		defer close(done)
		out, err = collect(t, r, alice, args)
	}()
	time.Sleep(20 * time.Millisecond)
	r.commit(t, wAlicePi1, pipeSess, "fresh")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipe_wait did not wake for a committed turn")
	}
	if err != nil || len(out) != 1 || out[0].Text != "fresh" || out[0].Direction != ops.PipeReceived {
		t.Fatalf("wait = %+v, %v", out, err)
	}
}

func TestWaitWithACursorReturnsTurnsAfterIt(t *testing.T) {
	r := newPipeRig(t, 0)
	first := r.commit(t, wAlicePi1, pipeSess, "one")
	r.commit(t, wAlicePi1, pipeSess, "two")
	got, err := collect(t, r, authz.NewSessionPrincipal(idAlice),
		ops.PipeWaitArgs{Target: target(wAlicePi1, pipeSess), AfterTurnID: first, TimeoutMS: 1000})
	if err != nil || len(got) != 1 || got[0].Text != "two" {
		t.Fatalf("wait = %+v, %v", got, err)
	}
}

func TestWaitWithholdsFromAModelThatDoesNotTrustTheSource(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	args := ops.PipeWaitArgs{Target: target(wAlicePi1, pipeSess), TimeoutMS: 5000}
	_, err := collect(t, r, modelAlice(), args)
	wantCode(t, err, proto.CodeUntrustedPeer)

	// Trusted at the start, trust withdrawn before the turn arrives.
	if err := r.trust.Set(ctx, idAlice, wAlicePi1.ID, pipeSess); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := collect(t, r, modelAlice(), args)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if _, err := r.trust.Remove(ctx, idAlice, wAlicePi1.ID, pipeSess); err != nil {
		t.Fatal(err)
	}
	r.commit(t, wAlicePi1, pipeSess, "secret")
	select {
	case err := <-done:
		wantCode(t, err, proto.CodeUntrustedPeer)
	case <-time.After(3 * time.Second):
		t.Fatal("pipe_wait did not end")
	}

	// A person always gets the turn.
	got, err := collect(t, r, authz.NewSessionPrincipal(idAlice), ops.PipeWaitArgs{Target: target(wAlicePi1, pipeSess), AfterTurnID: "", TimeoutMS: 20})
	if err != nil || len(got) != 0 {
		t.Fatalf("person wait = %+v, %v", got, err)
	}
}

func TestWaitStopsWhenThePermissionIsWithdrawn(t *testing.T) {
	r := newPipeRig(t, 0)
	// A key that stops covering the session while the wait runs.
	var mu sync.Mutex
	covered := true
	p := authz.NewAPIKeyPrincipal(idAlice, scopeFunc(func(authz.ResolvedResource) []ops.Action {
		mu.Lock()
		defer mu.Unlock()
		if covered {
			return []ops.Action{ops.ActionRead}
		}
		return nil
	}), "")
	if err := r.trust.Set(context.Background(), idAlice, wAlicePi1.ID, pipeSess); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := collect(t, r, p, ops.PipeWaitArgs{Target: target(wAlicePi1, pipeSess), TimeoutMS: 5000})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	covered = false
	mu.Unlock()
	select {
	case err := <-done:
		wantCode(t, err, proto.CodeNotFound)
	case <-time.After(3 * time.Second):
		t.Fatal("pipe_wait kept running after the permission was withdrawn")
	}
}

func TestNewPipeNeedsItsPorts(t *testing.T) {
	if _, err := NewPipe(PipeConfig{}); err == nil {
		t.Fatal("NewPipe accepted an empty config")
	}
}

// mustMessage stores a message for a session, as the Hub's send does, and
// returns its id.
func mustMessage(t *testing.T, st *store.PipeStore, workerID, session string) int64 {
	t.Helper()
	m, err := st.AppendMessage(context.Background(), pipe.Message{Peer: pipe.Peer{WorkerID: workerID, SessionName: session}, Text: "a message", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}
