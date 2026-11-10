//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/api"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/workers"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	wagent "github.com/patrick-1984/ai-minion/internal/worker/agent"
	wstore "github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Story 9.5 composition case: a pipe turn is committed once across a lost
// turn.ack and a reconnect, and a model key does not see it until the source
// is trusted. The Hub side is the real Hub (connect endpoint, event ingest,
// Pipe.ServeTurns over the real stream, the SQLite store, the real trust
// table). The Worker side is the production connector with the real
// TurnEmitter on the link and the store-backed outbox. The only interference
// is the cut: the first turn.ack the Hub writes is swallowed and the
// connection is dropped, which is what a lost ack looks like.
//
// The turn itself is handed to the emitter, the point the real pane extractor
// feeds. Extracting a turn from a pane needs tmux 3.8 (A-18) and is covered by
// the tmux 3.8 job of the Worker package; on an older tmux the truthful result
// is the reason tmux_too_old and no turn, never a made-up transcript.

type ackDropper struct {
	*transport.Stream
	dropped *atomic.Int32
	cut     func()
}

func (s *ackDropper) WriteFrame(v any) error {
	b, err := json.Marshal(v)
	if err == nil {
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &head) == nil && head.Type == proto.TypeTurnAck && s.dropped.Add(1) == 1 {
			s.cut() // the ack is never written; the connection is lost
			return nil
		}
	}
	return s.Stream.WriteFrame(v)
}

// scopeAll is an API key scope that may read and message anything of its owner.
type scopeAll struct{}

func (scopeAll) Actions(authz.ResolvedResource) []ops.Action {
	return []ops.Action{ops.ActionRead, ops.ActionMessage}
}

type noPipeRouter struct{}

func (noPipeRouter) Call(context.Context, workers.CallRequest) (any, error) {
	return nil, proto.NewError(proto.CodeInternal, "this test sends no message through the Router")
}

func TestADroppedTurnAckAndAReconnectCommitOneTurnAndAModelKeyDoesNotSeeItUntrusted(t *testing.T) {
	h := newConnHub(t)
	dir := storeDirectory{workers.StoreDirectory{Store: h.st}}
	az := authz.New(userLookup{}, dir)
	trust := authz.NewTrust(api.TrustStore(h.st), nil)
	pp, err := workers.NewPipe(workers.PipeConfig{
		Store: h.st.Pipe(), Authz: az, Router: noPipeRouter{}, Trust: trust, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	ing, err := workers.NewIngest(workers.IngestOptions{Hub: workers.NewEventHub(workers.EventHubOptions{})})
	if err != nil {
		t.Fatal(err)
	}
	h.service().Registry().OnAdd(ing.Attach)
	var dropped atomic.Int32
	if !ing.OnStream(workers.FrameTurnsOpen, func(ctx context.Context, info workers.Online, st *transport.Stream, _ json.RawMessage) {
		pp.ServeTurns(ctx, info.WorkerID, &ackDropper{Stream: st, dropped: &dropped, cut: func() {
			h.service().Registry().Disconnect(info.WorkerID)
		}})
	}) {
		t.Fatal("the ingest already serves turns.open")
	}

	// The Worker: real state on disk, share all, store-backed outbox.
	w := newCompWorkerState(t)
	key, err := w.enr.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.enr.Commit(context.Background(), wagent.Enrollment{Pin: h.pin()}); err != nil {
		t.Fatal(err)
	}
	if err := w.st.SaveShare(wstore.ShareSelection{Mode: wstore.ShareAll}); err != nil {
		t.Fatal(err)
	}
	row := h.enrol(key.Public(), "pi")
	outbox := wagent.NewStoreOutbox(w.st, 0)
	em, err := wagent.NewTurnEmitter(wagent.TurnConfig{Shares: wagent.NewPolicy(w.st), Outbox: outbox})
	if err != nil {
		t.Fatal(err)
	}
	conn, _, _ := w.connect(h, proto.Range{})
	conn.OnLink("turns", em.Serve)
	eventually(t, "the Worker to be online", func() bool { return h.service().Registry().IsOnline(row.ID) })

	// A delivery of the Hub (what pipe_send stores) and the agent's reply.
	peer := pipe.Peer{WorkerID: row.ID, SessionName: "minion-1"}
	msg, err := h.st.Pipe().AppendMessage(context.Background(), pipe.Message{Peer: peer, Text: "hello agent", At: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := em.Emit(wagent.PendingTurn{MessageID: msg.ID, Session: "minion-1", Text: "the reply of the agent"}); err != nil {
		t.Fatal(err)
	}

	eventually(t, "the first ack to be dropped", func() bool { return dropped.Load() >= 1 })
	eventually(t, "the Worker to be acknowledged after the reconnect", func() bool {
		p, err := outbox.Pending()
		return err == nil && len(p) == 0
	})
	turns, err := h.st.Pipe().Turns(context.Background(), peer, "", 10)
	if err != nil || len(turns) != 1 || turns[0].Text != "the reply of the agent" {
		t.Fatalf("turns at the Hub = %+v, %v; want exactly one", turns, err)
	}
	time.Sleep(300 * time.Millisecond) // nothing may send it a third time
	if turns, _ = h.st.Pipe().Turns(context.Background(), peer, "", 10); len(turns) != 1 {
		t.Fatalf("%d turns after the settle", len(turns))
	}
	if n := dropped.Load(); n < 2 {
		t.Fatalf("the Hub wrote %d acks; the resent turn must be acknowledged again", n)
	}

	// Public entry: pipe_read. A person sees the turn; a model key does not
	// until its owner trusts the source, and is told what was withheld.
	read := func(p authz.Principal) ops.PipeReadResult {
		t.Helper()
		v, err := pp.Read(context.Background(), p, ops.PipeReadArgs{Target: ops.Target{WorkerID: row.ID, SessionName: "minion-1"}})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	received := func(v ops.PipeReadResult) int {
		n := 0
		for _, e := range v.Turns {
			if e.Direction == ops.PipeReceived {
				n++
			}
		}
		return n
	}
	person := authz.NewSessionPrincipal("owner-1")
	model := authz.NewAPIKeyPrincipal("owner-1", scopeAll{}, "")
	if v := read(person); received(v) != 1 || v.Withheld != 0 {
		t.Fatalf("a person's pipe_read = %+v", v)
	}
	v := read(model)
	if received(v) != 0 || v.Withheld != 1 || v.Reason != workers.ReasonUntrustedPeer || v.Notice == "" {
		t.Fatalf("an untrusting model key's pipe_read = %+v; want the turn withheld", v)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // pipe_wait of an untrusted source ends at once with untrusted_peer
		defer wg.Done()
		err := pp.Wait(context.Background(), model, ops.PipeWaitArgs{Target: ops.Target{WorkerID: row.ID, SessionName: "minion-1"}, TimeoutMS: 2000},
			func(ops.PipeTurn) error { t.Error("a withheld turn was streamed to a model"); return nil })
		if proto.CodeOf(err) != proto.CodeUntrustedPeer {
			t.Errorf("pipe_wait err = %v, want untrusted_peer", err)
		}
	}()
	wg.Wait()
	if err := trust.Set(context.Background(), "owner-1", row.ID, "minion-1"); err != nil {
		t.Fatal(err)
	}
	if v := read(model); received(v) != 1 || v.Withheld != 0 {
		t.Fatalf("after trust: %+v", v)
	}
}
