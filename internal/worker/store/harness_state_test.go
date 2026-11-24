package store

import (
	"os"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
)

func TestHarnessEventReplay(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	var events []HarnessEvent
	add := func(kind, entity string, registry map[string]string) {
		events = append(events, HarnessEvent{Kind: kind, Entity: entity, Registry: registry, NativeID: "a", At: t0.Add(time.Duration(len(events)) * time.Second)})
	}
	check := func(state, reason string) {
		t.Helper()
		latest := events[len(events)-1].At
		got, ok := projectHarness(events, "claude", latest)
		if !ok || got.State != state || got.Reason != reason || got.Source != "claude-hooks" || got.LastEvent != latest {
			t.Fatalf("after %s: %+v (%t), want %s %q", events[len(events)-1].Kind, got, ok, state, reason)
		}
	}
	add("session_start", "", nil)
	add("turn_start", "p", nil)
	add("task_start", "task", nil)
	add("task_start", "task", nil)
	add("turn_end", "p", nil)
	check("busy", "1 open task")
	add("task_end", "task", nil)
	check("stable", "")
	add("turn_start", "next", nil)
	add("subagent_start", "a", nil)
	add("subagent_start", "b", nil)
	add("turn_end", "next", nil)
	check("busy", "2 subagents")
	add("subagent_end", "b", nil)
	check("busy", "1 subagent")
	add("wait_permission", "", nil)
	check("dialog", "Permission requested")
	add("resume", "", nil)
	check("busy", "1 subagent")
	add("turn_end", "", map[string]string{"a": "subagent", "s": "shell", "w": "workflow", "m": "background task"})
	check("busy", "1 shell, 1 workflow, 1 subagent, 1 background task")
	add("subagent_end", "a", nil)
	check("busy", "1 shell, 1 workflow, 1 background task")
	add("turn_end", "", map[string]string{})
	check("stable", "")
	add("session_end", "", nil)
	add("turn_start", "future", nil)
	if _, ok := projectHarness(events, "claude", events[len(events)-1].At); ok {
		t.Fatal("late event reopened ended session")
	}
}

func TestHarnessActivityExpiresEvenWithHeartbeats(t *testing.T) {
	start := time.Now()
	events := []HarnessEvent{{Kind: "turn_start", NativeID: "n", At: start}}
	if _, ok := projectHarness(events, "codex", start.Add(harnessSilence+time.Second)); ok {
		t.Fatal("silent harness remained active")
	}
	events = append(events, HarnessEvent{Kind: "heartbeat", NativeID: "n", At: start.Add(14 * time.Minute)})
	got, ok := projectHarness(events, "codex", start.Add(20*time.Minute))
	if !ok || got.State != "busy" || got.LastEvent != start {
		t.Fatalf("heartbeat changed semantic state: %+v", got)
	}
	events = append(events, HarnessEvent{Kind: "heartbeat", NativeID: "n", At: start.Add(28 * time.Minute)})
	if _, ok := projectHarness(events, "codex", start.Add(31*time.Minute)); ok {
		t.Fatal("heartbeat kept stale foreground busy")
	}
	if _, ok := projectHarness(events[1:], "codex", start.Add(28*time.Minute)); ok {
		t.Fatal("liveness alone identified idle")
	}
}

func TestHarnessJournalConsentRestartAndPaneGeneration(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	s := openStore(t, dir)
	en := Enrollment{HubPub: pubKey(1), WorkerID: "work", Owner: "owner"}
	if err := s.SaveEnrollment(ctx, en); err != nil {
		t.Fatal(err)
	}
	if err := s.AcceptUsageRegistration(ctx, en.HubPub, en.WorkerID, en.Owner); err != nil {
		t.Fatal(err)
	}
	grant, err := s.UsageGrantID(ctx, en.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Minute)
	e := HarnessEvent{Session: "from-pane", Pane: "%7", Created: base.Unix(), Harness: "claude", ProcessPID: os.Getpid(), ProcessBirth: agentdetect.ProcessBirth(os.Getpid()), NativeID: ops.UsageID("test", "old"), At: base, Kind: "session_start"}
	if e.ProcessBirth == "" {
		t.Skip("process lifetime unavailable on this platform")
	}
	add := func(offset int, kind string) {
		t.Helper()
		e.At = base.Add(time.Duration(offset) * time.Second)
		e.Kind = kind
		if err := s.RecordHarnessEvent(ctx, grant, e); err != nil {
			t.Fatal(err)
		}
	}
	add(0, "session_start")
	id := ops.UsageID("test", "shell")
	e.Registry = map[string]string{id: "shell"}
	add(4, "turn_end")
	e.Registry = nil
	add(1, "turn_start") // async hook committed out of order
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dir)
	read := func(pane string, created int64) (HarnessState, bool) {
		t.Helper()
		got, ok, err := s.NativeHarnessState(ctx, e.Session, pane, e.Harness, created, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return got, ok
	}
	if got, ok := read("%7", e.Created); !ok || got.Reason != "1 shell" {
		t.Fatalf("reopened journal: %+v", got)
	}
	if _, ok := read("%8", e.Created); ok {
		t.Fatal("different pane")
	}
	if _, ok := read("%7", e.Created+1); ok {
		t.Fatal("reused session name")
	}
	oldID := e.NativeID
	e.NativeID = ops.UsageID("test", "new")
	add(5, "session_start")
	e.NativeID = oldID
	add(6, "turn_start")
	if got, ok := read("%7", e.Created); !ok || got.State != "stable" {
		t.Fatalf("late old session: %+v", got)
	}
	e.NativeID = ops.UsageID("test", "new")
	e.ProcessBirth = "obsolete"
	add(7, "turn_start")
	if _, ok := read("%7", e.Created); ok {
		t.Fatal("expired process accepted")
	}
	if _, err := s.db.Exec("UPDATE hub_enrollment SET usage_policy=0 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHarnessEvent(ctx, grant, e); err == nil {
		t.Fatal("revoked grant wrote events")
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM harness_events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("revocation left %d events: %v", count, err)
	}
}

func TestChildWaitSurvivesParentStopUntilChildResumes(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	events := []HarnessEvent{
		{Kind: "turn_start", At: start},
		{Kind: "subagent_start", Entity: "a", At: start.Add(time.Second)},
		{Kind: "wait_permission", Entity: "a", At: start.Add(2 * time.Second)},
		{Kind: "resume", Entity: "b", At: start.Add(3 * time.Second)},
		{Kind: "turn_end", At: start.Add(4 * time.Second)},
	}
	got, ok := projectHarness(events, "claude", time.Now())
	if !ok || got.State != "dialog" {
		t.Fatalf("parent released child wait: %+v", got)
	}
	events = append(events, HarnessEvent{Kind: "resume", Entity: "a", At: start.Add(5 * time.Second)})
	got, ok = projectHarness(events, "claude", time.Now())
	if !ok || got.State != "busy" || got.Reason != "1 subagent" {
		t.Fatalf("child did not resume: %+v", got)
	}
}

func TestEmptyRegistryDistinguishesPlannerFromRuntime(t *testing.T) {
	now := time.Now()
	events := []HarnessEvent{
		{Kind: "task_start", Entity: "planner", At: now},
		{Kind: "turn_end", Registry: map[string]string{}, At: now},
	}
	got, ok := projectHarness(events, "claude", now)
	if !ok || got.State != "stable" || got.Reason != "1 open task" || !got.RuntimeKnown {
		t.Fatalf("pending planner task treated as a running process: %+v", got)
	}
}
