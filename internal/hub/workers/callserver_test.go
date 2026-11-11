package workers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/auth/apikey"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// --- fakes ---

const (
	keyBoundPi2  = "mn_bound_to_pi2"
	keyBoundPi1  = "mn_bound_to_pi1"
	keyUnbound   = "mn_unbound"
	keyScopedAll = "mn_scoped"
)

// fakeKeys applies the real binding rule (apikey.CheckCallBinding) to a small
// table of keys.
type fakeKeys struct {
	mu      sync.Mutex
	keys    map[string]apikey.Identity
	revoked map[string]bool
	calls   int
}

func newFakeKeys() *fakeKeys {
	all := scopeFunc(func(authz.ResolvedResource) []ops.Action {
		return []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage}
	})
	return &fakeKeys{
		keys: map[string]apikey.Identity{
			keyBoundPi2: {KeyID: "k-pi2", Principal: authz.NewAPIKeyPrincipal(idAlice, all, wAlicePi2.ID)},
			keyBoundPi1: {KeyID: "k-pi1", Principal: authz.NewAPIKeyPrincipal(idAlice, all, wAlicePi1.ID)},
			keyUnbound:  {KeyID: "k-free", Principal: authz.NewAPIKeyPrincipal(idAlice, all, "")},
		},
		revoked: map[string]bool{},
	}
}

func (f *fakeKeys) AuthenticateCall(_ context.Context, token, workerID string) (apikey.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	id, ok := f.keys[token]
	if !ok || f.revoked[token] {
		return apikey.Identity{}, apikey.ErrInvalidKey
	}
	if err := apikey.CheckCallBinding(id, workerID); err != nil {
		return apikey.Identity{}, err
	}
	return id, nil
}

func (f *fakeKeys) revoke(token string) {
	f.mu.Lock()
	f.revoked[token] = true
	f.mu.Unlock()
}

type memAudit struct {
	mu  sync.Mutex
	evs []audit.Event
}

func (a *memAudit) Record(_ context.Context, ev audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evs = append(a.evs, ev)
	return nil
}

func (a *memAudit) all() []audit.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]audit.Event(nil), a.evs...)
}

// callStream is a fakeStream that behaves like a call stream: after the
// request the Hub reads from it (to notice the caller leaving), which blocks
// until the stream is closed or reset.
type callStream struct{ *fakeStream }

func (callStream) SetReadDeadline(time.Time) error  { return nil }
func (callStream) SetWriteDeadline(time.Time) error { return nil }
func (s callStream) Close() error                   { _ = s.fakeStream.Close(); s.Reset(); return nil }

func newCallStream(t *testing.T, req any) callStream {
	t.Helper()
	fs := newFakeStream(frame(t, req))
	fs.hang = true
	return callStream{fs}
}

type callRig struct {
	*pipeRig
	cs    *CallServer
	keys  *fakeKeys
	audit *memAudit
}

func newCallRig(t *testing.T, mutate func(*CallServerConfig)) *callRig {
	t.Helper()
	pr := newPipeRig(t, 0)
	rr := newRig(t, nil) // the router that resolves human targets
	keys := newFakeKeys()
	au := &memAudit{}
	cfg := CallServerConfig{
		Keys: keys, Resolver: rr.rt, Owners: rr.dir, Auditor: au,
		Logger: slog.New(slog.DiscardHandler),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	cs := NewCallServer(cfg)
	if err := pr.p.RegisterCalls(cs); err != nil {
		t.Fatal(err)
	}
	return &callRig{pipeRig: pr, cs: cs, keys: keys, audit: au}
}

func (r *callRig) call(t *testing.T, workerID string, req any) []json.RawMessage {
	t.Helper()
	st := newCallStream(t, req)
	r.cs.Serve(context.Background(), workerID, st)
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]json.RawMessage(nil), st.written...)
}

func request(op string, args any, key string) map[string]any {
	m := map[string]any{"type": proto.TypeOpRequest, "op": op, "request_id": ops.NewUUIDv7(), "args": args}
	if key != "" {
		m["api_key"] = key
	}
	return m
}

func lastFrame(t *testing.T, frames []json.RawMessage) (typ string, body json.RawMessage) {
	t.Helper()
	if len(frames) == 0 {
		t.Fatal("no frame was written")
	}
	body = frames[len(frames)-1]
	typ, err := proto.FrameType(body)
	if err != nil {
		t.Fatal(err)
	}
	return typ, body
}

func errCodeOf(t *testing.T, frames []json.RawMessage) proto.Code {
	t.Helper()
	typ, body := lastFrame(t, frames)
	if typ != proto.TypeError {
		t.Fatalf("last frame is %q (%s), want an error frame", typ, body)
	}
	var f proto.ErrorFrame
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatal(err)
	}
	return f.Code
}

// --- AC 1: pipe_send through a Worker call stream ---

func TestCallStreamPipeSendAsWorkerIdentity(t *testing.T) {
	r := newCallRig(t, nil)
	// pi-1 sends to a session on itself, by the human target form (A -> itself).
	frames := r.call(t, wAlicePi1.ID, request(ops.OpPipeSend,
		map[string]any{"request_id": ops.NewUUIDv7(), "target": "pi-1:minion-1", "text": "ping from pi-2"}, ""))
	if len(frames) != 1 {
		t.Fatalf("%d frames: %s", len(frames), frames)
	}
	typ, body := lastFrame(t, frames)
	var f struct {
		Result ops.WriteResult `json:"result"`
	}
	if typ != proto.TypeOpResult || json.Unmarshal(body, &f) != nil || f.Result.Outcome != ops.OutcomeDelivered {
		t.Fatalf("answer = %s", body)
	}
	if r.rt.count() != 1 || r.rt.calls[0].WorkerID != wAlicePi1.ID || r.rt.calls[0].Session != "minion-1" {
		t.Fatalf("routed = %+v", r.rt.calls)
	}
	if r.rt.calls[0].Principal.Kind != authz.KindWorker {
		t.Fatalf("routed as %q, want the Worker identity", r.rt.calls[0].Principal.Kind)
	}
	msgs, _ := r.st.Messages(context.Background(), peerOfRec(wAlicePi1, "minion-1"), 0, 0)
	if len(msgs) != 1 || msgs[0].Text != "ping from pi-2" {
		t.Fatalf("stored = %+v", msgs)
	}
	evs := r.audit.all()
	if len(evs) != 1 || evs[0].Actor != "worker:"+wAlicePi1.ID || evs[0].Action != ops.OpPipeSend || evs[0].Outcome != "success" {
		t.Fatalf("audit = %+v", evs)
	}
}

func TestCallStreamWorkerIdentityIsCheckedAgainstItsOwnersSpace(t *testing.T) {
	r := newCallRig(t, nil)
	// pi-2 (alice) cannot message a session of bob's Worker, and cannot even
	// name it by name: both look like a missing target.
	for _, tgt := range []any{
		"pi-1:minion-1", // alice's pi-1, resolved in alice's space: allowed
		map[string]any{"worker_id": wBobPi1.ID, "session_name": "minion-1"},
	} {
		frames := r.call(t, wAlicePi1.ID, request(ops.OpPipeSend,
			map[string]any{"request_id": ops.NewUUIDv7(), "target": tgt, "text": "x"}, ""))
		typ, _ := lastFrame(t, frames)
		if _, isString := tgt.(string); isString {
			if typ != proto.TypeOpResult {
				t.Fatalf("own space: %s", frames)
			}
			continue
		}
		if code := errCodeOf(t, frames); code != proto.CodeNotFound {
			t.Fatalf("foreign target: code %q", code)
		}
	}
	if r.rt.count() != 1 {
		t.Fatalf("%d calls routed, want only the allowed one", r.rt.count())
	}
	// Another owner's name is not resolvable from alice's Worker.
	frames := r.call(t, wAlicePi1.ID, request(ops.OpPipeSend,
		map[string]any{"request_id": ops.NewUUIDv7(), "target": "box:minion-1", "text": "x"}, ""))
	if code := errCodeOf(t, frames); code != proto.CodeTargetNotFound {
		t.Fatalf("foreign name: code %q", code)
	}
}

func TestCallStreamBoundKeyWorksOnlyOnItsOwnWorker(t *testing.T) {
	r := newCallRig(t, nil)
	args := func() map[string]any {
		return map[string]any{"request_id": ops.NewUUIDv7(), "target": map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}, "text": "hi"}
	}
	// The key is bound to pi-2 and arrives on pi-2's stream: accepted.
	frames := r.call(t, wAlicePi2.ID, request(ops.OpPipeSend, args(), keyBoundPi2))
	if typ, _ := lastFrame(t, frames); typ != proto.TypeOpResult {
		t.Fatalf("own stream: %s", frames)
	}
	if r.rt.calls[0].Principal.Kind != authz.KindAPIKey {
		t.Fatalf("routed as %q, want the API key", r.rt.calls[0].Principal.Kind)
	}
	// The same key on the stream of a different Worker: rejected, nothing runs.
	before := r.rt.count()
	frames = r.call(t, wAlicePi1.ID, request(ops.OpPipeSend, args(), keyBoundPi2))
	if code := errCodeOf(t, frames); code != proto.CodeForbidden {
		t.Fatalf("different Worker: code %q, want forbidden", code)
	}
	// A key that is not bound to any Worker is for REST, not for call streams.
	frames = r.call(t, wAlicePi2.ID, request(ops.OpPipeSend, args(), keyUnbound))
	if code := errCodeOf(t, frames); code != proto.CodeForbidden {
		t.Fatalf("unbound key: code %q, want forbidden", code)
	}
	frames = r.call(t, wAlicePi2.ID, request(ops.OpPipeSend, args(), "mn_unknown"))
	if code := errCodeOf(t, frames); code != proto.CodeUnauthenticated {
		t.Fatalf("unknown key: code %q, want unauthenticated", code)
	}
	if r.rt.count() != before {
		t.Fatalf("a rejected call reached the router")
	}
	for _, ev := range r.audit.all() {
		if ev.Action == "call.auth" && ev.Outcome != "denied" {
			t.Fatalf("refused credential audited as %+v", ev)
		}
	}
	got := 0
	for _, ev := range r.audit.all() {
		if ev.Action == "call.auth" {
			got++
		}
		for _, field := range []string{ev.Actor, ev.Action, ev.Target, ev.Outcome} {
			if field == keyBoundPi2 || field == "mn_unknown" {
				t.Fatalf("an audit field holds a key: %+v", ev)
			}
		}
	}
	if got != 3 {
		t.Fatalf("%d refused credentials audited, want 3", got)
	}
}

func TestCallStreamWithoutKeyAuthenticatorRefusesKeys(t *testing.T) {
	r := newCallRig(t, func(c *CallServerConfig) { c.Keys = nil })
	frames := r.call(t, wAlicePi2.ID, request(ops.OpPipeRead, map[string]any{"target": map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}}, keyBoundPi2))
	if code := errCodeOf(t, frames); code != proto.CodeUnauthenticated {
		t.Fatalf("code %q", code)
	}
}

func TestCallStreamRateLimitAppliesToCallsToo(t *testing.T) {
	r := newCallRig(t, nil)
	tgt := map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}
	for i := 0; i < pipe.SendBurst; i++ {
		frames := r.call(t, wAlicePi1.ID, request(ops.OpPipeSend, map[string]any{"request_id": ops.NewUUIDv7(), "target": tgt, "text": "x"}, ""))
		if typ, _ := lastFrame(t, frames); typ != proto.TypeOpResult {
			t.Fatalf("send %d: %s", i, frames)
		}
	}
	frames := r.call(t, wAlicePi1.ID, request(ops.OpPipeSend, map[string]any{"request_id": ops.NewUUIDv7(), "target": tgt, "text": "x"}, ""))
	if code := errCodeOf(t, frames); code != proto.CodePipeRateLimited {
		t.Fatalf("code %q", code)
	}
}

func TestCallStreamRejectsMalformedRequests(t *testing.T) {
	r := newCallRig(t, nil)
	tests := []struct {
		name string
		req  any
		want proto.Code
	}{
		{"not an op.request", map[string]any{"type": "op.result"}, proto.CodeInvalidArgument},
		{"an op the Hub does not serve on call streams", request(ops.OpSendText, map[string]any{}, ""), proto.CodeUnsupportedOp},
		{"an unknown op", request("no_such_op", map[string]any{}, ""), proto.CodeUnsupportedOp},
		{"arguments that fail validation", request(ops.OpPipeSend, map[string]any{"request_id": "x", "target": map[string]any{"worker_id": wAlicePi1.ID, "session_name": "s"}, "text": "t"}, ""), proto.CodeInvalidArgument},
		{"arguments that are not an object", request(ops.OpPipeSend, []int{1}, ""), proto.CodeInvalidArgument},
		{"a human target for a name nobody has", request(ops.OpPipeRead, map[string]any{"target": "ghost:s"}, ""), proto.CodeTargetNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code := errCodeOf(t, r.call(t, wAlicePi2.ID, tc.req)); code != tc.want {
				t.Fatalf("code %q, want %q", code, tc.want)
			}
		})
	}
}

func TestCallStreamHumanTargetNeedsAResolver(t *testing.T) {
	r := newCallRig(t, func(c *CallServerConfig) { c.Resolver = nil })
	frames := r.call(t, wAlicePi2.ID, request(ops.OpPipeRead, map[string]any{"target": "pi-1:minion-1"}, ""))
	if code := errCodeOf(t, frames); code != proto.CodeInvalidArgument {
		t.Fatalf("code %q", code)
	}
}

func TestCallStreamLimitsOpenCallsPerWorker(t *testing.T) {
	r := newCallRig(t, func(c *CallServerConfig) { c.MaxCalls = 1 })
	hold := &fakeStream{reset: make(chan struct{}), hang: true} // never sends a request
	first := callStream{hold}
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		r.cs.Serve(context.Background(), wAlicePi2.ID, first)
	}()
	<-started
	deadline := time.Now().Add(3 * time.Second)
	var code proto.Code
	for time.Now().Before(deadline) {
		frames := r.call(t, wAlicePi2.ID, request(ops.OpPipeRead, map[string]any{"target": map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}}, ""))
		if typ, _ := lastFrame(t, frames); typ == proto.TypeError {
			code = errCodeOf(t, frames)
			break
		}
		time.Sleep(time.Millisecond)
	}
	if code != proto.CodeRateLimited {
		t.Fatalf("second call: code %q, want rate_limited", code)
	}
	// Another Worker has its own allowance.
	frames := r.call(t, wAlicePi1.ID, request(ops.OpPipeRead, map[string]any{"target": map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}}, ""))
	if typ, _ := lastFrame(t, frames); typ != proto.TypeOpResult {
		t.Fatalf("other Worker: %s", frames)
	}
	hold.Reset()
	<-done
}

// --- AC 3 over call streams: the model reads only trusted sources ---

func TestCallStreamPipeReadWithholdsUntrustedTurnsFromModels(t *testing.T) {
	ctx := context.Background()
	r := newCallRig(t, nil)
	r.commit(t, wAlicePi1, "minion-1", "secret answer")
	args := map[string]any{"target": map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}}
	// A Worker identity reads its own Worker's sessions; a bound key arrives on
	// the stream of the Worker it is bound to (pi-2) and reaches pi-1 by scope.
	streamOf := func(key string) string {
		if key == "" {
			return wAlicePi1.ID
		}
		return wAlicePi2.ID
	}
	read := func(key string) PipeReadView {
		frames := r.call(t, streamOf(key), request(ops.OpPipeRead, args, key))
		typ, body := lastFrame(t, frames)
		if typ != proto.TypeOpResult {
			t.Fatalf("read: %s", body)
		}
		var f struct {
			Result PipeReadView `json:"result"`
		}
		if err := json.Unmarshal(body, &f); err != nil {
			t.Fatal(err)
		}
		return f.Result
	}
	for _, key := range []string{"", keyBoundPi2} { // Worker identity and Worker-bound key
		v := read(key)
		if len(v.Turns) != 0 || v.Withheld != 1 || v.Reason != "untrusted_peer" {
			t.Fatalf("key %q, untrusted: %+v", key, v)
		}
	}
	if err := r.trust.Set(ctx, idAlice, wAlicePi1.ID, "minion-1"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", keyBoundPi2} {
		v := read(key)
		if len(v.Turns) != 1 || v.Turns[0].Text != "secret answer" || v.Withheld != 0 {
			t.Fatalf("key %q, trusted: %+v", key, v)
		}
	}
}

func TestCallStreamPipeWaitStreamsTheNextTurn(t *testing.T) {
	ctx := context.Background()
	r := newCallRig(t, nil)
	if err := r.trust.Set(ctx, idAlice, wAlicePi1.ID, "minion-1"); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"target": map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}, "timeout_ms": 5000}
	st := newCallStream(t, request(ops.OpPipeWait, args, ""))
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.cs.Serve(ctx, wAlicePi1.ID, st)
	}()
	time.Sleep(30 * time.Millisecond)
	r.commit(t, wAlicePi1, "minion-1", "the reply")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipe_wait over a call stream did not end")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.written) != 2 {
		t.Fatalf("%d frames: %s", len(st.written), st.written)
	}
	var prog struct {
		Type string       `json:"type"`
		Data ops.PipeTurn `json:"data"`
	}
	if err := json.Unmarshal(st.written[0], &prog); err != nil || prog.Type != proto.TypeOpProgress || prog.Data.Text != "the reply" {
		t.Fatalf("progress = %s", st.written[0])
	}
	if typ, _ := proto.FrameType(st.written[1]); typ != proto.TypeOpResult {
		t.Fatalf("closing frame = %s", st.written[1])
	}
}

func TestCallStreamPipeWaitIsRefusedForUntrustedSources(t *testing.T) {
	r := newCallRig(t, nil)
	args := map[string]any{"target": map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}, "timeout_ms": 5000}
	if code := errCodeOf(t, r.call(t, wAlicePi1.ID, request(ops.OpPipeWait, args, ""))); code != proto.CodeUntrustedPeer {
		t.Fatalf("code %q", code)
	}
}

func TestCallStreamWithdrawsARevokedKeyWhileItWaits(t *testing.T) {
	ctx := context.Background()
	r := newCallRig(t, func(c *CallServerConfig) { c.ReauthInterval = 10 * time.Millisecond })
	if err := r.trust.Set(ctx, idAlice, wAlicePi1.ID, "minion-1"); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"target": map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}, "timeout_ms": 60000}
	st := newCallStream(t, request(ops.OpPipeWait, args, keyBoundPi2))
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.cs.Serve(ctx, wAlicePi2.ID, st)
	}()
	time.Sleep(30 * time.Millisecond)
	r.keys.revoke(keyBoundPi2)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the call kept running after its key was revoked")
	}
	st.mu.Lock()
	frames := append([]json.RawMessage(nil), st.written...)
	st.mu.Unlock()
	if code := errCodeOf(t, frames); code != proto.CodeUnauthenticated {
		t.Fatalf("code %q, want unauthenticated", code)
	}
}

func TestCallStreamEndsQuietlyWhenTheCallerLeaves(t *testing.T) {
	ctx := context.Background()
	r := newCallRig(t, nil)
	if err := r.trust.Set(ctx, idAlice, wAlicePi1.ID, "minion-1"); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"target": map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}, "timeout_ms": 60000}
	st := newCallStream(t, request(ops.OpPipeWait, args, ""))
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.cs.Serve(ctx, wAlicePi1.ID, st)
	}()
	time.Sleep(30 * time.Millisecond)
	st.Reset() // the Worker abandons the call
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the call kept running after the caller left")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.written) != 0 {
		t.Fatalf("frames were written to an abandoned call: %s", st.written)
	}
}

func TestCallServerHandleRejectsBadRegistrations(t *testing.T) {
	cs := NewCallServer(CallServerConfig{})
	ok := func(context.Context, *CallContext, func(any) error) (any, error) { return nil, nil }
	if err := cs.Handle("no_such_op", ok); err == nil {
		t.Error("an op outside the catalog was accepted")
	}
	if err := cs.Handle(ops.OpPipeRead, nil); err == nil {
		t.Error("a nil handler was accepted")
	}
	if err := cs.Handle(ops.OpPipeRead, ok); err != nil {
		t.Fatal(err)
	}
	if err := cs.Handle(ops.OpPipeRead, ok); err == nil {
		t.Error("a duplicate handler was accepted")
	}
	if got := cs.Ops(); len(got) != 1 || got[0] != ops.OpPipeRead {
		t.Errorf("Ops = %v", got)
	}
}

// --- integration: Hub and two Workers over real transport sessions ---

// worldWorker is the test side of one connected Worker.
type worldWorker struct {
	rec  WorkerRecord
	rig  *pipeRig
	sess *transport.Session // the Worker's end
	hub  *transport.Session // the Hub's end
	mu   sync.Mutex
	ops  []OpRequest // op.requests the Hub sent to this Worker
	// served is closed when the accept loop of the Worker's session returned.
	served chan struct{}
}

type world struct {
	*pipeRig
	reg      *Registry
	events   *EventHub
	evIngest *Ingest
	handler  *Handler
	cs       *CallServer
	workers  map[string]*worldWorker
	ctx      context.Context
	cancel   context.CancelFunc
}

// newWorld composes the Hub side of the pipe the way the production Hub does:
// every Worker session has ONE accept loop (the stream host of the connect
// handler), the call streams go to the handler's call loop, which serves
// worker_retire and hands the pipe ops to the call server, and the "ev"
// streams go to the event ingest, which gives the turn channel to the pipe.
func newWorld(t *testing.T, recs ...WorkerRecord) *world {
	t.Helper()
	pr := newPipeRig(t, 0)
	reg := NewRegistry()
	dir := &fakeDir{recs: []WorkerRecord{wAlicePi1, wAlicePi2, wBobPi1, wBobBox, wRootLab}}
	rt, err := NewRouter(RouterConfig{
		Backend: RegistryBackend(reg), Directory: dir, Authz: pr.az,
		OwnerName: func(_ context.Context, id string) string { return ownerNames[id] },
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The Hub pipe uses the real router and the real registry.
	p, err := NewPipe(PipeConfig{
		Store: pr.st, Authz: pr.az, Router: rt, Trust: pr.trust,
		Now: pr.clock.Now, Poll: 5 * time.Millisecond, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	pr.p = p
	events := NewEventHub(EventHubOptions{})
	ing, err := NewIngest(IngestOptions{Hub: events, FirstFrameTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !p.RegisterTurns(ing) {
		t.Fatal("the pipe could not register the turn channel")
	}
	reg.OnAdd(ing.Attach)
	h := &Handler{log: slog.New(slog.DiscardHandler), cfg: Config{Registry: reg}}
	cs := NewCallServer(CallServerConfig{Keys: newFakeKeys(), Resolver: rt, Owners: dir, Logger: slog.New(slog.DiscardHandler)})
	if err := p.RegisterCalls(cs); err != nil {
		t.Fatal(err)
	}
	if err := cs.Bind(h); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w := &world{pipeRig: pr, reg: reg, events: events, evIngest: ing, handler: h, cs: cs,
		workers: map[string]*worldWorker{}, ctx: ctx, cancel: cancel}
	for _, rec := range recs {
		w.connect(t, rec)
	}
	return w
}

// connect adds a Worker the way the connect handler's Welcome does: a hosted
// session with the call handler registered, added to the registry (the hooks
// join the host), then the one accept loop.
func (w *world) connect(t *testing.T, rec WorkerRecord) *worldWorker {
	t.Helper()
	a, b := net.Pipe()
	hubSide, err := transport.NewSession(a, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	workerSide, err := transport.NewSession(b, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hubSide.Close(); _ = workerSide.Close() })
	ww := &worldWorker{rec: rec, rig: w.pipeRig, sess: workerSide, hub: hubSide}
	w.workers[rec.ID] = ww
	host := newHostedSession(hubSide, w.handler.streamAdmission().attach(rec.ID), nil)
	host.Handle(transport.KindCall, newCallStreams(w.handler, store.WorkerRow{ID: rec.ID, Name: rec.Name, OwnerID: rec.OwnerID}, hubSide))
	w.reg.Add(Online{WorkerID: rec.ID, Name: rec.Name, OwnerID: rec.OwnerID, Kind: rec.Kind,
		Ops: []string{ops.OpPipeSend, ops.OpPipeRead, ops.OpPipeWait}, Features: allFeatures()}, host)
	served := make(chan struct{})
	go func() { defer close(served); host.serve(w.ctx) }()
	ww.served = served
	go ww.serveOps(w.ctx, t)
	return ww
}

// serveOps plays the Worker's op server: it records the request and says
// delivered.
func (ww *worldWorker) serveOps(ctx context.Context, t *testing.T) {
	for {
		st, err := ww.sess.Accept(ctx)
		if err != nil {
			return
		}
		go func() {
			body, err := st.ReadFrame()
			if err != nil {
				return
			}
			var rq OpRequest
			if json.Unmarshal(body, &rq) != nil {
				return
			}
			ww.mu.Lock()
			ww.ops = append(ww.ops, rq)
			ww.mu.Unlock()
			_ = st.WriteFrame(map[string]any{"type": proto.TypeOpResult, "result": deliveredResult()})
			_ = st.Close()
		}()
	}
}

func (ww *worldWorker) requests() []OpRequest {
	ww.mu.Lock()
	defer ww.mu.Unlock()
	return append([]OpRequest(nil), ww.ops...)
}

// call is what `mn pipe_send` on a Worker host does: open a call stream, send
// one op.request, read until the result or error.
func (ww *worldWorker) call(t *testing.T, op string, args any, key string) (proto.Code, json.RawMessage) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := ww.sess.OpenCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteFrame(request(op, args, key)); err != nil {
		t.Fatal(err)
	}
	for {
		body, err := st.ReadFrame()
		if err != nil {
			t.Fatalf("reading the answer: %v", err)
		}
		typ, _ := proto.FrameType(body)
		switch typ {
		case proto.TypeOpResult:
			var f struct {
				Result json.RawMessage `json:"result"`
			}
			_ = json.Unmarshal(body, &f)
			_ = st.Close()
			return "", f.Result
		case proto.TypeError:
			var f proto.ErrorFrame
			_ = json.Unmarshal(body, &f)
			_ = st.Close()
			return f.Code, nil
		}
	}
}

// emit is what the turn emitter of a Worker does on its turn channel.
func (ww *worldWorker) emit(t *testing.T, session, text string, at time.Time) TurnAckFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := ww.sess.OpenEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Reset()
	id, _ := pipe.NewTurnID(at)
	for _, f := range []any{
		map[string]string{"type": FrameTurnsOpen},
		TurnFrame{Type: FrameTurn, TurnID: id, Session: session, MessageID: ww.rig.delivery(t, ww.rec, session), Text: text, Time: ops.Timestamp(at)},
	} {
		if err := st.WriteFrame(f); err != nil {
			t.Fatal(err)
		}
	}
	body, err := st.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	var ack TurnAckFrame
	if err := json.Unmarshal(body, &ack); err != nil || ack.TurnID != id {
		t.Fatalf("answer = %s", body)
	}
	return ack
}

// Integration: one Worker sends through a call stream, the other receives the
// write op; the reply travels back as a turn; trust decides what a model sees.
func TestHubWithTwoWorkersPipeSendAndTrustedReads(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, wAlicePi1, wAlicePi2)
	sender, receiver := w.workers[wAlicePi2.ID], w.workers[wAlicePi1.ID]

	// 0. F-01: pi-2 speaking as its own Worker identity cannot reach a session
	// of pi-1, a Worker of the same owner. It cannot even name it.
	for _, tgt := range []any{"pi-1:minion-1", map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}} {
		code, _ := sender.call(t, ops.OpPipeSend,
			map[string]any{"request_id": ops.NewUUIDv7(), "target": tgt, "text": "no key"}, "")
		if code != proto.CodeTargetNotFound && code != proto.CodeNotFound {
			t.Fatalf("keyless cross-Worker pipe_send: code %q, want not found", code)
		}
	}
	if n := len(receiver.requests()); n != 0 {
		t.Fatalf("pi-1 received %d requests from a keyless peer", n)
	}

	// 1. `mn pipe_send` on pi-2 to a session on pi-1, with the call key of pi-2
	// (call_api_key_file): an API key bound to pi-2.
	code, res := sender.call(t, ops.OpPipeSend,
		map[string]any{"request_id": ops.NewUUIDv7(), "target": "pi-1:minion-1", "text": "please run the tests"}, keyBoundPi2)
	var wr ops.WriteResult
	if code != "" || json.Unmarshal(res, &wr) != nil || wr.Outcome != ops.OutcomeDelivered {
		t.Fatalf("pipe_send over the call stream: code %q, result %s", code, res)
	}
	reqs := receiver.requests()
	if len(reqs) != 1 || reqs[0].Op != ops.OpPipeSend || !reqs[0].Actions.Permits(ops.ActionMessage) {
		t.Fatalf("pi-1 received %+v", reqs)
	}
	// The origin is derived by the Hub from the key: the receiver sees the
	// bound Worker as the source, with its fingerprint and its owner.
	wantOrigin := proto.Origin{
		Kind: proto.OriginAPIKey, UserID: idAlice, KeyID: "k-pi2", KeyBoundWorkerID: wAlicePi2.ID,
		SourceWorkerID: wAlicePi2.ID, SourceWorkerFP: wAlicePi2.Fingerprint, SourceWorkerOwnerID: idAlice,
	}
	if reqs[0].Origin != wantOrigin {
		t.Fatalf("origin = %+v, want %+v", reqs[0].Origin, wantOrigin)
	}
	var sent ops.PipeSendArgs
	if err := json.Unmarshal(reqs[0].Args, &sent); err != nil || sent.Text != "please run the tests" || sent.Target.WorkerID != wAlicePi1.ID || sent.Target.SessionName != "minion-1" {
		t.Fatalf("pi-1 received args %s (%v)", reqs[0].Args, err)
	}

	// 2. The agent on pi-1 answers; the Worker emits the turn. Twice: the
	// second time stands for a retry after a lost ack.
	at := w.clock.Now()
	ack := receiver.emit(t, "minion-1", "all tests pass", at)
	if ack.Status != pipe.AckStored {
		t.Fatalf("ack = %+v", ack)
	}
	turns, _ := w.st.Turns(ctx, peerOfRec(wAlicePi1, "minion-1"), "", 0)
	if len(turns) != 1 {
		t.Fatalf("%d turns stored", len(turns))
	}

	// 3. Trust off: a model on pi-2 (through its Worker-bound call key) gets the
	// sent message but not the reply. Its keyless Worker identity reads nothing
	// of pi-1.
	readArgs := map[string]any{"target": "pi-1:minion-1"}
	if code, _ := sender.call(t, ops.OpPipeRead, readArgs, ""); code != proto.CodeTargetNotFound && code != proto.CodeNotFound {
		t.Fatalf("keyless cross-Worker pipe_read: code %q, want not found", code)
	}
	for _, key := range []string{keyBoundPi2} {
		code, res := sender.call(t, ops.OpPipeRead, readArgs, key)
		var v PipeReadView
		if code != "" || json.Unmarshal(res, &v) != nil {
			t.Fatalf("read: %q %s", code, res)
		}
		if v.Withheld != 1 || v.Reason != "untrusted_peer" || len(v.Turns) != 1 || v.Turns[0].Direction != ops.PipeSent {
			t.Fatalf("trust off, key %q: %+v", key, v)
		}
	}
	// A person sees everything they may read, trusted or not.
	view, err := w.p.Read(ctx, authz.NewSessionPrincipal(idAlice), ops.PipeReadArgs{Target: target(wAlicePi1, "minion-1")})
	if err != nil || len(view.Turns) != 2 || view.Withheld != 0 {
		t.Fatalf("human read = %+v, %v", view, err)
	}

	// 4. Trust on.
	if err := w.trust.Set(ctx, idAlice, wAlicePi1.ID, "minion-1"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{keyBoundPi2} {
		code, res := sender.call(t, ops.OpPipeRead, readArgs, key)
		var v PipeReadView
		if code != "" || json.Unmarshal(res, &v) != nil {
			t.Fatalf("read: %q %s", code, res)
		}
		if v.Withheld != 0 || len(v.Turns) != 2 || v.Turns[1].Text != "all tests pass" {
			t.Fatalf("trust on, key %q: %+v", key, v)
		}
	}

	// 5. A key bound to pi-1 but used on pi-2's stream is rejected.
	code, _ = sender.call(t, ops.OpPipeRead, readArgs, keyBoundPi1)
	if code != proto.CodeForbidden {
		t.Fatalf("a Worker-bound key on another Worker's call stream: code %q, want forbidden", code)
	}
}

// F-07: a running keyless Worker call ends when the Worker's owner loses the
// last grant, like a call with an API key ends when its key is revoked.
func TestCallStreamWithdrawsAKeylessCallWhenTheOwnerLosesAccess(t *testing.T) {
	ctx := context.Background()
	pr := newPipeRig(t, 0)
	cs := NewCallServer(CallServerConfig{Authz: pr.az, ReauthInterval: 10 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	if err := cs.Handle(ops.OpPipeWait, func(ctx context.Context, _ *CallContext, _ func(any) error) (any, error) {
		<-ctx.Done() // a long call that never asks the Authorizer itself
		return nil, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"target": map[string]any{"worker_id": wAlicePi1.ID, "session_name": "minion-1"}}
	st := newCallStream(t, request(ops.OpPipeWait, args, ""))
	done := make(chan struct{})
	go func() {
		defer close(done)
		cs.Serve(ctx, wAlicePi1.ID, st)
	}()
	time.Sleep(30 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the call ended while the owner still had access")
	default:
	}
	pr.users.set(idAlice, authz.UserState{Role: "", Active: true})
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the keyless call kept running after its owner lost every grant")
	}
	st.mu.Lock()
	frames := append([]json.RawMessage(nil), st.written...)
	st.mu.Unlock()
	if code := errCodeOf(t, frames); code != proto.CodeForbidden {
		t.Fatalf("code %q, want forbidden", code)
	}
}
