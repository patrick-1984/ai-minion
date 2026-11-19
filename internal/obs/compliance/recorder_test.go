package compliance

import (
	"context"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

type sink struct {
	mu  sync.Mutex
	rec []Record
}

func (s *sink) Emit(r Record) {
	s.mu.Lock()
	s.rec = append(s.rec, r)
	s.mu.Unlock()
}

func attr(r Record, k string) any {
	for _, a := range r.Attrs {
		if a.Key == k {
			return a.Value
		}
	}
	return nil
}

func mon(p Policy, managed bool, digest bool) (*Monitor, *sink) {
	m := NewMonitor(Deps{
		UserManaged:      func(context.Context, string) (bool, error) { return managed, nil },
		WorkerHasFeature: func(_, f string) bool { return digest && f == proto.FeatureArgsDigest },
	})
	s := &sink{}
	m.SetSink(s)
	m.SetPolicy(p)
	return m, s
}

func info(op string, args any) CallInfo {
	return CallInfo{Path: PathREST, Op: op, Args: args, Client: ClientAPI, PrincipalKind: "api_key", Actor: "key:k1", KeyID: "k1"}
}

func tgt() ops.Target { return ops.Target{WorkerID: "w1", SessionName: "s1"} }

func TestInputKindsAndMasking(t *testing.T) {
	key := "mn_" + strings.Repeat("c", 52)
	rid := ops.NewUUIDv7()
	wa := ops.WriteArgs{RequestID: rid}
	cases := []struct {
		op   string
		args any
		kind string
		body string
	}{
		{ops.OpSendText, &ops.SendTextArgs{WriteArgs: wa, Target: tgt(), Text: "hello " + key}, "text", "hello mn_[redacted]"},
		{ops.OpSendKeys, &ops.SendKeysArgs{WriteArgs: wa, Target: tgt(), Keys: []string{"C-c", "Enter"}}, "keys", "C-c Enter"},
		{ops.OpAnswer, &ops.AnswerArgs{WriteArgs: wa, Target: tgt(), Choice: "yes"}, "choice", "yes"},
		{ops.OpSetTask, &ops.SetTaskArgs{WriteArgs: wa, Target: tgt(), Task: "fix MN-ABCD-EFGH"}, "task", "fix MN-[redacted]"},
		{ops.OpPipeSend, &ops.PipeSendArgs{WriteArgs: wa, Target: tgt(), Text: "msg"}, "message", "msg"},
		{ops.OpSessionCreate, &ops.SessionCreateArgs{WriteArgs: wa, Cwd: "/srv", Argv: []string{"claude", "--k", key}}, "create", `{"cwd":"/srv","argv":["claude","--k","mn_[redacted]"]}`},
	}
	for _, c := range cases {
		t.Run(c.op, func(t *testing.T) {
			m, s := mon(Policy{Version: 2, Level: LevelActions, Input: true}, true, true)
			ci := info(c.op, c.args)
			ci.WorkerID = "w1" // session_create names its Worker in the request, not the arguments
			capt := m.Begin(context.Background(), ci)
			capt.Attempted()
			capt.Done(ops.WriteResult{Outcome: ops.OutcomeDelivered}, "")
			if len(s.rec) != 2 {
				t.Fatalf("records = %d", len(s.rec))
			}
			in := s.rec[0]
			if in.Event != EventInput || attr(in, "minion.input.kind") != c.kind || in.Body != c.body || attr(in, "minion.input.status") != "attempted" {
				t.Fatalf("input = %+v", in)
			}
			if strings.Contains(in.Body, strings.Repeat("c", 52)) {
				t.Fatal("a Minion key left in the clear")
			}
			if attr(s.rec[1], "minion.request.id") != rid || attr(in, "minion.request.id") != rid {
				t.Fatal("request id does not join the records")
			}
		})
	}
}

func TestInputOffAndActionsLevelWriteNoContent(t *testing.T) {
	args := &ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: tgt(), Text: "secret plans"}
	for _, p := range []Policy{{Version: 1, Level: LevelActions}, {Version: 1, Level: LevelOutput}} {
		m, s := mon(p, true, true)
		c := m.Begin(context.Background(), info(ops.OpSendText, args))
		c.Attempted()
		c.Done(ops.WriteResult{Outcome: ops.OutcomeDelivered}, "")
		for _, r := range s.rec {
			if r.Event == EventInput || r.Body != "" {
				t.Fatalf("level %s without compliance_input wrote content: %+v", p.Level, r)
			}
		}
	}
}

func TestReplayedFlagAndOutcomes(t *testing.T) {
	args := &ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: tgt(), Text: "x"}
	cases := []struct {
		name     string
		digest   bool
		res      any
		code     string
		outcome  string
		replayed any
	}{
		{"delivered", true, ops.WriteResult{Outcome: ops.OutcomeDelivered}, "", "delivered", "false"},
		{"replayed", true, ops.WriteResult{Outcome: ops.OutcomeDelivered, Replayed: true}, "", "delivered", "true"},
		{"old Worker: replayed unknown", false, ops.WriteResult{Outcome: ops.OutcomeDelivered}, "", "delivered", "unknown"},
		{"partial", true, ops.WriteResult{Outcome: ops.OutcomePartial, Reason: "internal"}, "", "partial", "false"},
		{"failed_not_sent", true, ops.WriteResult{Outcome: ops.OutcomeFailedNotSent, Reason: "busy"}, "", "failed_not_sent", "false"},
		{"unknown", true, ops.WriteResult{Outcome: ops.OutcomeUnknown, Reason: "connection_lost"}, "", "unknown", "false"},
		{"refused", true, nil, "conflict", "refused:conflict", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, s := mon(Policy{Version: 1, Level: LevelActions}, true, c.digest)
			m.Begin(context.Background(), info(ops.OpSendText, args)).Done(c.res, c.code)
			got := s.rec[0]
			if attr(got, "minion.outcome") != c.outcome || attr(got, "minion.replayed") != c.replayed {
				t.Fatalf("outcome %v replayed %v", attr(got, "minion.outcome"), attr(got, "minion.replayed"))
			}
		})
	}
}

func TestBodyCapAndRuneBoundary(t *testing.T) {
	text := strings.Repeat("é", MaxBody) // 2 bytes each: cut inside a rune is possible
	m, s := mon(Policy{Version: 1, Level: LevelActions, Input: true}, true, true)
	args := &ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: tgt(), Text: text}
	m.Begin(context.Background(), info(ops.OpSendText, args)).Attempted()
	in := s.rec[0]
	if len(in.Body) > MaxBody || !utf8.ValidString(in.Body) {
		t.Fatalf("body %d bytes, valid=%v", len(in.Body), utf8.ValidString(in.Body))
	}
	if attr(in, "minion.content.truncated_bytes") != int64(len(text)-len(in.Body)) {
		t.Fatalf("truncated = %v", attr(in, "minion.content.truncated_bytes"))
	}
}

func TestAttributesAreCapped(t *testing.T) {
	long := strings.Repeat("x", 5000)
	in := capAttrs([]Attr{{"a", long}, {"b", "keep"}})
	if len(in[0].Value.(string)) > maxAttrString {
		t.Fatal("string attribute not capped")
	}
	many := make([]Attr, 0, 100)
	for i := 0; i < 100; i++ {
		many = append(many, Attr{"k", strings.Repeat("y", 1000)})
	}
	total := 0
	for _, a := range capAttrs(many) {
		total += len(a.Key) + attrSize(a.Value)
	}
	if total > MaxAttrBytes {
		t.Fatalf("attributes %d bytes", total)
	}
}

func TestScreenDedupeAndFingerprintScope(t *testing.T) {
	m, s := mon(Policy{Version: 1, Level: LevelOutput}, true, true)
	a := ops.SessionSnapshotResult{Pane: ops.PaneInfo{ID: "%1"}, Screen: ops.Screen{Width: 80, Height: 1, Lines: []string{"one"}}}
	b := ops.SessionSnapshotResult{Pane: ops.PaneInfo{ID: "%1"}, Screen: ops.Screen{Width: 80, Height: 1, Lines: []string{"two"}}}
	read := func(r ops.SessionSnapshotResult) {
		m.Begin(context.Background(), info(ops.OpSessionSnapshot, &ops.SessionSnapshotArgs{Target: tgt()})).Done(r, "")
	}
	read(a)
	read(a)
	read(b)
	read(a) // back to the first text: it differs from the last emitted, so it is emitted again
	var bodies []string
	for _, r := range s.rec {
		if r.Event == EventOutput {
			bodies = append(bodies, r.Body)
			if attr(r, "minion.content.sha256") == "" {
				t.Fatal("no fingerprint")
			}
		}
	}
	if strings.Join(bodies, "|") != "one||two|one" {
		t.Fatalf("bodies = %q", bodies)
	}
	// Two captures of different ranges under one pane are both emitted.
	m2, s2 := mon(Policy{Version: 1, Level: LevelOutput}, true, true)
	for _, lines := range [][]string{{"a", "b"}, {"b", "c"}} {
		m2.Begin(context.Background(), info(ops.OpSessionCapture, &ops.SessionCaptureArgs{Target: tgt(), Lines: 2})).
			Done(ops.SessionCaptureResult{Pane: ops.PaneInfo{ID: "%1"}, Screen: ops.Screen{Lines: lines}}, "")
	}
	n := 0
	for _, r := range s2.rec {
		if r.Event == EventOutput && r.Body != "" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("captures with a body = %d, want 2", n)
	}
}

func TestPipeTurnTextOncePerTurnID(t *testing.T) {
	m, s := mon(Policy{Version: 1, Level: LevelOutput}, true, true)
	t1 := ops.PipeTurn{TurnID: "t1", Session: "s1", Direction: ops.PipeReceived, Text: "hello"}
	t2 := ops.PipeTurn{TurnID: "t2", Session: "s1", Direction: ops.PipeSent, Text: "world"}
	for i := 0; i < 2; i++ {
		m.Begin(context.Background(), info(ops.OpPipeRead, &ops.PipeReadArgs{Target: tgt()})).
			Done(ops.PipeReadResult{Turns: []ops.PipeTurn{t1, t2}, Withheld: 1}, "")
	}
	var bodies []string
	actions := 0
	for _, r := range s.rec {
		switch r.Event {
		case EventOutput:
			bodies = append(bodies, r.Body)
		case EventAction:
			actions++
			if ids, _ := attr(r, "minion.turn.ids").([]string); len(ids) != 2 || attr(r, "minion.pipe.withheld") != int64(1) {
				t.Fatalf("a read must reference its turn ids: %+v", r.Attrs)
			}
		}
	}
	if strings.Join(bodies, "|") != "hello|world" || actions != 2 {
		t.Fatalf("bodies %q actions %d", bodies, actions)
	}
}

func TestNotUserManagedAndUnknownWorkerWriteNoContent(t *testing.T) {
	for _, managed := range []bool{false} {
		m, s := mon(Policy{Version: 1, Level: LevelOutput, Input: true}, managed, true)
		c := m.Begin(context.Background(), info(ops.OpSessionSnapshot, &ops.SessionSnapshotArgs{Target: tgt()}))
		c.Done(ops.SessionSnapshotResult{Screen: ops.Screen{Lines: []string{"x"}}}, "")
		for _, r := range s.rec {
			if r.Event == EventOutput || r.Body != "" {
				t.Fatalf("content for a Worker that is not user-managed: %+v", r)
			}
		}
		if len(s.rec) != 1 {
			t.Fatalf("records = %d", len(s.rec))
		}
	}
	// An op without a known Worker (no target, no worker id) cannot be judged: no content.
	m, s := mon(Policy{Version: 1, Level: LevelOutput, Input: true}, true, true)
	c := m.Begin(context.Background(), info(ops.OpSendText, &ops.SendTextArgs{}))
	c.Attempted()
	if len(s.rec) != 0 {
		t.Fatal("content without a Worker")
	}
}

func TestPathMatrixSkipsSessionCreateOnCallStreams(t *testing.T) {
	m, _ := mon(Policy{Version: 1, Level: LevelActions}, true, true)
	ci := info(ops.OpSessionCreate, &ops.SessionCreateArgs{})
	ci.Path = PathCall
	if m.Begin(context.Background(), ci) != nil {
		t.Fatal("session_create is captured on REST only")
	}
}

func TestMonitorWithoutSinkCountsAndOffIsFree(t *testing.T) {
	m := NewMonitor(Deps{})
	if m.Begin(context.Background(), info(ops.OpSendText, &ops.SendTextArgs{})) != nil {
		t.Fatal("off policy must record nothing")
	}
	m.SetPolicy(Policy{Version: 1, Level: LevelActions})
	m.Begin(context.Background(), info(ops.OpSessionsList, nil)).Done(ops.SessionsListResult{}, "")
	if m.NoSink() != 1 {
		t.Fatalf("noSink = %d", m.NoSink())
	}
	var nilCapture *Capture
	nilCapture.Attempted()
	nilCapture.Done(nil, "")
	nilCapture.Denied()
}

func TestSubscribersSeeEveryPolicyChange(t *testing.T) {
	m := NewMonitor(Deps{})
	var got []int64
	cancel := m.Subscribe(func(p Policy) { got = append(got, p.Version) })
	m.SetPolicy(Policy{Version: 1, Level: LevelActions})
	m.SetPolicy(Policy{Version: 2, Level: LevelOutput})
	cancel()
	m.SetPolicy(Policy{Version: 3, Level: LevelOff})
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("got %v", got)
	}
}
