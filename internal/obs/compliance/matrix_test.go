package compliance

import (
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// The matrix fails closed: a Worker-scope op of the catalog without a row
// fails this test, so a new op cannot ship without a decision about what the
// compliance record holds for it.
func TestEveryWorkerOpHasAMatrixRow(t *testing.T) {
	seen := map[string]bool{}
	for _, op := range ops.Default.All() {
		if op.Scope != ops.ScopeWorker {
			continue
		}
		seen[op.Name] = true
		if _, ok := Matrix[op.Name]; !ok {
			t.Errorf("Worker op %q has no row in the compliance matrix (internal/obs/compliance/matrix.go): decide what its record holds at each level", op.Name)
		}
	}
	for name := range Matrix {
		if !seen[name] {
			t.Errorf("the matrix has a row for %q, which is not a Worker op of the catalog", name)
		}
	}
}

// Every op that puts content into a session has an input class; every op that
// returns session text has an output class.
func TestMatrixClassesOfContentOps(t *testing.T) {
	in := map[string]InputKind{
		ops.OpSendText: InText, ops.OpSendKeys: InKeys, ops.OpAnswer: InChoice, ops.OpSetTask: InTask,
		ops.OpPipeSend: InMessage, ops.OpSessionCreate: InCreate,
	}
	for op, k := range in {
		if Matrix[op].Input != k {
			t.Errorf("%s input = %v, want %v", op, Matrix[op].Input, k)
		}
	}
	out := map[string]OutputKind{
		ops.OpSessionSnapshot: OutScreen, ops.OpSessionCapture: OutCapture, ops.OpPipeRead: OutTurns, ops.OpPipeWait: OutTurns,
	}
	for op, k := range out {
		if Matrix[op].Output != k {
			t.Errorf("%s output = %v, want %v", op, Matrix[op].Output, k)
		}
	}
	// Nothing else carries content: the worker_logs lines are logs, not session content.
	for op, r := range Matrix {
		if _, ok := in[op]; ok {
			continue
		}
		if _, ok := out[op]; ok {
			continue
		}
		if r.Input != InNone || r.Output != OutNone {
			t.Errorf("%s carries content but is not one of the content ops", op)
		}
	}
	// session_create is captured on REST only (design §5.3).
	if Matrix[ops.OpSessionCreate].Paths != PathREST {
		t.Error("session_create is not on the call stream")
	}
}

func TestUnknownOpIsActionsOnly(t *testing.T) {
	r := RowOf("a_new_op")
	if r.Input != InNone || r.Output != OutNone || r.Paths != both || ContentBearing("a_new_op") {
		t.Fatalf("row = %+v", r)
	}
}

func TestPolicyTextsAndStrictness(t *testing.T) {
	p := Policy{Version: 7, Level: LevelOutput, Input: true, TrafficPayload: true, Destination: "https://c.example:443"}
	if h := p.Header(); h != "v=7; level=output; input=on; traffic=payload" {
		t.Fatal(h)
	}
	if n := p.Notice(); !strings.Contains(n, "output, input on") || !strings.Contains(n, "v7") || !strings.Contains(n, "frame contents") {
		t.Fatal(n)
	}
	if (Policy{Level: LevelOff}).Notice() != "" || (Policy{Level: LevelOff}).Header() != "v=0; level=off; input=off" {
		t.Fatal("off policy")
	}
	if tg := p.AuditTarget(); tg != "v=7 level=output input=on traffic=payload dest=https://c.example:443" {
		t.Fatal(tg)
	}
	long := Policy{Version: 1, Level: LevelActions, Destination: strings.Repeat("h", 400)}
	if len(long.AuditTarget()) > 256 {
		t.Fatal("audit target is longer than 256")
	}
	cases := []struct {
		cur, seen Policy
		want      bool
	}{
		{Policy{Level: "actions"}, Policy{Level: "off"}, true},
		{Policy{Level: "output"}, Policy{Level: "actions"}, true},
		{Policy{Level: "actions", Input: true}, Policy{Level: "actions"}, true},
		{Policy{Level: "off", TrafficPayload: true}, Policy{Level: "off"}, true},
		{Policy{Level: "actions"}, Policy{Level: "output", Input: true}, false},
		{Policy{Level: "off"}, Policy{Level: "output"}, false},
		{Policy{Level: "actions"}, Policy{Level: "actions"}, false},
	}
	for i, c := range cases {
		if got := c.cur.StricterThan(c.seen); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}
