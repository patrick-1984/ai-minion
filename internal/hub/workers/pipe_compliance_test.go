package workers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	"github.com/patrick-1984/ai-minion/internal/obs/otlptest"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// memSink collects the events of a Monitor.
type memSink struct {
	mu  sync.Mutex
	got []otlptest.Event
}

func (s *memSink) EmitEvent(name string, _ time.Time, body string, attrs []compliance.KV) {
	e := otlptest.Event{Name: name, Body: body, Attrs: map[string]string{}}
	for _, a := range attrs {
		if v, ok := a.Value.(string); ok {
			e.Attrs[a.Key] = v
		}
	}
	s.mu.Lock()
	s.got = append(s.got, e)
	s.mu.Unlock()
}

func (s *memSink) take() []otlptest.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.got
	s.got = nil
	return out
}

func eventTexts(evs []otlptest.Event, name string) []string {
	var out []string
	for _, e := range evs {
		if e.Name == name {
			out = append(out, e.Body)
		}
	}
	return out
}

// TestPipeComplianceLevels: the broker records pipe traffic once, received
// turns from output on, sent text only at full (also when a read returns it
// again), and nothing of a turn that a read withheld.
func TestPipeComplianceLevels(t *testing.T) {
	ctx := context.Background()
	sink := &memSink{}
	mon := compliance.New(func() compliance.Sink { return sink })
	r := newPipeRigWith(t, 0, func(c *PipeConfig) { c.Monitor = mon })
	alice := authz.NewSessionPrincipal(idAlice)
	read := ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)}

	// off: nothing.
	if _, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "secret question")); err != nil {
		t.Fatal(err)
	}
	r.commit(t, wAlicePi1, pipeSess, "answer text")
	if _, err := r.p.Read(ctx, alice, read); err != nil {
		t.Fatal(err)
	}
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("off recorded %+v", got)
	}

	// actions: metadata only, once per call.
	mon.SetMode(compliance.Actions)
	if _, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "q2")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.p.Read(ctx, alice, read); err != nil {
		t.Fatal(err)
	}
	got := sink.take()
	if len(got) != 2 || got[0].Attrs["minion.op"] != "pipe_send" || got[1].Attrs["minion.op"] != "pipe_read" || got[0].Attrs["minion.outcome"] != "delivered" {
		t.Fatalf("actions recorded %+v", got)
	}
	for _, e := range got {
		if e.Body != "" || e.Name != compliance.EventAction {
			t.Fatalf("content at actions level: %+v", e)
		}
	}

	// output: received turns, not the sent text.
	mon.SetMode(compliance.Output)
	if _, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "q3")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.p.Read(ctx, alice, read); err != nil {
		t.Fatal(err)
	}
	got = sink.take()
	if o := eventTexts(got, compliance.EventOutput); len(o) != 1 || o[0] != "answer text" {
		t.Fatalf("output level recorded %v", o)
	}
	if in := eventTexts(got, compliance.EventInput); len(in) != 0 {
		t.Fatalf("input at output level: %v", in)
	}

	// full: the sent messages too, as sent and as read back.
	mon.SetMode(compliance.Full)
	if _, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "q4")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.p.Read(ctx, alice, read); err != nil {
		t.Fatal(err)
	}
	got = sink.take()
	// Sent text is input, recorded when it is sent; the history a read returns
	// again is not exported (a stored row can be pending or refused).
	in := strings.Join(eventTexts(got, compliance.EventInput), "|")
	if in != "q4" {
		t.Fatalf("full recorded input %q, want only the send", in)
	}
	if o := strings.Join(eventTexts(got, compliance.EventOutput), "|"); strings.Contains(o, "secret question") || strings.Contains(o, "q2") || !strings.Contains(o, "answer text") {
		t.Fatalf("full read recorded output %q", o)
	}

	// a model that does not trust the source: the turn is withheld, so it is
	// not recorded; the action says how many.
	sink.take()
	if _, err := r.p.Read(ctx, modelAlice(), read); err != nil {
		t.Fatal(err)
	}
	got = sink.take()
	if strings.Contains(strings.Join(eventTexts(got, compliance.EventOutput), "|"), "answer text") {
		t.Fatal("a withheld turn was recorded")
	}
	if len(got) == 0 || got[0].Name != compliance.EventAction {
		t.Fatalf("no action for the read: %+v", got)
	}

	// a caller that is not allowed is not recorded.
	if _, err := r.p.Read(ctx, authz.NewSessionPrincipal(idBob), read); err == nil {
		t.Fatal("bob read alice's pipe")
	}
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("a denied read was recorded: %+v", got)
	}
}

// TestPipeComplianceSkipsWorkersThatAreNotUserManaged: the action stays, the
// text does not.
func TestPipeComplianceSkipsWorkersThatAreNotUserManaged(t *testing.T) {
	ctx := context.Background()
	sink := &memSink{}
	mon := compliance.New(func() compliance.Sink { return sink })
	mon.SetMode(compliance.Full)
	r := newPipeRigWith(t, 0, func(c *PipeConfig) {
		c.Monitor = mon
		c.ContentAllowed = func(context.Context, string) bool { return false }
	})
	alice := authz.NewSessionPrincipal(idAlice)
	if _, err := r.p.Send(ctx, alice, sendArgs(wAlicePi1, pipeSess, "hidden")); err != nil {
		t.Fatal(err)
	}
	r.commit(t, wAlicePi1, pipeSess, "hidden reply")
	if _, err := r.p.Read(ctx, alice, ops.PipeReadArgs{Target: target(wAlicePi1, pipeSess)}); err != nil {
		t.Fatal(err)
	}
	for _, e := range sink.take() {
		if e.Name != compliance.EventAction || e.Body != "" {
			t.Fatalf("content of a Worker that is not user-managed: %+v", e)
		}
	}
}

// TestRouterComplianceSkipsPipeOpsAndForeignContent: the Router leaves pipe
// ops to the broker and records no content for a Worker that is not
// user-managed.
func TestRouterComplianceSkipsPipeOpsAndForeignContent(t *testing.T) {
	ctx := context.Background()
	sink := &memSink{}
	mon := compliance.New(func() compliance.Sink { return sink })
	mon.SetMode(compliance.Full)
	r := newRig(t, func(c *RouterConfig) {
		c.Monitor = mon
		// Control is allowed when the call is checked (no person may call a
		// Worker that is not user-managed, a master session's grant may) and
		// the Worker reads as not user-managed when content is asked for.
		answers := []bool{true, true, false} // the pipe_send check, the send_text check, its content
		c.UserManaged = func(context.Context, string) (bool, error) {
			a := answers[0]
			answers = answers[1:]
			return a, nil
		}
	})
	target := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}
	r.script(newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})), newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})))
	if _, err := r.rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: ops.OpPipeSend, WorkerID: target.WorkerID, Session: target.SessionName,
		Args: ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target, Text: "m"}}); err != nil {
		t.Fatal(err)
	}
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("the router recorded a pipe op: %+v", got)
	}
	if _, err := r.rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: ops.OpSendText, WorkerID: target.WorkerID, Session: target.SessionName,
		Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target, Text: "typed"}}); err != nil {
		t.Fatal(err)
	}
	got := sink.take()
	if len(got) != 1 || got[0].Name != compliance.EventAction || got[0].Body != "" {
		t.Fatalf("recorded %+v, want the action only", got)
	}
}

// A Worker that refuses a delivery (pipe_origin_refused) or a Router call that
// the Worker answers with an error exports no input: only the action.
func TestComplianceRecordsInputOnlyWhenTheWorkerTookIt(t *testing.T) {
	ctx := context.Background()
	sink := &memSink{}
	mon := compliance.New(func() compliance.Sink { return sink })
	mon.SetMode(compliance.Full)

	r := newRig(t, func(c *RouterConfig) { c.Monitor = mon })
	target := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}
	r.script(newFakeStream(errorFrame(t, proto.CodeForbidden)))
	_, err := r.rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: ops.OpSendText, WorkerID: target.WorkerID, Session: target.SessionName,
		Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target, Text: "refused text"}})
	if err == nil {
		t.Fatal("the Worker's refusal was not returned")
	}
	got := sink.take()
	if len(got) != 1 || got[0].Name != compliance.EventAction || got[0].Attrs["minion.outcome"] != "forbidden" {
		t.Fatalf("recorded %+v, want only the action", got)
	}
	// An unknown error code from a Worker never becomes an attribute.
	r.script(newFakeStream(errorFrame(t, proto.Code("secret mn_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"))))
	_, _ = r.rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: ops.OpSendText, WorkerID: target.WorkerID, Session: target.SessionName,
		Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target, Text: "x"}})
	if got := sink.take(); len(got) != 1 || got[0].Attrs["minion.outcome"] == "" || strings.Contains(got[0].Attrs["minion.outcome"], "mn_") {
		t.Fatalf("outcome of an unknown code: %+v", got)
	}

	// The pipe broker: a delivery the receiver refused is not input.
	pr := newPipeRigWith(t, 0, func(c *PipeConfig) { c.Monitor = mon })
	pr.rt.err = proto.NewError(proto.CodePipeOriginRefused, "refused")
	_, _ = pr.p.Send(ctx, authz.NewSessionPrincipal(idAlice), sendArgs(wAlicePi1, pipeSess, "refused message"))
	got = sink.take()
	if len(got) != 1 || got[0].Name != compliance.EventAction || got[0].Attrs["minion.outcome"] != string(proto.CodePipeOriginRefused) {
		t.Fatalf("pipe recorded %+v, want only the action", got)
	}
}

// A Worker that is not known (being removed) is not user-managed for content.
func TestRouterContentFollowsTheContentCheck(t *testing.T) {
	ctx := context.Background()
	sink := &memSink{}
	mon := compliance.New(func() compliance.Sink { return sink })
	mon.SetMode(compliance.Full)
	r := newRig(t, func(c *RouterConfig) {
		c.Monitor = mon
		c.ContentManaged = func(context.Context, string) (bool, error) { return false, errors.New("unknown Worker") }
	})
	target := ops.Target{WorkerID: wAlicePi1.ID, SessionName: "minion-1"}
	r.script(newFakeStream(resultFrame(t, ops.WriteResult{Outcome: ops.OutcomeDelivered})))
	if _, err := r.rt.Call(ctx, CallRequest{Principal: routeAlice(), Op: ops.OpSendText, WorkerID: target.WorkerID, Session: target.SessionName,
		Args: ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target, Text: "x"}}); err != nil {
		t.Fatal(err)
	}
	if got := sink.take(); len(got) != 1 || got[0].Body != "" {
		t.Fatalf("recorded %+v", got)
	}
}

// Keys typed one by one and rows of a wrapped secret are masked whole.
func TestComplianceMasksKeysAndWrappedRows(t *testing.T) {
	key := "mn_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst"
	var keys []string
	for _, c := range key {
		keys = append(keys, string(c))
	}
	_, text := inputText(ops.SendKeysArgs{Keys: append(keys, "Enter")})
	sink := &memSink{}
	mon := compliance.New(func() compliance.Sink { return sink })
	mon.SetMode(compliance.Full)
	mon.Emit(compliance.Full, compliance.EventInput, text)
	mon.Emit(compliance.Full, compliance.EventOutput, "$ echo "+key[:30]+"\n"+key[30:]+"\nnext")
	for _, e := range sink.take() {
		if strings.Contains(e.Body, key[:12]) || strings.Contains(e.Body, key[30:42]) {
			t.Fatalf("secret in %q", e.Body)
		}
	}
}

// A retry with the same request id is answered from the stored result: the
// replacement text was not delivered and is not exported.
func TestPipeComplianceCachedReplyExportsNoReplacementText(t *testing.T) {
	ctx := context.Background()
	sink := &memSink{}
	mon := compliance.New(func() compliance.Sink { return sink })
	mon.SetMode(compliance.Full)
	r := newPipeRigWith(t, 0, func(c *PipeConfig) { c.Monitor = mon })
	alice := authz.NewSessionPrincipal(idAlice)
	a := sendArgs(wAlicePi1, pipeSess, "original")
	if _, err := r.p.Send(ctx, alice, a); err != nil {
		t.Fatal(err)
	}
	sink.take()
	a.Text = "replacement"
	_, _ = r.p.Send(ctx, alice, a)
	for _, e := range sink.take() {
		if e.Name == compliance.EventInput || strings.Contains(e.Body, "replacement") {
			t.Fatalf("exported %+v", e)
		}
	}
}
