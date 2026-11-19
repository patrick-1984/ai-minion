package compliance

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// dropSink drops every record while drop is set.
type dropSink struct {
	mu   sync.Mutex
	rec  []Record
	drop bool
}

func (s *dropSink) Emit(r Record) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drop {
		return false
	}
	s.rec = append(s.rec, r)
	return true
}

// A call admitted under one generation records only into that generation's
// sink: after a retarget its late records are dropped and counted, never sent
// to the replacement (A49 review: wrong collector on retarget).
func TestCallKeepsItsGenerationAcrossARetarget(t *testing.T) {
	m := NewMonitor(Deps{UserManaged: func(context.Context, string) (bool, error) { return true, nil }})
	oldSink, newSink := &sink{}, &sink{}
	m.Install(Policy{Version: 1, Level: LevelOutput, Input: true}, oldSink)
	capt := m.Begin(context.Background(), info(ops.OpSendText, &ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: tgt(), Text: "secret"}))
	// The admin retargets and enables more while the call is in flight.
	m.Install(Policy{Version: 2, Level: LevelOutput, Input: true, DestID: "other"}, newSink)
	capt.Attempted()
	capt.Done(ops.WriteResult{Outcome: ops.OutcomeDelivered}, "")
	if len(newSink.rec) != 0 {
		t.Fatalf("a call admitted under the old policy reached the new collector: %+v", newSink.rec)
	}
	if m.NoSink() != 2 {
		t.Fatalf("late records must be counted, noSink = %d", m.NoSink())
	}
	// A call admitted after the swap records into the new sink only, with the new version.
	m.Begin(context.Background(), info(ops.OpSessionsList, nil)).Done(ops.SessionsListResult{}, "")
	if len(newSink.rec) != 1 || attr(newSink.rec[0], "minion.policy.version") != int64(2) || len(oldSink.rec) != 0 {
		t.Fatalf("new %d old %d", len(newSink.rec), len(oldSink.rec))
	}
}

// A body that the queue dropped is sent again by the next read: the dedupe
// remembers only what was accepted, and a new generation starts afresh.
func TestDroppedFirstBodyIsSentAgain(t *testing.T) {
	m := NewMonitor(Deps{UserManaged: func(context.Context, string) (bool, error) { return true, nil }})
	s := &dropSink{drop: true}
	m.Install(Policy{Version: 1, Level: LevelOutput}, s)
	read := func() {
		m.Begin(context.Background(), info(ops.OpSessionSnapshot, &ops.SessionSnapshotArgs{Target: tgt()})).
			Done(ops.SessionSnapshotResult{Pane: ops.PaneInfo{ID: "%1"}, Screen: ops.Screen{Lines: []string{"the screen"}}}, "")
	}
	read() // dropped
	s.mu.Lock()
	s.drop = false
	s.mu.Unlock()
	read() // accepted: must carry the body
	read() // now a reference
	var bodies []string
	for _, r := range s.rec {
		if r.Event == EventOutput {
			bodies = append(bodies, r.Body)
		}
	}
	if strings.Join(bodies, "|") != "the screen|" {
		t.Fatalf("bodies = %q", bodies)
	}
	// Turn text likewise.
	m2 := NewMonitor(Deps{UserManaged: func(context.Context, string) (bool, error) { return true, nil }})
	s2 := &dropSink{drop: true}
	m2.Install(Policy{Version: 1, Level: LevelOutput}, s2)
	turn := ops.PipeReadResult{Turns: []ops.PipeTurn{{TurnID: "t1", Session: "s", Text: "hello"}}}
	m2.Begin(context.Background(), info(ops.OpPipeRead, &ops.PipeReadArgs{Target: tgt()})).Done(turn, "")
	s2.mu.Lock()
	s2.drop = false
	s2.mu.Unlock()
	m2.Begin(context.Background(), info(ops.OpPipeRead, &ops.PipeReadArgs{Target: tgt()})).Done(turn, "")
	got := 0
	for _, r := range s2.rec {
		if r.Event == EventOutput && r.Body == "hello" {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("the turn text was lost after a dropped first emit: %d", got)
	}
}

// Known credentials are masked wherever they sit: decoded argv and cwd of
// session_create (also after a line break) and every string attribute.
func TestKnownCredentialsAreMaskedInCreateAndAttributes(t *testing.T) {
	key := "mn_" + strings.Repeat("z", 52)
	m, s := mon(Policy{Version: 1, Level: LevelActions, Input: true}, true, true)
	args := &ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Name: "sess-" + key, Cwd: "/srv\n" + key,
		Argv: []string{"sh", "-c", "echo hi\n" + key + "\n" + "mnj1-" + strings.Repeat("Q9_", 12)}}
	ci := info(ops.OpSessionCreate, args)
	ci.WorkerID = "w1"
	capt := m.Begin(context.Background(), ci)
	capt.Attempted()
	capt.Done(ops.WriteResult{Outcome: ops.OutcomeDelivered, Pane: &ops.PaneInfo{Session: "sess-" + key}}, "")
	for _, r := range s.rec {
		all := r.Body
		for _, a := range r.Attrs {
			if v, ok := a.Value.(string); ok {
				all += " " + v
			}
		}
		if strings.Contains(all, strings.Repeat("z", 52)) || strings.Contains(all, strings.Repeat("Q9_", 12)) {
			t.Fatalf("a credential left in %s: %s", r.Event, all)
		}
	}
	// A Minion key in a session name at the actions level with input off.
	m2, s2 := mon(Policy{Version: 1, Level: LevelActions}, true, true)
	a2 := &ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: ops.Target{WorkerID: "w1", SessionName: "s-" + key}, Text: "x"}
	m2.Begin(context.Background(), info(ops.OpSendText, a2)).Done(ops.WriteResult{Outcome: ops.OutcomeDelivered}, "")
	for _, a := range s2.rec[0].Attrs {
		if v, ok := a.Value.(string); ok && strings.Contains(v, strings.Repeat("z", 52)) {
			t.Fatalf("attribute %s carries the key", a.Key)
		}
	}
}

// A cut body is a copy: it does not keep the original allocation alive.
func TestTruncatedBodyIsACopy(t *testing.T) {
	big := strings.Repeat("a", 1<<20)
	m, s := mon(Policy{Version: 1, Level: LevelActions, Input: true}, true, true)
	args := &ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: tgt(), Text: big}
	m.Begin(context.Background(), info(ops.OpSendText, args)).Attempted()
	body := s.rec[0].Body
	if len(body) != MaxBody {
		t.Fatalf("len = %d", len(body))
	}
	// Strings.Clone allocates exactly; a slice of the big string would share its pointer.
	if cap([]byte(body)) > 2*MaxBody {
		t.Fatal("unexpected capacity")
	}
	if strings.Count(body, "a") != MaxBody {
		t.Fatal("content changed")
	}
}

func TestWorkerRetireIsRecordedAsAnAction(t *testing.T) {
	m, s := mon(Policy{Version: 1, Level: LevelActions}, true, true)
	m.RecordAction(CallInfo{Path: PathCall, Op: ops.OpWorkerRetire, Client: ClientCallStream, PrincipalKind: "worker", Actor: "worker:w1", WorkerID: "w1"}, "delivered")
	if len(s.rec) != 1 || s.rec[0].Event != EventAction || attr(s.rec[0], "minion.outcome") != "delivered" || attr(s.rec[0], "minion.op") != ops.OpWorkerRetire {
		t.Fatalf("records = %+v", s.rec)
	}
	m.Install(Policy{Version: 2, Level: LevelOff}, s)
	m.RecordAction(CallInfo{Path: PathCall, Op: ops.OpWorkerRetire, Actor: "worker:w1"}, "delivered")
	if len(s.rec) != 1 {
		t.Fatal("recorded with monitoring off")
	}
}

func TestPolicySameSeesTheFullDestination(t *testing.T) {
	a := Policy{Level: LevelActions, Destination: "https://c:443", DestID: "tenant-a"}
	b := a
	b.DestID = "tenant-b"
	if a.Same(b) {
		t.Fatal("two paths on one host must differ")
	}
	c := a
	c.TrafficID = "scope-2"
	if a.Same(c) {
		t.Fatal("a change of the payload scope must differ")
	}
}
