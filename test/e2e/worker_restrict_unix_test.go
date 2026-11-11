//go:build e2e && unix

package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/workers"
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

// Story 6.6 integration test: the real Hub (connect endpoint, registry,
// Restrictions), the real Worker connector, op server and executor over a real
// tmux server on a private socket. The Hub restricts the Worker and a
// send_text through the Hub is refused with preview_only; the Worker's own
// local restriction survives the Hub lifting its own.

type restrictWorker struct {
	conn   *agent.Connector
	policy *agent.Policy
	key    *identity.Key
	srv    *tmuxtest.Server
	target string
}

func startRestrictWorker(t *testing.T, h *connHub) *restrictWorker {
	t.Helper()
	dir := t.TempDir()
	st, err := wstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveShare(wstore.ShareSelection{Mode: wstore.ShareAll}); err != nil {
		t.Fatal(err)
	}
	enr := agent.NewEnrollments(st, dir)
	key, err := enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := enr.Commit(context.Background(), agent.Enrollment{Pin: h.pin()}); err != nil {
		t.Fatal(err)
	}
	tsrv := tmuxtest.New(t)
	host, err := tmux.New(tsrv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	policy := agent.NewPolicy(st)
	ex, err := agent.NewExecutor(agent.Config{
		Host: host, Classifier: agentdetect.New(), State: st, Policy: policy, LockWait: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := agent.NewOpServer(agent.OpServerConfig{Executor: ex, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := agent.NewConnector(agent.ConnectorConfig{
		HubURL:     h.url(),
		Name:       "pi",
		Ops:        func() []string { return []string{ops.OpSessionsList, ops.OpSendText, ops.OpPipeSend} },
		Features:   ex.Features, // what the real executor implements, as the production connector announces
		Key:        enr.LoadKey,
		Enrollment: enr,
		Policy:     policy,
		Dialer:     agent.NewHubDialer(),
		Backoff:    &transport.Backoff{Initial: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		IdlePoll:   20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn.OnLink("op-server", srv.Serve)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); conn.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the connector did not stop")
		}
		if err := srv.Stop(context.Background()); err != nil {
			t.Errorf("stop the op server: %v", err)
		}
	})
	return &restrictWorker{conn: conn, policy: policy, key: key, srv: tsrv, target: tsrv.NewSession("catwork", "cat")}
}

// hubSendText sends send_text to the Worker through the Hub's op stream, with
// the actions the Hub computes for an owner (operate, read), and returns the
// first frame of the answer.
func hubSendText(t *testing.T, svc *workers.Service, workerID, text string) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := svc.Registry().OpenOp(ctx, workerID, ops.OpSendText)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	args := ops.SendTextArgs{
		WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()},
		Target:    ops.Target{WorkerID: workerID, SessionName: "catwork"},
		Text:      text,
	}
	if err := st.WriteFrame(map[string]any{
		"type": proto.TypeOpRequest, "op": ops.OpSendText,
		"actions": []ops.Action{ops.ActionOperate, ops.ActionRead}, "args": args,
		"origin": proto.Origin{Kind: proto.OriginUser, UserID: "user-1"},
	}); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		body json.RawMessage
		err  error
	}
	ch := make(chan answer, 1)
	go func() {
		b, err := st.ReadFrame()
		ch <- answer{b, err}
	}()
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

func frameKind(m map[string]json.RawMessage) string {
	var s string
	_ = json.Unmarshal(m["type"], &s)
	return s
}

func TestHubRestrictionRefusesSendTextAndTheLocalRestrictionOutlivesIt(t *testing.T) {
	h := newConnHub(t)
	rs, err := workers.NewRestrictions(workers.RestrictionsConfig{Store: h.st, Connections: h.service().Registry()})
	if err != nil {
		t.Fatal(err)
	}
	h.service().UsePolicy(rs.Policy)
	w := startRestrictWorker(t, h)
	row := h.enrol(w.key.Public(), "pi")
	eventually(t, "the Worker to be online", func() bool { return h.service().Registry().IsOnline(row.ID) })
	eventually(t, "the Worker to be connected", func() bool { return w.conn.Status().State == agent.ConnConnected })

	// Free Worker: the write is delivered to the pane.
	if m := hubSendText(t, h.service(), row.ID, "first-line"); frameKind(m) != proto.TypeOpResult {
		t.Fatalf("answer before the restriction = %v", m)
	}
	w.srv.WaitFor(w.target, "first-line", 10*time.Second)

	// The Hub restricts the Worker; the connection closes, the reconnect
	// reads the policy.
	res, err := rs.Restrict(context.Background(), workers.Actor{ID: "owner-1"}, row.ID, true)
	if err != nil || !res.Changed || !res.Disconnected {
		t.Fatalf("Restrict = %+v, %v", res, err)
	}
	eventually(t, "the Worker to learn the policy on reconnect", func() bool {
		return w.policy.HubPreviewOnly() && h.service().Registry().IsOnline(row.ID)
	})
	m := hubSendText(t, h.service(), row.ID, "second-line")
	if frameKind(m) != proto.TypeError || !strings.Contains(string(m["code"]), "forbidden") || !strings.Contains(string(m["details"]), "preview_only") {
		t.Fatalf("answer after the restriction = %v", m)
	}
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(w.srv.Capture(w.target), "second-line") {
		t.Fatal("text reached the pane of a preview-only Worker")
	}

	// The Worker's own restriction is never relaxed by the Hub: with it on,
	// lifting the Hub restriction leaves the Worker preview-only.
	w.policy.SetPreviewOnly(true)
	if _, err := rs.Restrict(context.Background(), workers.Actor{ID: "owner-1"}, row.ID, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Worker to reconnect without the Hub restriction", func() bool {
		return !w.policy.HubPreviewOnly() && h.service().Registry().IsOnline(row.ID) && w.conn.Status().State == agent.ConnConnected
	})
	if !w.policy.PreviewOnly() {
		t.Fatal("the Hub lifted the Worker's local restriction")
	}
	m = hubSendText(t, h.service(), row.ID, "third-line")
	if frameKind(m) != proto.TypeError || !strings.Contains(string(m["details"]), "preview_only") {
		t.Fatalf("answer with only the local restriction = %v", m)
	}

	// Local restriction off and Hub restriction off: writes work again.
	w.policy.SetPreviewOnly(false)
	if m := hubSendText(t, h.service(), row.ID, "fourth-line"); frameKind(m) != proto.TypeOpResult {
		t.Fatalf("answer after both restrictions are lifted = %v", m)
	}
	w.srv.WaitFor(w.target, "fourth-line", 10*time.Second)
}
