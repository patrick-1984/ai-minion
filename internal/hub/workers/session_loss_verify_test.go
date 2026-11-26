package workers

// Verification of "a lost session loses its permission" (owner question,
// v0.22.0). These tests pin what the code does today, including the windows
// in which a Claimed (name-only) permission could reach a new terminal that
// took the name; since v0.23.0 (A115) they assert that it does not.

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// A Worker that was offline while its session was killed and a new one was
// created under the same name sends a snapshot that lists the name again.
// With the terminal's instance the Hub tells the new terminal apart: the name
// is first seen at the reconnect and a name-only grant made for the old
// terminal does not reach the new one. The same instance keeps the first-seen
// time and the grant. A Worker that reports no instance (older Workers,
// psmux) keeps the name-only rule: the name listed again keeps its first-seen
// time (documented, docs/guides/access.md).
func TestVerifyLossReconnectSnapshotWithANewInstanceLosesTheOldTerminalsGrant(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after string
		newTerminal   bool
	}{
		{"new instance", "b1", "b2", true},
		{"same instance", "b1", "b1", false},
		{"no instance (older Worker)", "", "", false},
		{"instance becomes known", "", "b2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clock sync.Mutex
			nowMS := int64(10_000)
			setNow := func(ms int64) { clock.Lock(); nowMS = ms; clock.Unlock() }
			hub := NewEventHub(EventHubOptions{Now: func() time.Time { clock.Lock(); defer clock.Unlock(); return time.UnixMilli(nowMS) }})
			snap := func(id string, s ...SessionState) *Link {
				t.Helper()
				l := hub.Connect(id, id)
				f, err := l.NewFeed()
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Snapshot(0, s); err != nil {
					t.Fatal(err)
				}
				return l
			}
			// A first snapshot, then the names appear: a real first-seen time
			// (not the 0 of the first snapshot of the process).
			snap(wAlicePi1.ID)
			snap(wAlicePi2.ID)
			setNow(10_700)
			snap(wAlicePi1.ID, SessionState{Name: "agent", Kind: "agent:claude", InstanceID: "a1"})
			l2 := snap(wAlicePi2.ID, SessionState{Name: "build", Kind: "agent:claude", InstanceID: tc.before})
			if first, _ := hub.FirstSeen(wAlicePi2.ID, "build"); first != 10_700 {
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
			// session "build" is created (or the tmux server restarts and a
			// startup script recreates it). The Worker reconnects.
			setNow(12_000)
			l2.Disconnect()
			setNow(20_000)
			snap(wAlicePi2.ID, SessionState{Name: "build", Kind: "agent:claude", InstanceID: tc.after})
			at, _ := hub.FirstSeen(wAlicePi2.ID, "build")
			if !tc.newTerminal {
				if at != 10_700 {
					t.Fatalf("first seen after the reconnect = %d, want the old 10700", at)
				}
				resultOf(t, send(), &wr)
				return
			}
			if at != 20_000 {
				t.Fatalf("first seen after the reconnect = %d, want 20000 (a new terminal)", at)
			}
			wantRefused(t, send())
		})
	}
}

// wantRefused fails unless the answer is an error frame.
func wantRefused(t *testing.T, frames []json.RawMessage) {
	t.Helper()
	typ, body := lastFrame(t, frames)
	if typ != proto.TypeError {
		t.Fatalf("answer = %s, want a refusal", body)
	}
}

// memInstances is an in-memory [InstanceStore].
type memInstances struct {
	mu   sync.Mutex
	rows map[string]map[string]string
}

func (m *memInstances) LoadSessionInstances(_ context.Context, workerID string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for n, id := range m.rows[workerID] {
		out[n] = id
	}
	return out, nil
}

func (m *memInstances) SaveSessionInstances(_ context.Context, workerID string, in map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]map[string]string{}
	}
	if m.rows[workerID] == nil {
		m.rows[workerID] = map[string]string{}
	}
	for n, id := range in {
		m.rows[workerID][n] = id
	}
	return nil
}

// After a Hub restart the first snapshot of a Worker is compared with the
// stored last instances: a name whose instance changed while the Hub was down
// is first seen now (not 0, which passes every grant); the same instance and a
// name without a stored or a reported instance keep the first-seen 0 of the
// name-only rule.
func TestVerifyLossHubRestartComparesTheStoredInstance(t *testing.T) {
	inst := &memInstances{}
	now := time.UnixMilli(10_000)
	clock := func() time.Time { return now }
	before := NewEventHub(EventHubOptions{Now: clock, Instances: inst})
	f, _ := before.Connect("wA", "a").NewFeed()
	if err := f.Snapshot(0, []SessionState{{Name: "boss", InstanceID: "i1"}, {Name: "same", InstanceID: "s1"},
		{Name: "plain"}}); err != nil {
		t.Fatal(err)
	}
	if got := inst.rows["wA"]; len(got) != 2 || got["boss"] != "i1" || got["same"] != "s1" {
		t.Fatalf("stored instances = %+v", got)
	}

	// The Hub restarts; "boss" was recreated meanwhile; "fresh" is new and
	// was never stored.
	now = now.Add(time.Hour)
	after := NewEventHub(EventHubOptions{Now: clock, Instances: inst})
	f, _ = after.Connect("wA", "a").NewFeed()
	if err := f.Snapshot(0, []SessionState{{Name: "boss", InstanceID: "i2"}, {Name: "same", InstanceID: "s1"},
		{Name: "plain"}, {Name: "fresh", InstanceID: "f1"}}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int64{"boss": now.UnixMilli(), "same": 0, "plain": 0, "fresh": 0} {
		if at, ok := after.FirstSeen("wA", name); !ok || at != want {
			t.Errorf("%s: first seen %d (%t), want %d", name, at, ok, want)
		}
	}
	if inst.rows["wA"]["boss"] != "i2" || inst.rows["wA"]["fresh"] != "f1" {
		t.Fatalf("stored instances after the restart = %+v", inst.rows["wA"])
	}
}

// A Worker that predates the instance-aware diff sends a state_changed for a
// name that a new terminal took within one poll: the instance in the event
// makes it a new generation of the name.
func TestVerifyLossEventWithAnotherInstanceIsANewTerminal(t *testing.T) {
	now := time.UnixMilli(10_000)
	hub := NewEventHub(EventHubOptions{Now: func() time.Time { return now }})
	f, _ := hub.Connect("wA", "a").NewFeed()
	if err := f.Snapshot(0, []SessionState{{Name: "boss", Kind: "shell", InstanceID: "i1"}}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := f.Apply(Change{Seq: 1, Event: EventStateChanged, HasTask: true, Session: SessionState{Name: "boss", Kind: "agent:claude", InstanceID: "i1"}}); err != nil {
		t.Fatal(err)
	}
	if at, _ := hub.FirstSeen("wA", "boss"); at != 0 {
		t.Fatalf("the same instance changed the first-seen time: %d", at)
	}
	now = now.Add(time.Second)
	if err := f.Apply(Change{Seq: 2, Event: EventStateChanged, HasTask: true, Session: SessionState{Name: "boss", Kind: "agent:claude", InstanceID: "i2"}}); err != nil {
		t.Fatal(err)
	}
	if at, _ := hub.FirstSeen("wA", "boss"); at != now.UnixMilli() {
		t.Fatalf("another instance: first seen %d, want %d", at, now.UnixMilli())
	}
	// The wire carries the instance.
	if err := feedFrame(f, json.RawMessage(`{"type":"event","seq":3,"event":"state_changed","session":"boss","kind":"agent:claude","task":"","instance_id":"i3"}`), nil); err != nil {
		t.Fatal(err)
	}
	if at, _ := hub.FirstSeen("wA", "boss"); at != now.UnixMilli() {
		t.Fatalf("first seen %d", at)
	}
	now = now.Add(time.Second)
	if err := feedFrame(f, json.RawMessage(`{"type":"event","seq":4,"event":"state_changed","session":"boss","kind":"shell","task":"","instance_id":"i4"}`), nil); err != nil {
		t.Fatal(err)
	}
	if at, _ := hub.FirstSeen("wA", "boss"); at != now.UnixMilli() {
		t.Fatalf("a decoded event with another instance: first seen %d, want %d", at, now.UnixMilli())
	}
}

// A Claimed (name-only) master nomination is fenced at call time like a
// direct grant: the source session ends and a new terminal takes its name;
// before the session purge module (server/mod_session_purge.go) has deleted
// the nomination, a call from the new terminal is refused.
func TestVerifyLossClaimedMasterIsFencedAtCallTime(t *testing.T) {
	now := time.UnixMilli(50_000)
	hub := NewEventHub(EventHubOptions{Now: func() time.Time { return now }})
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
	r.grants.setMeta(wRootLab.ID, "boss", 50_000)
	args := scArgs(ops.OpSessionSnapshot, ops.Target{WorkerID: wAlicePi2.ID, SessionName: "x"})
	call := func() []json.RawMessage {
		frames, _ := r.call(t, wRootLab.ID, scRequest(ops.OpSessionSnapshot, args, "", "boss"), map[string]any{})
		return frames
	}
	var snap ops.SessionSnapshotResult
	resultOf(t, call(), &snap)
	// "boss" ends and a new terminal takes the name; no purge has run.
	now = now.Add(time.Second)
	if err := f.Apply(Change{Seq: 1, Event: EventSessionRemoved, Session: SessionState{Name: "boss"}}); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, call()) // not listed
	if err := f.Apply(Change{Seq: 2, Event: EventSessionAdded, Session: SessionState{Name: "boss", Kind: "agent:claude"}}); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, call()) // first seen after the nomination
}

// A session target of a session-limited acceptance is fenced at call time:
// a target first seen after it was accepted (another terminal under the name)
// or not listed is left out of the grant; the other targets stay.
func TestVerifyLossAcceptedTargetSessionIsFencedAtCallTime(t *testing.T) {
	now := time.UnixMilli(50_000)
	hub := NewEventHub(EventHubOptions{Now: func() time.Time { return now }})
	for id, names := range map[string][]string{wRootLab.ID: {"boss"}, wBobPi1.ID: {"build"}} {
		f, err := hub.Connect(id, id).NewFeed()
		if err != nil {
			t.Fatal(err)
		}
		var s []SessionState
		for _, n := range names {
			s = append(s, SessionState{Name: n, Kind: "agent:claude"})
		}
		if err := f.Snapshot(0, s); err != nil {
			t.Fatal(err)
		}
	}
	r := newSCRig(t)
	r.cs.cfg.Sessions = hub
	r.grants.set(t, wRootLab.ID, "boss", gAlicePi2, gBobBuild)
	named := store.ControlSessionTarget{WorkerID: wBobPi1.ID, SessionName: "build", NamedAt: 40_000}
	r.grants.setMeta(wRootLab.ID, "boss", 40_000, named)
	grant := func() string {
		g, err := r.cs.lookupGrant(t.Context(), wRootLab.ID, "boss")
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	if g := grant(); !strings.Contains(g, `"build"`) || !strings.Contains(g, wAlicePi2.ID) {
		t.Fatalf("grant = %s, want both targets", g)
	}
	// "build" ends (a snapshot without it), then a new terminal takes the
	// name: first seen 50000, after it was accepted at 40000.
	f, _ := hub.Connect(wBobPi1.ID, wBobPi1.ID).NewFeed()
	if err := f.Snapshot(0, nil); err != nil {
		t.Fatal(err)
	}
	if g := grant(); strings.Contains(g, `"build"`) || !strings.Contains(g, wAlicePi2.ID) {
		t.Fatalf("grant = %s, want the unlisted target left out", g)
	}
	if err := f.Apply(Change{Seq: 1, Event: EventSessionAdded, Session: SessionState{Name: "build", Kind: "agent:claude"}}); err != nil {
		t.Fatal(err)
	}
	if g := grant(); strings.Contains(g, `"build"`) || !strings.Contains(g, wAlicePi2.ID) {
		t.Fatalf("grant = %s, want the new terminal under the name left out", g)
	}
	// Only a session target that is left: no grant at all.
	r.grants.set(t, wRootLab.ID, "boss", gBobBuild)
	if g := grant(); g != "" {
		t.Fatalf("grant = %s, want none", g)
	}
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
	// The terminal is gone (nothing reported): no lease, nothing live, and
	// the stored row of the old incarnation still shows "retired".
	report(3)
	if len(r.lane.leases) != 0 || len(m.LiveLeases()) != 0 {
		t.Fatalf("leases after the terminal ended: %+v %+v", r.lane.leases, m.LiveLeases())
	}
	if view, err = m.MasterView(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b := view[r.source.ID].Bound; len(b) != 1 || b[0].Attachment != "retired" {
		t.Fatalf("bound view with no candidates = %+v, want retired", b)
	}
}

// savesInstances records every write of an [InstanceStore].
type savesInstances struct {
	memInstances
	saves []map[string]string
}

func (s *savesInstances) SaveSessionInstances(ctx context.Context, workerID string, in map[string]string) error {
	s.saves = append(s.saves, maps.Clone(in))
	return s.memInstances.SaveSessionInstances(ctx, workerID, in)
}

// A long-lived session that never changes is written again (a new seen_at) at
// most hourly, with the next change or snapshot, so the store's bound drops
// the names that are gone first and never the one still listed.
func TestInstancesOfListedNamesAreTouchedHourly(t *testing.T) {
	inst := &savesInstances{}
	now := time.UnixMilli(10_000)
	h := NewEventHub(EventHubOptions{Now: func() time.Time { return now }, Instances: inst})
	f, _ := h.Connect("wA", "a").NewFeed()
	if err := f.Snapshot(0, []SessionState{{Name: "main", InstanceID: "m1"}}); err != nil {
		t.Fatal(err)
	}
	add := func(seq uint64, name string) {
		t.Helper()
		if err := f.Apply(Change{Seq: seq, Event: EventSessionAdded, Session: SessionState{Name: name, InstanceID: "i-" + name}}); err != nil {
			t.Fatal(err)
		}
	}
	add(1, "tmp1")
	now = now.Add(instanceTouchEvery - time.Minute)
	add(2, "tmp2")
	now = now.Add(2 * time.Minute)
	add(3, "tmp3")
	want := []map[string]string{
		{"main": "m1"},
		{"tmp1": "i-tmp1"},
		{"tmp2": "i-tmp2"},
		{"main": "m1", "tmp1": "i-tmp1", "tmp2": "i-tmp2", "tmp3": "i-tmp3"},
	}
	if !reflect.DeepEqual(inst.saves, want) {
		t.Fatalf("saves = %v, want %v", inst.saves, want)
	}
	// A snapshot that changes nothing writes only once the touch is due.
	all := []SessionState{{Name: "main", InstanceID: "m1"}}
	if err := f.Snapshot(0, all); err != nil {
		t.Fatal(err)
	}
	if len(inst.saves) != 4 {
		t.Fatalf("an unchanged snapshot wrote: %v", inst.saves[4:])
	}
	now = now.Add(instanceTouchEvery)
	f2, _ := h.Connect("wA", "a").NewFeed()
	if err := f2.Snapshot(0, all); err != nil {
		t.Fatal(err)
	}
	if len(inst.saves) != 5 || !reflect.DeepEqual(inst.saves[4], map[string]string{"main": "m1"}) {
		t.Fatalf("saves = %v", inst.saves)
	}
}
