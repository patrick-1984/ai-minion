package workers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// gateClock is a clock a test moves.
type gateClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *gateClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *gateClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// expiringGrant is aliceGrant that ends at the time.
func expiringGrant(level int, at time.Time) store.SessionGrantRow {
	g := aliceGrant(level)
	ms := at.UnixMilli()
	g.ExpiresAt = &ms
	return g
}

// A grant that expires after the decision and before the dispatch is refused
// by the fence, although no row changed: the Worker's stream is opened but no
// request is written to it.
func TestSessionGrantExpiredBeforeDispatchIsRefused(t *testing.T) {
	r := newSCRig(t)
	clock := &gateClock{now: time.Now()}
	r.cs.cfg.Now = clock.Now
	r.grants.setDirect(wAlicePi1.ID, "agent", expiringGrant(authz.LevelControl, clock.Now().Add(time.Hour)))
	// The clock passes the expiry while the destination stream is opening.
	r.be.beforeOpen = func(*fakeBackend) { clock.advance(2 * time.Hour) }
	frames, sent := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden {
		t.Fatalf("expired before dispatch: %q, want forbidden", c)
	}
	sent.mu.Lock()
	defer sent.mu.Unlock()
	if len(sent.written) != 0 {
		t.Fatalf("%d frames reached the Worker", len(sent.written))
	}
}

// A running call ends at the first re-check after its grant expired, and the
// key of a grant differs on the two sides of its expiry (valid while the clock
// is before expires_at).
func TestSessionGrantExpiryEndsARunningCall(t *testing.T) {
	clock := &gateClock{now: time.UnixMilli(1_800_000_000_000)}
	g := &fakeGrants{}
	exp := clock.Now().Add(time.Minute)
	g.setDirect(wAlicePi1.ID, "agent", expiringGrant(authz.LevelView, exp))
	cs := NewCallServer(CallServerConfig{Grants: g, Now: clock.Now, ReauthInterval: 5 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	pr, _, key, err := cs.authenticate(context.Background(), wAlicePi1.ID, "", "agent", ops.OpSessionCapture)
	if err != nil || key == "" {
		t.Fatalf("key %q %v", key, err)
	}
	clock.advance(time.Minute - time.Millisecond)
	if cur, _ := cs.grantKey(context.Background(), wAlicePi1.ID, "agent", true); cur != key {
		t.Fatal("the key changed before the expiry")
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cs.reauthorizeWorker(ctx, cancel, pr, "agent", key, true)
	}()
	time.Sleep(20 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("ended before the grant expired")
	}
	clock.advance(time.Millisecond) // the exact instant of the expiry
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the call outlived its grant")
	}
	if c := proto.CodeOf(context.Cause(ctx)); c != proto.CodeForbidden {
		t.Fatalf("cause = %v", context.Cause(ctx))
	}
}

// Direct grants count only for the operations of D3. On pipe_send, pipe_read
// and pipe_wait a session with direct grants is who it was without them: the
// plain Worker identity, or its explicit call key, which a grant row (also a
// suspended one) does not displace.
func TestDirectGrantsDoNotReachPipeOps(t *testing.T) {
	ctx := context.Background()
	suspended := aliceGrant(authz.LevelView)
	suspended.ID = "g-2"
	at := int64(1)
	suspended.SuspendedAt = &at
	g := &fakeGrants{}
	g.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelControl), suspended)
	cs := NewCallServer(CallServerConfig{Keys: newFakeKeys(), Grants: g, Logger: slog.New(slog.DiscardHandler)})
	for _, op := range []string{ops.OpPipeSend, ops.OpPipeRead, ops.OpPipeWait, ops.OpSessionCreate, "an_op_added_later"} {
		pr, actor, key, err := cs.authenticate(ctx, wAlicePi1.ID, "", "agent", op)
		if err != nil || key != "" || len(pr.DirectGrants()) != 0 || pr != authz.NewWorkerPrincipal(wAlicePi1.ID) {
			t.Errorf("%s without a key: principal %+v, grant key %q, %v; want the plain Worker identity", op, pr, key, err)
		}
		if actor != "worker:"+wAlicePi1.ID {
			t.Errorf("%s: actor %q", op, actor)
		}
		pr, actor, key, err = cs.authenticate(ctx, wAlicePi1.ID, keyBoundPi1, "agent", op)
		if err != nil || key != "" || pr.Kind != authz.KindAPIKey || actor != "key:k-pi1" {
			t.Errorf("%s with a call key: principal %+v, actor %q, grant key %q, %v; want the key", op, pr, actor, key, err)
		}
		// The fence and the re-check of such a call do not read them either.
		if cur, err := cs.grantKey(ctx, wAlicePi1.ID, "agent", directGrantOps[op]); err != nil || cur != "" {
			t.Errorf("%s: grant key %q %v", op, cur, err)
		}
	}
	for op := range directGrantOps {
		pr, _, key, err := cs.authenticate(ctx, wAlicePi1.ID, keyBoundPi1, "agent", op)
		if err != nil || key == "" || len(pr.DirectGrants()) != 2 || pr.Kind != authz.KindWorker {
			t.Errorf("%s: principal %+v, grant key %q, %v; want the session principal", op, pr, key, err)
		}
	}
	if len(directGrantOps) != 8 {
		t.Errorf("%d operations take direct grants, D3 lists 8", len(directGrantOps))
	}
}

// The Hub sets submit on the prompt of a Message grant: a send_text without
// the flag is sent agent-only and submitted, never typed and left unsent.
func TestSessionGrantMessageSubmitIsSetByTheHub(t *testing.T) {
	r := newSCRig(t)
	r.targetOnline(proto.FeatureSendTextAgentOnly, proto.FeatureSendTextSubmit)
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelMessage))
	frames, sent := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	var wr ops.WriteResult
	resultOf(t, frames, &wr)
	var got ops.SendTextArgs
	if err := json.Unmarshal(sent.request(t).Args, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Submit || !got.AgentOnly || got.Force {
		t.Fatalf("prompt = %+v, want agent-only and submitted", got)
	}
}

// The message a Worker identity holds on its own sessions is no Message
// grant: a session with a direct grant elsewhere cannot prompt a session of
// its own Worker through the grant-only send_text.
func TestSessionGrantPromptPathNeedsAGrantOnTheTarget(t *testing.T) {
	r := newSCRig(t)
	r.be.mu.Lock()
	r.be.online[wAlicePi1.ID] = Online{WorkerID: wAlicePi1.ID, Features: []string{proto.FeatureSendTextAgentOnly, proto.FeatureSendTextSubmit}}
	r.be.mu.Unlock()
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelView))
	args := scArgs(ops.OpSendText, ops.Target{WorkerID: wAlicePi1.ID, SessionName: "other"})
	args["submit"] = true
	frames, _ := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, args, "", "agent"), ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden {
		t.Fatalf("prompt to the own Worker: %q, want forbidden", c)
	}
	if r.be.openCount() != 0 {
		t.Fatal("the request reached a Worker")
	}
}

// The audit event of a brokered write names the direct grant that authorized
// it; a refused write names none.
func TestSessionGrantWriteAuditNamesTheGrant(t *testing.T) {
	r := newSCRig(t)
	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelControl))
	frames, _ := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	var wr ops.WriteResult
	resultOf(t, frames, &wr)
	evs := r.audit.all()
	if len(evs) != 1 || evs[0].Action != ops.OpSendText || evs[0].GrantID != "g-1" ||
		evs[0].Source != wAlicePi1.ID+":agent" || evs[0].Outcome != "success" {
		t.Fatalf("audit = %+v, want one send_text event with grant g-1", evs)
	}

	r.grants.setDirect(wAlicePi1.ID, "agent", aliceGrant(authz.LevelView))
	frames, _ = r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
		ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if c := errCodeOf(t, frames); c != proto.CodeForbidden {
		t.Fatalf("View grant: %q, want forbidden", c)
	}
	if evs = r.audit.all(); len(evs) != 2 || evs[1].GrantID != "" || evs[1].Outcome != "denied" {
		t.Fatalf("audit of the refusal = %+v", evs)
	}
}

// failingKeys authenticates until it is told to fail.
type failingKeys struct{ fail atomic.Bool }

func (k *failingKeys) AuthenticateCall(context.Context, string, string) (apikey.Identity, error) {
	if k.fail.Load() {
		return apikey.Identity{}, errors.New("store down")
	}
	return apikey.Identity{KeyID: "k"}, nil
}

// The re-check of a call key passes over one store error and ends the call
// on the second in a row.
func TestCallKeyTwoFailedRechecksEndARunningCall(t *testing.T) {
	keys := &failingKeys{}
	cs := NewCallServer(CallServerConfig{Keys: keys, ReauthInterval: 5 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cs.reauthenticate(ctx, cancel, wAlicePi1.ID, "mn_key")
	}()
	time.Sleep(20 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("ended while the store answered")
	}
	keys.fail.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the call was not withdrawn")
	}
	if c := proto.CodeOf(context.Cause(ctx)); c != proto.CodeForbidden {
		t.Fatalf("cause = %v", context.Cause(ctx))
	}
}
