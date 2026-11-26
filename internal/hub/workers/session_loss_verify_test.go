package workers

// Verification of "a lost session loses its permission" (owner question,
// v0.22.0). These tests pin what the code does today, including the windows
// in which a Claimed (name-only) permission still reaches a new terminal
// that took the name. A test named ...Gap asserts the current, unwanted
// behaviour so that a fix makes it fail and is noticed.

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// A Worker that was offline while its session was killed and a new one was
// created under the same name sends a snapshot that lists the name again.
// The Hub keeps the first-seen time of a name it listed before the
// disconnect, so a name-only grant made for the old terminal still passes
// the first-seen fence and reaches the new terminal.
func TestVerifyLossReconnectSnapshotKeepsTheOldTerminalsGrantGap(t *testing.T) {
	var clock sync.Mutex
	nowMS := int64(10_000)
	setNow := func(ms int64) { clock.Lock(); nowMS = ms; clock.Unlock() }
	hub := NewEventHub(EventHubOptions{Now: func() time.Time { clock.Lock(); defer clock.Unlock(); return time.UnixMilli(nowMS) }})
	links := map[string]*Link{}
	connect := func(id, name string) {
		l := hub.Connect(id, id)
		f, err := l.NewFeed()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Snapshot(0, []SessionState{{Name: name, Kind: "agent:claude"}}); err != nil {
			t.Fatal(err)
		}
		links[id] = l
	}
	connect(wAlicePi1.ID, "agent")
	connect(wAlicePi2.ID, "build")
	// A second observation of both, so that the names have a real first-seen
	// time (not the 0 of the first snapshot of the process).
	setNow(10_500)
	for id, l := range links {
		l.Disconnect()
		_ = id
	}
	setNow(10_600)
	f1, _ := hub.Connect(wAlicePi1.ID, wAlicePi1.ID).NewFeed()
	_ = f1.Snapshot(0, nil)
	f2, _ := hub.Connect(wAlicePi2.ID, wAlicePi2.ID).NewFeed()
	_ = f2.Snapshot(0, nil)
	setNow(10_700)
	f1, _ = hub.Connect(wAlicePi1.ID, wAlicePi1.ID).NewFeed()
	if err := f1.Snapshot(0, []SessionState{{Name: "agent", Kind: "agent:claude"}}); err != nil {
		t.Fatal(err)
	}
	l2 := hub.Connect(wAlicePi2.ID, wAlicePi2.ID)
	f2, _ = l2.NewFeed()
	if err := f2.Snapshot(0, []SessionState{{Name: "build", Kind: "agent:claude"}}); err != nil {
		t.Fatal(err)
	}
	first, _ := hub.FirstSeen(wAlicePi2.ID, "build")
	if first != 10_700 {
		t.Fatalf("setup: build first seen %d", first)
	}

	r := newSCRig(t)
	r.cs.cfg.Sessions = hub
	g := aliceGrant(authz.LevelControl)
	g.CreatedAt = 11_000 // made for the terminal "build" as it is now
	r.grants.setDirect(wAlicePi1.ID, "agent", g)
	send := func() []json.RawMessage {
		frames, _ := r.call(t, wAlicePi1.ID, scRequest(ops.OpSendText, scArgs(ops.OpSendText, grantedBuild), "", "agent"),
			ops.WriteResult{Outcome: ops.OutcomeDelivered})
		return frames
	}
	var wr ops.WriteResult
	resultOf(t, send(), &wr)

	// Pi-2 goes offline; while it is, "build" is killed and a NEW tmux
	// session "build" is created (or the tmux server restarts and a startup
	// script recreates it). The Worker reconnects and lists "build" again.
	setNow(12_000)
	l2.Disconnect()
	setNow(20_000)
	f2, _ = hub.Connect(wAlicePi2.ID, wAlicePi2.ID).NewFeed()
	if err := f2.Snapshot(0, []SessionState{{Name: "build", Kind: "agent:claude"}}); err != nil {
		t.Fatal(err)
	}
	if at, _ := hub.FirstSeen(wAlicePi2.ID, "build"); at != 10_700 {
		t.Fatalf("first seen after the reconnect = %d", at)
	}
	// GAP: the grant made for the old terminal reaches the new one.
	resultOf(t, send(), &wr)
}

// A Claimed (name-only) master nomination has no first-seen fence at call
// time: the source session ends and a new terminal takes its name; until the
// session purge module (server/mod_session_purge.go) has committed the purge
// of the nomination, a call from the new terminal is authorized by the old
// nomination. (A direct grant in the same situation is refused at once:
// TestObservedRemovalFencesOlderNameOnlyGrantsBeforeThePurge.)
func TestVerifyLossClaimedMasterHasNoCallTimeFenceBeforeThePurgeGap(t *testing.T) {
	hub := NewEventHub(EventHubOptions{Now: func() time.Time { return time.UnixMilli(50_000) }})
	f, err := hub.Connect(wRootLab.ID, wRootLab.ID).NewFeed()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Snapshot(0, []SessionState{{Name: "boss", Kind: "agent:claude"}}); err != nil {
		t.Fatal(err)
	}
	r := newSCRig(t)
	r.cs.cfg.Sessions = hub
	r.grants.set(t, wRootLab.ID, "boss", gAlicePi2) // the name-only master row
	args := scArgs(ops.OpSessionSnapshot, ops.Target{WorkerID: wAlicePi2.ID, SessionName: "x"})
	call := func() []json.RawMessage {
		frames, _ := r.call(t, wRootLab.ID, scRequest(ops.OpSessionSnapshot, args, "", "boss"), map[string]any{})
		return frames
	}
	var snap ops.SessionSnapshotResult
	resultOf(t, call(), &snap)
	// "boss" ends and a new terminal takes the name; no purge has run.
	if err := f.Apply(Change{Seq: 1, Event: EventSessionRemoved, Session: SessionState{Name: "boss"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Apply(Change{Seq: 2, Event: EventSessionAdded, Session: SessionState{Name: "boss", Kind: "agent:claude"}}); err != nil {
		t.Fatal(err)
	}
	frames := call()
	for _, fr := range frames {
		var e struct {
			Error *proto.Error `json:"error"`
		}
		if json.Unmarshal(fr, &e) == nil && e.Error != nil {
			t.Fatalf("now fenced at call time (gap fixed?): %+v", e.Error)
		}
	}
	resultOf(t, frames, &snap) // GAP: the old nomination serves the new terminal
}

// A session-bound (Verified) master nomination belongs to one incarnation
// (a random id the Worker makes per prepared harness process). When the
// terminal ends and a new terminal under the same name is prepared, the lane
// reports a new incarnation: the old lease is dropped at once, a call with
// the old lease is refused, the new incarnation gets no lease, and the
// stored row shows "retired" (not active) while the old incarnation is not
// reported. When the terminal is not reported at all, no lease remains.
func TestVerifyLossBoundMasterDoesNotPassToANewIncarnationOfTheName(t *testing.T) {
	r := newMasterRig(t)
	m := r.master
	old := r.call(ops.OpSendText)
	m.mu.Lock()
	if _, _, err := m.authorizeLocked(old); err != nil {
		m.mu.Unlock()
		t.Fatalf("setup: the bound master is not authorized: %v", err)
	}
	m.mu.Unlock()

	report := func(seq uint64, c ...proto.MasterCandidate) {
		t.Helper()
		if c == nil {
			c = []proto.MasterCandidate{}
		}
		b, _ := json.Marshal(proto.MasterReport{Type: proto.TypeMasterReport, Seq: seq, Candidates: c})
		if !m.applyReport(r.lane, b, false) {
			t.Fatal("report refused")
		}
	}
	// Same name, new terminal, new incarnation.
	report(2, proto.MasterCandidate{SessionName: "prepared", IncarnationID: "second", Adapter: proto.MasterAdapterClaude})
	if len(r.lane.leases) != 0 {
		t.Fatalf("a new incarnation of the name holds a lease: %+v", r.lane.leases)
	}
	m.mu.Lock()
	_, _, err := m.authorizeLocked(old)
	m.mu.Unlock()
	if err == nil {
		t.Fatal("the old lease still authorizes after its terminal was replaced")
	}
	forged := old
	forged.Master = &proto.MasterCallArgs{Generation: old.Master.Generation, IncarnationID: "second", LeaseID: old.Master.LeaseID,
		Operation: ops.OpSendText, Arguments: json.RawMessage(`{}`)}
	m.mu.Lock()
	_, _, err = m.authorizeLocked(forged)
	m.mu.Unlock()
	if err == nil {
		t.Fatal("the new incarnation is authorized by the old nomination")
	}
	view, err := m.MasterView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b := view[r.source.ID].Bound
	if len(b) != 1 || b[0].Attachment != "retired" || b[0].IncarnationID != "first" {
		t.Fatalf("bound view = %+v, want the old incarnation retired", b)
	}
	if c := view[r.source.ID].Candidates; len(c) != 1 || c[0].Master || c[0].Attachment != "ready" {
		t.Fatalf("the new terminal is shown as master: %+v", c)
	}
	// The terminal is gone (nothing reported): no lease, nothing live.
	report(3)
	if len(r.lane.leases) != 0 || len(m.LiveLeases()) != 0 {
		t.Fatalf("leases after the terminal ended: %+v %+v", r.lane.leases, m.LiveLeases())
	}
}
