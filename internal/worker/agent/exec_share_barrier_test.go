package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// The Hub numbers its share_set calls (op_seq) and asks for the rule with a probe
// that raises a barrier: a change at or below the barrier is refused and
// changes nothing, so an answer to a probe proves that no earlier change can
// still be applied (S4.15 residual R2-02).

func (e *env) probe(barrier uint64) ops.ShareProbe {
	e.t.Helper()
	p, err := e.ex.ShareProbe(context.Background(), barrier)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func setSeq(mode string, seq uint64, names ...string) ops.ShareSetArgs {
	a := shareSetArgs(mode, names...)
	a.OpSeq = seq
	a.WithRevision = true
	return a
}

func TestAShareSetAtOrBelowTheBarrierIsRefusedAndChangesNothing(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareAll}, "a", "b")
	if res := e.mustShare(ops.OpShareSet, setSeq("select", 10, "a")); res.OpBarrier != "10" || res.Rule.Mode != "select" {
		t.Fatalf("share_set 10 = %+v, want barrier 10", res)
	}
	revAfter := e.mustShare(ops.OpShareGet, ops.ShareGetArgs{WorkerID: shareWorkerID, WithRevision: true}).Revision
	for _, seq := range []uint64{10, 9, 1} { // the same change repeated, and older ones that arrive late
		_, err := e.share(ops.OpShareSet, operateActions, setSeq("all", seq))
		if ae, ok := AsAPIError(err); !ok || ae.Code != proto.CodeConflict {
			t.Fatalf("share_set seq %d = %v, want conflict", seq, err)
		}
	}
	if sel := e.saved(); sel.Mode != store.ShareSelect {
		t.Fatalf("a refused change was applied: %+v", sel)
	}
	if got := e.mustShare(ops.OpShareGet, ops.ShareGetArgs{WorkerID: shareWorkerID, WithRevision: true}).Revision; got != revAfter {
		t.Fatalf("a refused change moved the revision %s -> %s", revAfter, got)
	}
	// A newer change is applied and raises the barrier.
	if res := e.mustShare(ops.OpShareSet, setSeq("none", 11)); res.OpBarrier != "11" || res.Rule.Mode != "none" {
		t.Fatalf("share_set 11 = %+v", res)
	}
}

func TestAProbeRaisesTheBarrierBeforeItReadsTheRule(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareAll}, "a", "b")
	p := e.probe(50)
	if p.OpBarrier != "50" || p.Rule.Mode != "all" || p.Revision == "" {
		t.Fatalf("probe = %+v", p)
	}
	// A change the Hub dispatched earlier, delivered after the probe, never applies.
	if _, err := e.share(ops.OpShareSet, operateActions, setSeq("none", 49)); err == nil {
		t.Fatal("a change older than the probe's barrier was applied")
	}
	if sel := e.saved(); sel.Mode != store.ShareAll {
		t.Fatalf("rule after the refused change = %+v", sel)
	}
	// The probe never lowers the barrier, and echoes it with share_get too.
	if p := e.probe(5); p.OpBarrier != "50" {
		t.Fatalf("a lower probe moved the barrier to %s", p.OpBarrier)
	}
	if res := e.mustShare(ops.OpShareGet, ops.ShareGetArgs{WorkerID: shareWorkerID, WithRevision: true}); res.OpBarrier != "50" {
		t.Fatalf("share_get barrier = %q", res.OpBarrier)
	}
}

// A change with no sequence number (the local socket, an older Hub) is not
// ordered by the barrier, and the probe reads what a local change has just
// committed.
func TestUnnumberedChangesAndLocalChangesAreSeenByTheProbe(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareAll}, "a", "b")
	e.probe(100)
	if _, err := e.share(ops.OpShareSet, operateActions, shareSetArgs("none")); err != nil {
		t.Fatalf("an unnumbered change was refused: %v", err)
	}
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"a"}}); err != nil { // the owner's local change
		t.Fatal(err)
	}
	p := e.probe(0)
	if p.Rule.Mode != "select" || len(p.Rule.Names) != 1 || p.Rule.Names[0] != "a" {
		t.Fatalf("the probe did not see the local change that had returned: %+v", p)
	}
	if n, _ := strconv.ParseUint(p.Revision, 10, 64); n < 2 {
		t.Fatalf("revision = %q", p.Revision)
	}
}

// The probe frame on an op stream is served without any action and answers the
// rule and the barrier only; an executor that cannot answer refuses it.
func TestTheOpServerAnswersTheHubsShareProbe(t *testing.T) {
	e := shareEnv(t, store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"a"}}, "a", "b")
	srv, err := NewOpServer(OpServerConfig{Executor: e.ex})
	if err != nil {
		t.Fatal(err)
	}
	st := newFakeOpStream(t, ops.ShareProbeRequest{Type: proto.TypeShareProbe, OpBarrier: 7})
	srv.ServeStream(context.Background(), st)
	if len(st.frames) != 1 {
		t.Fatalf("frames = %d", len(st.frames))
	}
	var f struct {
		Type   string          `json:"type"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(st.frames[0], &f); err != nil || f.Type != proto.TypeOpResult {
		t.Fatalf("answer = %s", st.frames[0])
	}
	var p ops.ShareProbe
	if err := json.Unmarshal(f.Result, &p); err != nil || p.OpBarrier != "7" || p.Rule.Mode != "select" {
		t.Fatalf("probe = %+v (%v)", p, err)
	}
	// An executor that is not a ShareProber refuses it.
	srv2, _ := NewOpServer(OpServerConfig{Executor: plainExecutor{e.ex}})
	st2 := newFakeOpStream(t, ops.ShareProbeRequest{Type: proto.TypeShareProbe})
	srv2.ServeStream(context.Background(), st2)
	if typ, _ := proto.FrameType(st2.frames[0]); typ != proto.TypeError {
		t.Fatalf("a non-prober answered %s", st2.frames[0])
	}
}

type plainExecutor struct{ inner *Executor }

func (p plainExecutor) Execute(ctx context.Context, rq Request) (json.RawMessage, error) {
	return p.inner.Execute(ctx, rq)
}
