package agent

import (
	"context"
	"encoding/json"
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

// A96: the harness session name is reported with the native state and with
// the screen fallback, but only for the process that recorded it.
func TestReportStateAddsHarnessName(t *testing.T) {
	ctx := t.Context()
	base := ops.PaneInfo{Session: "work", ID: "%3", Kind: "agent:claude", InputKind: "agent:claude", State: ops.PaneStateStable}
	locator := reportStateLocator{Result: agentdetect.Result{Kind: "agent:claude"}, pid: 42}
	native := store.HarnessState{State: ops.PaneStateBusy, Source: "claude-hooks", LastEvent: time.Now(), ProcessPID: 42}

	info := base
	reportState(ctx, titledReader{state: native, ok: true, title: "release prep", pid: 42}, locator, &info, ops.Screen{}, 123, false)
	if info.HarnessName != "release prep" || info.StateSource != "claude-hooks" {
		t.Fatalf("native state with a name = %+v", info)
	}

	info = base
	reportState(ctx, titledReader{title: "release prep", pid: 42}, locator, &info, ops.Screen{}, 123, false)
	if info.HarnessName != "release prep" || info.StateSource != ops.PaneStateSourceScreen {
		t.Fatalf("screen fallback with a name = %+v", info)
	}

	info = base
	reportState(ctx, titledReader{title: "old process", pid: 41}, locator, &info, ops.Screen{}, 123, false)
	if info.HarnessName != "" {
		t.Fatalf("a name of another process was reported: %+v", info)
	}

	info = base
	reportState(ctx, titledReader{title: "codex", pid: 42}, reportStateLocator{Result: agentdetect.Result{Kind: "agent:codex"}, pid: 42}, &info, ops.Screen{}, 123, false)
	if info.HarnessName != "" {
		t.Fatalf("a name of another harness was reported: %+v", info)
	}

	// A reader without names (an older store or a test fake) reports none.
	info = base
	reportState(ctx, reportStateReader(func(context.Context, string, string, string, int64, time.Time) (store.HarnessState, bool, error) {
		return native, true, nil
	}), locator, &info, ops.Screen{}, 123, false)
	if info.HarnessName != "" || info.StateSource != "claude-hooks" {
		t.Fatalf("reader without names = %+v", info)
	}

	// The name travels as an optional field of the event frame.
	frame := eventFrameOf(1, time.Now(), Change{Event: EventStateChanged, Session: SessionState{Name: "work", PaneStateDetail: ops.PaneStateDetail{HarnessName: "release prep"}}})
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
}
