package workers

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/obs/compliance"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

type captureSink struct {
	mu  sync.Mutex
	rec []compliance.Record
}

func (s *captureSink) Emit(r compliance.Record) bool {
	s.mu.Lock()
	s.rec = append(s.rec, r)
	s.mu.Unlock()
	return true
}

func attr(r compliance.Record, key string) any {
	for _, a := range r.Attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return nil
}

func newMonitoredSCRig(t *testing.T, pol compliance.Policy, managed bool) (*scRig, *captureSink) {
	t.Helper()
	r := newRig(t, nil)
	g := &fakeGrants{}
	mon := compliance.NewMonitor(compliance.Deps{
		UserManaged:      func(context.Context, string) (bool, error) { return managed, nil },
		WorkerHasFeature: func(string, string) bool { return true },
	})
	sink := &captureSink{}
	mon.SetSink(sink)
	mon.SetPolicy(pol)
	cs := NewCallServer(CallServerConfig{
		Keys: newFakeKeys(), Resolver: r.rt, Owners: r.dir, Auditor: &memAudit{}, Grants: g, Authz: r.rt.az, Monitor: mon,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err := RegisterSessionControlCalls(cs, r.rt); err != nil {
		t.Fatal(err)
	}
	return &scRig{rig: r, cs: cs, grants: g, audit: &memAudit{}}, sink
}

// A session-control grant that types into a target on another Worker is
// recorded: the input as attempted before routing, the action with the
// outcome after, both with client "grant" and the same request id.
func TestCallServerRecordsGrantInputAndAction(t *testing.T) {
	r, sink := newMonitoredSCRig(t, compliance.Policy{Version: 4, Level: "actions", Input: true}, true)
	r.grants.set(t, wRootLab.ID, "boss", gAlicePi2, gBobBuild)
	args := scArgs(ops.OpSendText, ops.Target{WorkerID: wBobPi1.ID, SessionName: "build"})
	args["text"] = "rm -rf /tmp/x mn_" + strings.Repeat("b", 52)
	frames, _ := r.call(t, wRootLab.ID, scRequest(ops.OpSendText, args, "", "boss"), ops.WriteResult{Outcome: ops.OutcomeDelivered})
	var wr ops.WriteResult
	resultOf(t, frames, &wr)
	if len(sink.rec) != 2 || sink.rec[0].Event != compliance.EventInput || sink.rec[1].Event != compliance.EventAction {
		t.Fatalf("records = %+v", sink.rec)
	}
	in, act := sink.rec[0], sink.rec[1]
	if attr(in, "minion.client") != compliance.ClientGrant || attr(in, "minion.path") != "call" || attr(in, "minion.input.status") != "attempted" {
		t.Fatalf("input = %+v", in.Attrs)
	}
	if !strings.HasPrefix(in.Body, "rm -rf /tmp/x ") || strings.Contains(in.Body, strings.Repeat("b", 52)) {
		t.Fatalf("body = %q", in.Body)
	}
	if attr(act, "minion.outcome") != "delivered" || attr(act, "minion.request.id") != attr(in, "minion.request.id") ||
		attr(act, "minion.request.id") != args["request_id"] || attr(act, "minion.worker.id") != wBobPi1.ID {
		t.Fatalf("action = %+v", act.Attrs)
	}
}

// Out of scope for the grant: authorisation refuses, the record is a denial
// without content, and nothing reaches a Worker.
func TestCallServerDeniedCallHasNoContent(t *testing.T) {
	r, sink := newMonitoredSCRig(t, compliance.Policy{Version: 1, Level: "output", Input: true}, true)
	r.grants.set(t, wRootLab.ID, "boss", gBobBuild)
	args := scArgs(ops.OpSendText, ops.Target{WorkerID: wBobPi1.ID, SessionName: "other"})
	frames, _ := r.call(t, wRootLab.ID, scRequest(ops.OpSendText, args, "", "boss"), ops.WriteResult{Outcome: ops.OutcomeDelivered})
	_ = frames
	if len(sink.rec) != 1 || sink.rec[0].Event != compliance.EventAction || attr(sink.rec[0], "minion.outcome") != "denied" || sink.rec[0].Body != "" {
		t.Fatalf("records = %+v", sink.rec)
	}
	if r.be.openCount() != 0 {
		t.Fatal("a refused call reached a Worker")
	}
}

// A Worker that is not user-managed: actions at every level, never content.
func TestCallServerNotUserManagedIsActionsOnly(t *testing.T) {
	r, sink := newMonitoredSCRig(t, compliance.Policy{Version: 1, Level: "output", Input: true}, false)
	r.grants.set(t, wRootLab.ID, "boss", gBobBuild)
	args := scArgs(ops.OpSendText, ops.Target{WorkerID: wBobPi1.ID, SessionName: "build"})
	_, _ = r.call(t, wRootLab.ID, scRequest(ops.OpSendText, args, "", "boss"), ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if len(sink.rec) != 1 || sink.rec[0].Event != compliance.EventAction || sink.rec[0].Body != "" {
		t.Fatalf("records = %+v", sink.rec)
	}
}

// Monitoring off: the call server writes nothing and costs nothing.
func TestCallServerOffRecordsNothing(t *testing.T) {
	r, sink := newMonitoredSCRig(t, compliance.Policy{Version: 1, Level: "off"}, true)
	r.grants.set(t, wRootLab.ID, "boss", gBobBuild)
	args := scArgs(ops.OpSendText, ops.Target{WorkerID: wBobPi1.ID, SessionName: "build"})
	_, _ = r.call(t, wRootLab.ID, scRequest(ops.OpSendText, args, "", "boss"), ops.WriteResult{Outcome: ops.OutcomeDelivered})
	if len(sink.rec) != 0 {
		t.Fatalf("records = %+v", sink.rec)
	}
}

// pipe_read and pipe_wait name their session: the recorder decides the same
// resource the handler does, so an authorised read is not recorded as denied.
func TestTargetOfArgsKnowsPipeReadAndWait(t *testing.T) {
	tgt := ops.Target{WorkerID: "w1", SessionName: "s1"}
	for _, a := range []any{&ops.PipeReadArgs{Target: tgt}, &ops.PipeWaitArgs{Target: tgt}, &ops.PipeSendArgs{Target: tgt}} {
		got, ok := targetOfArgs(a)
		if !ok || got != tgt {
			t.Fatalf("%T: %v %v", a, got, ok)
		}
	}
}

// A write is settled with its Worker outcome even when the caller went away;
// a read is recorded only after its answer was written.
func TestCallServerSettlesWritesAndRecordsReadsAfterTheReply(t *testing.T) {
	r, sink := newMonitoredSCRig(t, compliance.Policy{Version: 1, Level: "output", Input: true}, true)
	r.grants.set(t, wRootLab.ID, "boss", gBobBuild)
	// A snapshot whose reply cannot be written: no output record, an action that says so.
	args := scArgs(ops.OpSessionSnapshot, ops.Target{WorkerID: wBobPi1.ID, SessionName: "build"})
	st := newCallStream(t, scRequest(ops.OpSessionSnapshot, args, "", "boss"))
	st.writeErr = errors.New("reset")
	sent := newFakeStream(resultFrame(t, ops.SessionSnapshotResult{Screen: ops.Screen{Lines: []string{"visible?"}}}))
	r.script(sent)
	r.cs.Serve(context.Background(), wRootLab.ID, st)
	for _, rec := range sink.rec {
		if rec.Event == compliance.EventOutput {
			t.Fatalf("output recorded for a reply that failed: %+v", rec)
		}
	}
	if n := len(sink.rec); n != 1 || attr(sink.rec[0], "minion.outcome") != "refused:reply_failed" {
		t.Fatalf("records = %+v", sink.rec)
	}
}
