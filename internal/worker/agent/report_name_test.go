package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// titledReader is a native-state reader that also knows the harness session name.
type titledReader struct {
	state store.HarnessState
	ok    bool
	title string
	pid   int
}

func (r titledReader) NativeHarnessState(context.Context, string, string, string, int64, time.Time) (store.HarnessState, bool, error) {
	return r.state, r.ok, nil
}

func (r titledReader) HarnessTitle(context.Context, string, string, string, int64, time.Time) (string, int, error) {
	return r.title, r.pid, nil
}

// A96: the harness session name is reported on the state stream with the
// native state and with the screen fallback, but only for the process that
// recorded it. Operation results (PaneInfo) never carry it.
func TestReportedHarnessName(t *testing.T) {
	ctx := t.Context()
	base := ops.PaneInfo{Session: "work", ID: "%3", Kind: "agent:claude", InputKind: "agent:claude", State: ops.PaneStateStable}
	locator := reportStateLocator{Result: agentdetect.Result{Kind: "agent:claude"}, pid: 42}
	native := store.HarnessState{State: ops.PaneStateBusy, Source: "claude-hooks", LastEvent: time.Now(), ProcessPID: 42}

	name := func(r nativeStateReader, c Classifier, info ops.PaneInfo) (string, ops.PaneInfo) {
		pid := reportState(ctx, r, c, &info, ops.Screen{}, 123, false)
		return reportedHarnessName(ctx, r, c, info, 123, pid), info
	}
	if got, info := name(titledReader{state: native, ok: true, title: "release prep", pid: 42}, locator, base); got != "release prep" || info.StateSource != "claude-hooks" {
		t.Fatalf("native state with a name = %q %+v", got, info)
	}
	if got, info := name(titledReader{title: "release prep", pid: 42}, locator, base); got != "release prep" || info.StateSource != ops.PaneStateSourceScreen {
		t.Fatalf("screen fallback with a name = %q %+v", got, info)
	}
	if got, _ := name(titledReader{title: "old process", pid: 41}, locator, base); got != "" {
		t.Fatalf("a name of another process was reported: %q", got)
	}
	if got, _ := name(titledReader{title: "codex", pid: 42}, reportStateLocator{Result: agentdetect.Result{Kind: "agent:codex"}, pid: 42}, base); got != "" {
		t.Fatalf("a name of another harness was reported: %q", got)
	}
	shell := base
	shell.InputKind = ops.PaneKindShell // a shell with a background agent
	if got, _ := name(titledReader{title: "release prep", pid: 42}, locator, shell); got != "" {
		t.Fatalf("a name for a display-only agent kind: %q", got)
	}
	// A reader without names (an older store or a test fake) reports none.
	if got, info := name(reportStateReader(func(context.Context, string, string, string, int64, time.Time) (store.HarnessState, bool, error) {
		return native, true, nil
	}), locator, base); got != "" || info.StateSource != "claude-hooks" {
		t.Fatalf("reader without names = %q %+v", got, info)
	}

	// The pane of an operation result has no name, whatever the reader knows.
	info := base
	reportState(ctx, titledReader{state: native, ok: true, title: "release prep", pid: 42}, locator, &info, ops.Screen{}, 123, false)
	if raw, err := json.Marshal(info); err != nil || strings.Contains(string(raw), "harness_name") || strings.Contains(string(raw), "release prep") {
		t.Fatalf("operation pane = %s (%v)", raw, err)
	}

	// The name travels as an optional field of the event frame.
	frame := eventFrameOf(1, time.Now(), Change{Event: EventStateChanged, Session: SessionState{Name: "work", HarnessName: "release prep"}})
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil || fields["harness_name"] != "release prep" {
		t.Fatalf("wire event = %s", raw)
	}
	raw, _ = json.Marshal(eventFrameOf(2, time.Now(), Change{Event: EventStateChanged, Session: SessionState{Name: "work"}}))
	fields = nil
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["harness_name"]; ok {
		t.Fatalf("an empty name is sent: %s", raw)
	}
	// A rename alone is a state change.
	if d := Diff(View{"work": {Name: "work"}}, View{"work": {Name: "work", HarnessName: "x"}}); len(d) != 1 || d[0].Event != EventStateChanged {
		t.Fatalf("diff of a rename = %+v", d)
	}
}
