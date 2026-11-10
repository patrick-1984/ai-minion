package workers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Review round 2 of Story 7.4: the lifecycle of one request_id.

// scriptRouter routes each call through fn, so that a test decides when and
// how an attempt ends.
type scriptRouter struct {
	mu    sync.Mutex
	calls int
	fn    func(n int, req CallRequest) (any, error)
}

func (r *scriptRouter) Call(_ context.Context, req CallRequest) (any, error) {
	r.mu.Lock()
	r.calls++
	n := r.calls
	fn := r.fn
	r.mu.Unlock()
	return fn(n, req)
}

func (r *scriptRouter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func useRouter(t *testing.T, r *pipeRig, rt PipeRouter) {
	t.Helper()
	p, err := NewPipe(PipeConfig{Store: r.st, Authz: r.az, Router: rt, Trust: r.trust, Now: r.clock.Now, Poll: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	r.p = p
}

// The Worker keeps its request_id cache for ten minutes, the Hub the message
// for seven days. A retry after the Worker's cache expired is answered from
// the outcome the Hub recorded: it is neither routed (the Worker would write
// the text a second time, with no history row and no budget charge) nor
// charged.
func TestRetryAfterTheWorkerCacheExpiredIsReplayedNotRouted(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	alice := authz.NewSessionPrincipal(idAlice)
	args := sendArgs(wAlicePi1, pipeSess, "run the tests")
	first, err := r.p.Send(ctx, alice, args)
	if err != nil || first.Outcome != ops.OutcomeDelivered {
		t.Fatalf("first send = %+v, %v", first, err)
	}
	for _, after := range []time.Duration{11 * time.Minute, time.Hour, 6 * 24 * time.Hour} {
		r.clock.Advance(after)
		res, err := r.p.Send(ctx, alice, args)
		if err != nil || res.Outcome != ops.OutcomeDelivered {
			t.Fatalf("retry after %v = %+v, %v, want the recorded outcome", after, res, err)
		}
	}
	if n := r.rt.count(); n != 1 {
		t.Fatalf("%d calls reached the Worker, want exactly the first", n)
	}
	if msgs, _ := r.st.Messages(ctx, peerOfRec(wAlicePi1, pipeSess), 0, 0); len(msgs) != 1 {
		t.Fatalf("%d messages stored, want 1", len(msgs))
	}
}

// A recorded outcome that is not "delivered" is replayed as it was.
func TestRetryReplaysAPartialOutcomeToo(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	r.rt.mu.Lock()
	r.rt.result = ops.WriteResult{Outcome: ops.OutcomePartial, Reason: "busy", Warnings: []ops.Warning{{Code: "typed_not_submitted", Message: "typed, not submitted"}}}
	r.rt.mu.Unlock()
	alice := authz.NewSessionPrincipal(idAlice)
	args := sendArgs(wAlicePi1, pipeSess, "x")
	if _, err := r.p.Send(ctx, alice, args); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(time.Hour)
	res, err := r.p.Send(ctx, alice, args)
	if err != nil || res.Outcome != ops.OutcomePartial || res.Reason != "busy" || len(res.Warnings) != 1 || r.rt.count() != 1 {
		t.Fatalf("replay = %+v, %v after %d calls", res, err, r.rt.count())
	}
}

// A message whose outcome was never recorded (the Hub stopped while the Worker
// wrote) is routed again only while the Worker's cache still makes it the same
// write; later the Hub says unknown and routes nothing.
func TestSendWithoutARecordedOutcomeIsRoutedAgainOnlyInsideTheWorkerWindow(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	alice := authz.NewSessionPrincipal(idAlice)
	peer := peerOfRec(wAlicePi1, pipeSess)

	pending := func(rid, text string) ops.PipeSendArgs {
		a := sendArgs(wAlicePi1, pipeSess, text)
		a.RequestID = rid
		m, err := r.p.Broker().Send(ctx, peer, text)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.st.SetMessageRequestID(ctx, m.ID, rid); err != nil {
			t.Fatal(err)
		}
		return a
	}
	inWindow := pending(ops.NewUUIDv7(), "inside")
	r.clock.Advance(RerouteWindow - time.Minute)
	res, err := r.p.Send(ctx, alice, inWindow)
	if err != nil || res.Outcome != ops.OutcomeDelivered || r.rt.count() != 1 {
		t.Fatalf("inside the window = %+v, %v after %d calls; want one routing", res, err, r.rt.count())
	}
	// Now the outcome is recorded: a later retry replays it.
	r.clock.Advance(time.Hour)
	if res, err := r.p.Send(ctx, alice, inWindow); err != nil || res.Outcome != ops.OutcomeDelivered || r.rt.count() != 1 {
		t.Fatalf("replay = %+v, %v after %d calls", res, err, r.rt.count())
	}

	r.clock.Advance(time.Hour) // keep the budget window clean for the next message
	stale := pending(ops.NewUUIDv7(), "stale")
	r.clock.Advance(RerouteWindow + time.Second)
	res, err = r.p.Send(ctx, alice, stale)
	if err != nil || res.Outcome != ops.OutcomeUnknown || res.Reason == "" || r.rt.count() != 1 {
		t.Fatalf("outside the window = %+v, %v after %d calls; want unknown and no routing", res, err, r.rt.count())
	}
	if err := res.Validate(); err != nil {
		t.Errorf("the refusal is not a valid WriteResult: %v", err)
	}
}

// The first attempt stalls in routing and fails; a concurrent identical request
// must not have its delivery retracted by it, and it must not be charged less
// than once.
func TestAFailedFirstAttemptCannotRetractAConcurrentRetrysDelivery(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	alice := authz.NewSessionPrincipal(idAlice)
	entered := make(chan struct{})
	release := make(chan struct{})
	rt := &scriptRouter{fn: func(n int, _ CallRequest) (any, error) {
		if n == 1 {
			close(entered)
			<-release
			return nil, proto.NewError(proto.CodeWorkerOffline, "the first attempt failed")
		}
		return deliveredResult(), nil
	}}
	useRouter(t, r, rt)
	args := sendArgs(wAlicePi1, pipeSess, "once")

	firstErr := make(chan error, 1)
	go func() { _, err := r.p.Send(ctx, alice, args); firstErr <- err }()
	<-entered // the first attempt created the message and is stalled in routing

	type result struct {
		res ops.WriteResult
		err error
	}
	second := make(chan result, 1)
	go func() { res, err := r.p.Send(ctx, alice, args); second <- result{res, err} }()
	// The second attempt waits for the first one to end.
	select {
	case got := <-second:
		t.Fatalf("the retry finished while the first attempt was still routing: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	if n := rt.count(); n != 1 {
		t.Fatalf("%d calls routed while the first attempt runs", n)
	}
	close(release)
	if err := <-firstErr; err == nil {
		t.Fatal("the first attempt should have failed")
	}
	got := <-second
	if got.err != nil || got.res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("the retry = %+v, %v", got.res, got.err)
	}
	msgs, _ := r.st.Messages(ctx, peerOfRec(wAlicePi1, pipeSess), 0, 0)
	if len(msgs) != 1 || msgs[0].Text != "once" {
		t.Fatalf("history = %+v: the delivered message must stay exactly once", msgs)
	}
	// And it keeps its budget accounting: one message in the last day.
	if n, _ := r.st.CountMessagesSince(ctx, peerOfRec(wAlicePi1, pipeSess), r.clock.Now().Add(-24*time.Hour)); n != 1 {
		t.Fatalf("budget accounting counts %d messages, want 1", n)
	}
	// A third attempt replays the delivery; the Worker saw two calls only.
	if res, err := r.p.Send(ctx, alice, args); err != nil || res.Outcome != ops.OutcomeDelivered || rt.count() != 2 {
		t.Fatalf("third attempt = %+v, %v after %d calls", res, err, rt.count())
	}
}

// A request waiting for the attempt in progress gives up with its own context.
func TestARetryWaitingForTheFirstAttemptEndsWithItsContext(t *testing.T) {
	r := newPipeRig(t, 0)
	alice := authz.NewSessionPrincipal(idAlice)
	entered := make(chan struct{})
	release := make(chan struct{})
	rt := &scriptRouter{fn: func(int, CallRequest) (any, error) {
		close(entered)
		<-release
		return deliveredResult(), nil
	}}
	useRouter(t, r, rt)
	args := sendArgs(wAlicePi1, pipeSess, "once")
	done := make(chan struct{})
	go func() { defer close(done); _, _ = r.p.Send(context.Background(), alice, args) }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.p.Send(ctx, alice, args); err == nil {
		t.Fatal("the waiting retry did not end with its context")
	}
	close(release)
	<-done
}

// A send without a request_id is never replayed or serialised.
func TestSendsWithoutARequestIDAreIndependent(t *testing.T) {
	ctx := context.Background()
	r := newPipeRig(t, 0)
	alice := authz.NewSessionPrincipal(idAlice)
	a := sendArgs(wAlicePi1, pipeSess, "x")
	a.RequestID = ""
	for i := 0; i < 2; i++ {
		if _, err := r.p.Send(ctx, alice, a); err != nil {
			t.Fatal(err)
		}
		r.clock.Advance(2 * time.Second)
	}
	if r.rt.count() != 2 {
		t.Fatalf("%d calls routed, want 2", r.rt.count())
	}
}
