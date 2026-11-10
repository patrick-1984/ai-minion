package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// callHub plays the Hub end of a real transport session: it accepts the call
// streams of the Worker and answers them with a script.
type callHub struct {
	t       *testing.T
	hub     *transport.Session
	worker  *transport.Session
	cancel  context.CancelFunc
	handler func(st *transport.Stream, req map[string]json.RawMessage)

	mu       sync.Mutex
	requests []map[string]json.RawMessage
}

func startCallHub(t *testing.T, handler func(st *transport.Stream, req map[string]json.RawMessage)) *callHub {
	t.Helper()
	a, b := net.Pipe()
	hub, err := transport.NewSession(a, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := transport.NewSession(b, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &callHub{t: t, hub: hub, worker: worker, cancel: cancel, handler: handler}
	t.Cleanup(func() { cancel(); _ = hub.Close(); _ = worker.Close() })
	go func() {
		for {
			st, err := hub.Accept(ctx)
			if err != nil {
				return
			}
			go func() {
				body, err := st.ReadFrame()
				if err != nil {
					return
				}
				var req map[string]json.RawMessage
				if json.Unmarshal(body, &req) != nil {
					return
				}
				h.mu.Lock()
				h.requests = append(h.requests, req)
				h.mu.Unlock()
				h.handler(st, req)
			}()
		}
	}()
	return h
}

func (h *callHub) requestAt(i int) map[string]json.RawMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.requests) {
		h.t.Fatalf("only %d requests", len(h.requests))
	}
	return h.requests[i]
}

// link gives a call client a Link over the Worker end of the session.
func (h *callHub) link(c *CallClient) (end func()) {
	ctx, cancel := context.WithCancelCause(context.Background())
	l := &Link{Session: h.worker, ctx: ctx}
	done := make(chan struct{})
	go func() { c.Serve(l); close(done) }()
	waitFor2(h.t, c.Connected)
	return func() { cancel(ErrLinkLost); <-done }
}

func reply(st *transport.Stream, frames ...any) {
	for _, f := range frames {
		_ = st.WriteFrame(f)
	}
	_ = st.Close()
}

func TestCallDoSendsOneRequestAndDecodesTheResult(t *testing.T) {
	h := startCallHub(t, func(st *transport.Stream, req map[string]json.RawMessage) {
		reply(st, map[string]any{"type": proto.TypeOpResult, "result": ops.WriteResult{Outcome: ops.OutcomeDelivered}})
	})
	c := NewCallClient(CallConfig{})
	defer h.link(c)()

	args := ops.PipeSendArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()},
		Target:    ops.Target{WorkerID: ops.NewUUIDv7(), SessionName: "minion-1"}, Text: "hi",
	}
	var res ops.WriteResult
	if err := c.Do(context.Background(), ops.OpPipeSend, args, &res); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("Do = %+v, %v", res, err)
	}
	req := h.requestAt(0)
	var typ, op, rid string
	_ = json.Unmarshal(req["type"], &typ)
	_ = json.Unmarshal(req["op"], &op)
	_ = json.Unmarshal(req["request_id"], &rid)
	if typ != proto.TypeOpRequest || op != ops.OpPipeSend || !ops.ValidUUIDv7(rid) {
		t.Fatalf("request = %v", req)
	}
	var got ops.PipeSendArgs
	if err := json.Unmarshal(req["args"], &got); err != nil || got.Text != "hi" || got.RequestID != args.RequestID {
		t.Fatalf("args = %s", req["args"])
	}
	if _, has := req["api_key"]; has {
		t.Fatalf("a call without a key carries api_key: %v", req)
	}
}

func TestCallPresentsTheWorkerBoundKeyAndNeverLeaksItInErrors(t *testing.T) {
	const key = "mn_secretsecretsecret"
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		reply(st, proto.NewError(proto.CodeForbidden, "the API key is not bound to this Worker").Frame())
	})
	c := NewCallClient(CallConfig{Key: func() (string, error) { return key, nil }})
	defer h.link(c)()

	err := c.Do(context.Background(), ops.OpPipeRead, map[string]any{}, nil)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeForbidden {
		t.Fatalf("error = %v, want the Hub's forbidden", err)
	}
	var sent string
	_ = json.Unmarshal(h.requestAt(0)["api_key"], &sent)
	if sent != key {
		t.Fatalf("the Hub got api_key %q", sent)
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("the error holds the key: %v", err)
	}

	// An unreadable key file is an unauthenticated error that names no key.
	bad := NewCallClient(CallConfig{Key: func() (string, error) { return "", errors.New("open /run/key: permission denied") }})
	defer h.link(bad)()
	err = bad.Do(context.Background(), ops.OpPipeRead, nil, nil)
	if proto.CodeOf(err) != proto.CodeUnauthenticated {
		t.Fatalf("key source failure = %v", err)
	}
}

func TestCallStreamDeliversElementsInOrder(t *testing.T) {
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		reply(st,
			map[string]any{"type": proto.TypeOpProgress, "data": map[string]string{"text": "one"}},
			map[string]any{"type": proto.TypeOpProgress, "data": map[string]string{"text": "two"}},
			map[string]any{"type": proto.TypeOpResult, "result": struct{}{}})
	})
	c := NewCallClient(CallConfig{})
	defer h.link(c)()

	var got []string
	err := c.Stream(context.Background(), ops.OpPipeWait, ops.PipeWaitArgs{Target: ops.Target{SessionName: "s"}}, func(d json.RawMessage) error {
		var e struct{ Text string }
		_ = json.Unmarshal(d, &e)
		got = append(got, e.Text)
		return nil
	})
	if err != nil || len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("Stream = %v, %v", got, err)
	}

	stop := errors.New("enough")
	err = c.Stream(context.Background(), ops.OpPipeWait, nil, func(json.RawMessage) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("an error from the element callback = %v", err)
	}
}

// The Hub reads the end of a call stream as "the caller is gone", so the
// client must keep its side open until it has the answer.
func TestCallKeepsTheStreamOpenUntilItHasTheAnswer(t *testing.T) {
	sawEnd := make(chan bool, 1)
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		_ = st.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		_, err := st.ReadFrame() // the Hub's "caller gone" watcher
		sawEnd <- err != nil && !isTimeout(err)
		reply(st, map[string]any{"type": proto.TypeOpResult, "result": struct{}{}})
	})
	c := NewCallClient(CallConfig{})
	defer h.link(c)()
	if err := c.Do(context.Background(), ops.OpPipeRead, nil, nil); err != nil {
		t.Fatal(err)
	}
	if <-sawEnd {
		t.Fatal("the client ended its side of the stream before the answer")
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func TestCallWithoutAHubFailsAtOnce(t *testing.T) {
	c := NewCallClient(CallConfig{})
	err := c.Do(context.Background(), ops.OpPipeRead, nil, nil)
	if proto.CodeOf(err) != proto.CodeWorkerOffline {
		t.Fatalf("error = %v, want worker_offline", err)
	}
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		reply(st, map[string]any{"type": proto.TypeOpResult, "result": struct{}{}})
	})
	end := h.link(c)
	if !c.Connected() {
		t.Fatal("not connected after a link")
	}
	if err := c.Do(context.Background(), "", nil, nil); proto.CodeOf(err) != proto.CodeInvalidArgument {
		t.Fatalf("an empty op name = %v, want invalid_argument", err)
	}
	end()
	if c.Connected() {
		t.Fatal("still connected after the link ended")
	}
	if err := c.Do(context.Background(), ops.OpPipeRead, nil, nil); proto.CodeOf(err) != proto.CodeWorkerOffline {
		t.Fatalf("after the link ended: %v", err)
	}
}

func TestCallLostAfterTheRequestIsReportedAsPossiblyApplied(t *testing.T) {
	gotRequest := make(chan struct{})
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		close(gotRequest)
		st.Reset() // the Hub dies without an answer
	})
	c := NewCallClient(CallConfig{})
	defer h.link(c)()
	err := c.Do(context.Background(), ops.OpPipeSend, map[string]string{"text": "x"}, nil)
	var lost *CallLostError
	if !errors.As(err, &lost) {
		t.Fatalf("error = %T %v, want *CallLostError", err, err)
	}
	<-gotRequest
	if proto.CodeOf(err) != proto.CodeWorkerOffline {
		t.Fatalf("code = %q, want worker_offline", proto.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "may have been applied") {
		t.Fatalf("message = %q", err)
	}
}

func TestCallContextEndAbandonsTheCall(t *testing.T) {
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		<-time.After(10 * time.Second) // never answers
		st.Reset()
	})
	c := NewCallClient(CallConfig{})
	defer h.link(c)()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Do(ctx, ops.OpPipeRead, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("error = %v after %v", err, time.Since(start))
	}
}

func TestCallIgnoresUnknownFramesAndRejectsGarbage(t *testing.T) {
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		reply(st, map[string]any{"type": "future.frame"}, map[string]any{"type": proto.TypeOpResult, "result": map[string]int{"n": 7}})
	})
	c := NewCallClient(CallConfig{})
	defer h.link(c)()
	var out struct{ N int }
	if err := c.Do(context.Background(), "x", nil, &out); err != nil || out.N != 7 {
		t.Fatalf("Do = %+v, %v", out, err)
	}
	if err := c.Do(context.Background(), "x", nil, new(string)); proto.CodeOf(err) != proto.CodeInternal {
		t.Fatalf("a result of the wrong shape = %v", err)
	}
}

// TestAWriteThatTimesOutAfterTheSendIsUnknownNotInternal: the Hub reads the
// request, withholds the answer, and the call's own timer (100 ms here) runs out
// while the caller's context is alive. The request may have been applied, so the
// call ends with a *CallLostError that wraps the deadline, not with the bare
// context error that the local socket would report as an internal failure. A
// read in the same situation keeps the plain deadline, and a call that never
// left (no link) is no CallLostError.
func TestAWriteThatTimesOutAfterTheSendIsUnknownNotInternal(t *testing.T) {
	got := make(chan struct{}, 4)
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		got <- struct{}{}
		<-time.After(5 * time.Second) // the response is withheld
		st.Reset()
	})
	c := NewCallClient(CallConfig{Timeout: 100 * time.Millisecond})
	defer h.link(c)()

	args := ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{SessionName: "s"}, Text: "x"}
	start := time.Now()
	err := c.Do(context.Background(), ops.OpPipeSend, args, nil)
	var lost *CallLostError
	if !errors.As(err, &lost) || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("write error = %T %v after %v, want a CallLostError wrapping the deadline", err, err, time.Since(start))
	}
	<-got
	h.mu.Lock()
	seen := len(h.requests)
	h.mu.Unlock()
	if seen != 1 {
		t.Fatalf("the Hub saw %d requests, want 1: nothing is sent again", seen)
	}

	// The same through the forwarding of the local pipe: an unknown outcome
	// with the explicit warning, never an internal error and never a resend.
	svc := &PipeService{cfg: PipeConfig{Hub: callsOnly{c}}}
	raw, ferr := svc.forward(context.Background(), Request{Op: ops.OpPipeSend, Actions: []ops.Action{ops.ActionMessage, ops.ActionOperate}, Args: mustJSON(t, args)})
	if ferr != nil {
		t.Fatalf("forward returned an error %v", ferr)
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil || res.Outcome != ops.OutcomeUnknown || len(res.Warnings) == 0 || res.Warnings[0].Code != "hub_call_lost" {
		t.Fatalf("forwarded result = %s", raw)
	}
	<-got

	// A read keeps the plain deadline.
	err = c.Do(context.Background(), ops.OpPipeRead, map[string]any{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &lost) {
		t.Fatalf("read error = %T %v, want the plain deadline", err, err)
	}
}

// callsOnly is a PipeHub whose only member is a real call client.
type callsOnly struct{ c *CallClient }

func (callsOnly) Enrolled(context.Context) (bool, error) { return true, nil }
func (h callsOnly) Calls() PipeCaller                    { return h.c }
func (callsOnly) Turns() TurnSink                        { return nil }

// completionStall lets the Hub receive a whole pipe_send and then delays the
// completion of the network write until the production connection write timeout
// of the multiplexer (10 s) fires. Nothing about the write result is mocked: the
// bytes reach the Hub, and the Worker's write fails late with the real error.
type completionStall struct {
	net.Conn
	received <-chan struct{}
}

func (c *completionStall) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && bytes.Contains(p, []byte(`"op":"pipe_send"`)) {
		select {
		case <-c.received:
		case <-time.After(time.Second):
			return n, net.ErrClosed
		}
		time.Sleep(10200 * time.Millisecond)
	}
	return n, err
}

// TestAWriteFailingAfterTheHubReceivedItIsUnknown is the reproduction of the
// second review: the Hub has the complete request, the network write completes
// late and fails, and the forwarding must say unknown (with the warning), not
// "could not be sent" with worker_offline, which invites a repeat under a new
// request id. It takes the 10 s of the production write timeout and is skipped
// with -short.
func TestAWriteFailingAfterTheHubReceivedItIsUnknown(t *testing.T) {
	if testing.Short() {
		t.Skip("takes the 10 s production connection write timeout")
	}
	a, b := net.Pipe()
	received := make(chan struct{})
	hub, err := transport.NewSession(a, transport.RoleHub, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := transport.NewSession(&completionStall{b, received}, transport.RoleWorker, transport.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	defer worker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var requests atomic.Int32
	go func() {
		st, err := hub.Accept(ctx)
		if err != nil {
			return
		}
		if _, err := st.ReadFrame(); err != nil {
			return
		}
		requests.Add(1)
		close(received)
		<-ctx.Done()
	}()
	c := NewCallClient(CallConfig{})
	c.cur = &Link{Session: worker, ctx: ctx}
	svc := &PipeService{cfg: PipeConfig{Hub: callsOnly{c}}}
	args := ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{WorkerID: ops.NewUUIDv7(), SessionName: "s"}, Text: "marker"}
	raw, ferr := svc.forward(ctx, Request{Op: ops.OpPipeSend, Actions: []ops.Action{ops.ActionMessage, ops.ActionOperate}, Args: mustJSON(t, args)})
	if requests.Load() != 1 {
		t.Fatalf("the proof needs the Hub to have received the request (%d)", requests.Load())
	}
	var res ops.WriteResult
	if ferr != nil || json.Unmarshal(raw, &res) != nil || res.Outcome != ops.OutcomeUnknown || len(res.Warnings) == 0 || res.Warnings[0].Code != "hub_call_lost" {
		t.Fatalf("forward = %s, %v: want the unknown outcome with the hub_call_lost warning", raw, ferr)
	}
}

// TestAFrameThatCannotBeSentIsNeverUnknown: an argument that does not encode and
// a frame over the size limit fail before a stream is opened, as errors that say
// nothing was sent (and that the call may be made again after fixing it).
func TestAFrameThatCannotBeSentIsNeverUnknown(t *testing.T) {
	h := startCallHub(t, func(st *transport.Stream, _ map[string]json.RawMessage) {
		t.Error("a request that cannot be encoded reached the Hub")
		reply(st, map[string]any{"type": proto.TypeOpResult, "result": struct{}{}})
	})
	c := NewCallClient(CallConfig{})
	defer h.link(c)()
	huge := ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{SessionName: "s"}, Text: strings.Repeat("x", proto.StreamFrameLimit+1)}
	for name, a := range map[string]any{"too large": huge, "does not encode": map[string]any{"f": func() {}}} {
		err := c.Do(context.Background(), ops.OpPipeSend, a, nil)
		var lost *CallLostError
		if err == nil || errors.As(err, &lost) {
			t.Errorf("%s: err = %v, want a plain error that says nothing was sent", name, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
}
