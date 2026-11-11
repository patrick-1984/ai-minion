//go:build e2e && unix

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// Story 9.5 composition cases: the real Hub (connect endpoint, registry, op
// streams, and for the cut write the real Router), the real connector, op
// server and executor, and a real tmux server on a private socket (via
// tmuxtest). The pane is the evidence: every count below is read from the
// terminal, not from a mock.

// hubWrite sends one op.request on a Hub op stream and returns the first
// answer frame. It is hubSendText with the op, the actions and the arguments
// chosen by the caller.
func hubWrite(t *testing.T, svc *workers.Service, workerID, op string, actions []ops.Action, args any) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := svc.Registry().OpenOp(ctx, workerID, op)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.WriteFrame(map[string]any{"type": proto.TypeOpRequest, "op": op, "actions": actions, "args": args,
		// What the Hub derives from the caller (protocol 2.0): a Worker refuses a message-class op without it.
		"origin": proto.Origin{Kind: proto.OriginUser, UserID: "user-1"}}); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		body json.RawMessage
		err  error
	}
	ch := make(chan answer, 1)
	go func() { b, err := st.ReadFrame(); ch <- answer{b, err} }()
	select {
	case a := <-ch:
		if a.err != nil {
			t.Fatalf("reading the answer: %v", a.err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(a.body, &m); err != nil {
			t.Fatal(err)
		}
		return m
	case <-ctx.Done():
		t.Fatal("no answer from the Worker")
		return nil
	}
}

func writeResult(t *testing.T, m map[string]json.RawMessage) ops.WriteResult {
	t.Helper()
	if k := frameKind(m); k != proto.TypeOpResult {
		t.Fatalf("answer is %s, not a result: %v", k, m)
	}
	var wr ops.WriteResult
	if err := json.Unmarshal(m["result"], &wr); err != nil {
		t.Fatal(err)
	}
	return wr
}

func sendTextArgs(workerID, session, text, id string, force bool) ops.SendTextArgs {
	return ops.SendTextArgs{
		WriteArgs: ops.WriteArgs{RequestID: id, Force: force},
		Target:    ops.Target{WorkerID: workerID, SessionName: session}, Text: text,
	}
}

var operateRead = []ops.Action{ops.ActionOperate, ops.ActionRead}

func onlineRestrictWorker(t *testing.T) (*connHub, *restrictWorker, string) {
	t.Helper()
	h := newConnHub(t)
	w := startRestrictWorker(t, h)
	row := h.enrol(w.key.Public(), "pi")
	eventually(t, "the Worker to be online", func() bool { return h.service().Registry().IsOnline(row.ID) })
	eventually(t, "the Worker to be connected", func() bool { return w.conn.Status().State == wagent.ConnConnected })
	return h, w, row.ID
}

func TestADuplicateRequestIDAtTheRealHubIsAnsweredFromTheCacheAndThePaneGetsTheTextOnce(t *testing.T) {
	h, w, id := onlineRestrictWorker(t)
	rid := ops.NewUUIDv7()
	args := sendTextArgs(id, "catwork", "dupmark", rid, false)

	first := writeResult(t, hubWrite(t, h.service(), id, ops.OpSendText, operateRead, args))
	if first.Outcome != ops.OutcomeDelivered {
		t.Fatalf("first = %+v", first)
	}
	w.srv.WaitFor(w.target, "dupmark", 10*time.Second)

	// The same request id again, also over a reconnect of the whole link.
	second := writeResult(t, hubWrite(t, h.service(), id, ops.OpSendText, operateRead, args))
	h.service().Registry().Disconnect(id)
	eventually(t, "the Worker to reconnect", func() bool {
		return h.service().Registry().IsOnline(id) && w.conn.Status().State == wagent.ConnConnected
	})
	third := writeResult(t, hubWrite(t, h.service(), id, ops.OpSendText, operateRead, args))
	if second.Outcome != ops.OutcomeDelivered || third.Outcome != ops.OutcomeDelivered {
		t.Fatalf("duplicates = %+v, %+v", second, third)
	}
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(w.srv.Capture(w.target), "dupmark"); n != 1 {
		t.Fatalf("the pane shows the text %d times after three identical requests:\n%s", n, w.srv.Capture(w.target))
	}
}

// cutBackend is the real registry backend that cuts the Worker's connection
// right after the op request was written, and counts the streams it opened.
type cutBackend struct {
	workers.Backend
	svc   *workers.Service
	opens atomic.Int32
	cut   atomic.Bool
}

func (b *cutBackend) OpenOp(ctx context.Context, workerID, op string, need ...string) (workers.OpStream, error) {
	b.opens.Add(1)
	st, err := b.Backend.OpenOp(ctx, workerID, op, need...)
	if err != nil {
		return nil, err
	}
	return &cutStream{OpStream: st, b: b, workerID: workerID}, nil
}

type cutStream struct {
	workers.OpStream
	b        *cutBackend
	workerID string
	once     sync.Once
}

func (s *cutStream) WriteFrame(v any) error {
	err := s.OpStream.WriteFrame(v)
	if err == nil && s.b.cut.Load() {
		s.once.Do(func() { s.b.svc.Registry().Disconnect(s.workerID) })
	}
	return err
}

func TestATransportCutDuringAWriteIsUnknownAtTheRealRouterAndIsNeverRetried(t *testing.T) {
	h, w, id := onlineRestrictWorker(t)
	be := &cutBackend{Backend: workers.RegistryBackend(h.service().Registry()), svc: h.service()}
	dir := storeDirectory{workers.StoreDirectory{Store: h.st}}
	rt, err := workers.NewRouter(workers.RouterConfig{
		Backend: be, Directory: dir, Authz: authz.New(userLookup{}, dir),
	})
	if err != nil {
		t.Fatal(err)
	}
	rid := ops.NewUUIDv7()
	args := sendTextArgs(id, "catwork", "cutmark", rid, false)
	be.cut.Store(true)
	res, err := rt.Call(context.Background(), workers.CallRequest{
		Principal: authz.NewSessionPrincipal("owner-1"), Op: ops.OpSendText, WorkerID: id, Session: "catwork", Args: args,
	})
	be.cut.Store(false)
	if err != nil {
		t.Fatalf("an unknown write is a result, not an error: %v", err)
	}
	wr, ok := res.(ops.WriteResult)
	if !ok || wr.Outcome != ops.OutcomeUnknown {
		t.Fatalf("result = %#v, want outcome unknown", res)
	}
	if n := be.opens.Load(); n != 1 {
		t.Fatalf("the Router opened %d streams for one write; a write is never retried", n)
	}

	// Nothing retries: after the Worker is back the pane shows the text at
	// most once (the Worker may have applied the write before the cut).
	eventually(t, "the Worker to reconnect", func() bool {
		return h.service().Registry().IsOnline(id) && w.conn.Status().State == wagent.ConnConnected
	})
	time.Sleep(500 * time.Millisecond)
	if n := strings.Count(w.srv.Capture(w.target), "cutmark"); n > 1 {
		t.Fatalf("the pane shows the text %d times", n)
	}
	if n := be.opens.Load(); n != 1 {
		t.Fatalf("a later retry opened another stream (%d)", n)
	}

	// Only the caller may decide to ask again, with the same request id: the
	// pane then has the text exactly once, whether the Worker had applied the
	// first request (cache) or not (applied now).
	again := writeResult(t, hubWrite(t, h.service(), id, ops.OpSendText, operateRead, args))
	if again.Outcome != ops.OutcomeDelivered {
		t.Fatalf("the explicit replay = %+v", again)
	}
	w.srv.WaitFor(w.target, "cutmark", 10*time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(w.srv.Capture(w.target), "cutmark"); n != 1 {
		t.Fatalf("the pane shows the text %d times after the replay:\n%s", n, w.srv.Capture(w.target))
	}
}

func TestAShellPaneRefusesThenTakesAnExplicitlyForcedSendAndAMessageOnlyKeyIsDeniedTheShell(t *testing.T) {
	h, w, id := onlineRestrictWorker(t)
	shell := w.srv.NewSession("shellwork") // the default shell
	time.Sleep(500 * time.Millisecond)     // let it draw its prompt

	refused := writeResult(t, hubWrite(t, h.service(), id, ops.OpSendText, operateRead,
		sendTextArgs(id, "shellwork", "forcemark", ops.NewUUIDv7(), false)))
	if refused.Outcome != ops.OutcomeFailedNotSent || refused.Reason != "shell_pane" || !refused.Forceable {
		t.Fatalf("refusal = %+v", refused)
	}
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(w.srv.Capture(shell), "forcemark") {
		t.Fatal("a refused write reached the shell")
	}

	// A key with only "message" is denied the shell, with or without force:
	// force overrides guards of the pane, never the missing permission.
	for _, force := range []bool{false, true} {
		m := hubWrite(t, h.service(), id, ops.OpSendText, []ops.Action{ops.ActionMessage},
			sendTextArgs(id, "shellwork", "denymark", ops.NewUUIDv7(), force))
		if frameKind(m) != proto.TypeError || !strings.Contains(string(m["code"]), string(proto.CodeForbidden)) {
			t.Fatalf("message-only, force=%v: answer = %v", force, m)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(w.srv.Capture(shell), "denymark") {
		t.Fatal("a message-only key typed into a shell")
	}

	forced := writeResult(t, hubWrite(t, h.service(), id, ops.OpSendText, operateRead,
		sendTextArgs(id, "shellwork", "forcemark", ops.NewUUIDv7(), true)))
	if forced.Outcome != ops.OutcomeDelivered || !forced.Forced {
		t.Fatalf("forced = %+v", forced)
	}
	w.srv.WaitFor(shell, "forcemark", 10*time.Second)
}
