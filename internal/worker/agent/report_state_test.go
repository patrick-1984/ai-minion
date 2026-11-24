package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

type reportStateReader func(context.Context, string, string, string, int64, time.Time) (store.HarnessState, bool, error)

func (fn reportStateReader) NativeHarnessState(ctx context.Context, session, pane, harness string, created int64, now time.Time) (store.HarnessState, bool, error) {
	return fn(ctx, session, pane, harness, created, now)
}

type reportStateLocator struct {
	agentdetect.Result
	pid int
}

func (fake reportStateLocator) Classify(context.Context, agentdetect.Input) agentdetect.Result {
	return fake.Result
}

func (fake reportStateLocator) Locate(context.Context, agentdetect.Input) (agentdetect.Result, int) {
	return fake.Result, fake.pid
}

func TestClaudeBackgroundWorkIsReported(t *testing.T) {
	for _, name := range []string{"claude_footer_shell_2_1_33x", "claude_footer_agents_2_1_33x", "claude_idle_2_1_330"} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile("../agentdetect/testdata/" + name + ".txt")
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			scr := ops.Screen{Lines: strings.Split(string(body), "\n")}
			r := agentdetect.New(agentdetect.WithProcessSource(nil)).Classify(ctx, agentdetect.Input{Command: "claude", Screen: scr})
			wantState, wantReason := ops.PaneStateBusy, ops.PaneStateReasonBackground
			if name == "claude_idle_2_1_330" {
				wantState, wantReason = ops.PaneStateStable, ""
			}
			if r.State != wantState || r.Background != (wantState == ops.PaneStateBusy) {
				t.Fatalf("input classification = %+v", r)
			}
			info := ops.PaneInfo{Session: "work", ID: "%3", Kind: r.Shown(), State: r.State, InputKind: r.Kind}
			reportState(ctx, nil, nil, &info, scr, 123, r.Background)
			if info.State != wantState || info.StateSource != ops.PaneStateSourceScreen || info.StateReason != wantReason {
				t.Fatalf("reported pane = %+v, want state %s, reason %q", info, wantState, wantReason)
			}

			e := newEnv(t, func(c *Config) {
				c.Classifier = agentdetect.New(agentdetect.WithProcessSource(nil))
			})
			ref := e.host.addSession("work", "claude")
			e.host.setScreen(ref, scr.Lines...)
			raw, err := e.call(ops.OpSessionSnapshot, allActions, ops.SessionSnapshotArgs{Target: target("work")})
			if err != nil {
				t.Fatal(err)
			}
			var snap ops.SessionSnapshotResult
			if err := json.Unmarshal(raw, &snap); err != nil {
				t.Fatal(err)
			}
			if snap.Pane.State != wantState || snap.Pane.PaneStateDetail != info.PaneStateDetail {
				t.Fatalf("snapshot = %+v", snap.Pane)
			}
			raw, err = e.call(ops.OpSessionsList, allActions, ops.SessionsListArgs{})
			if err != nil {
				t.Fatal(err)
			}
			var list ops.SessionsListResult
			if err := json.Unmarshal(raw, &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Sessions) != 1 || list.Sessions[0].Pane == nil || list.Sessions[0].Pane.State != wantState || list.Sessions[0].Pane.PaneStateDetail != info.PaneStateDetail {
				t.Fatalf("list = %+v", list)
			}
			observer, err := NewHostObserver(HostObserverConfig{Host: e.host, Classifier: e.ex.classify, Policy: e.policy})
			if err != nil {
				t.Fatal(err)
			}
			view, err := observer.Observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if observed := view["work"]; observed.State != wantState || observed.PaneStateDetail != info.PaneStateDetail {
				t.Fatalf("inventory = %+v", observed)
			}
			write := e.mustWrite(ops.OpSendText, sendText("work", "hello", false))
			if write.Outcome != ops.OutcomeDelivered || write.Pane.State != wantState {
				t.Fatalf("input readiness = %+v", write)
			}

			wrapped := ops.PaneInfo{Session: "work", ID: "%3", Kind: "agent:claude", State: ops.PaneStateStable, InputKind: ops.PaneKindShell}
			reportState(ctx, nil, nil, &wrapped, scr, 123, false)
			if wrapped.State != wantState || wrapped.PaneStateDetail != info.PaneStateDetail || wrapped.InputKind != ops.PaneKindShell {
				t.Fatalf("wrapped pane = %+v", wrapped)
			}

			at := time.Unix(124, 0).UTC()
			runtimeKnown := true
			native := reportStateReader(func(ctx context.Context, session, pane, harness string, created int64, _ time.Time) (store.HarnessState, bool, error) {
				if _, ok := ctx.Deadline(); !ok || session != "work" || pane != "%3" || harness != "claude" || created != 123 {
					t.Fatalf("lookup did not use bounded pane identity: %s %s %s %d", session, pane, harness, created)
				}
				return store.HarnessState{State: ops.PaneStateStable, Source: "claude-hooks", LastEvent: at, ProcessPID: 42, RuntimeKnown: runtimeKnown}, true, nil
			})
			info.State = r.State
			lookup := reportStateLocator{Result: r, pid: 42}
			reportState(ctx, native, lookup, &info, scr, 123, r.Background)
			if info.State != ops.PaneStateStable || info.StateReason != "" || info.StateSource != "claude-hooks" || !info.LastEvent.Time().Equal(at) {
				t.Fatalf("native idle should take precedence: %+v", info)
			}
			info.State, info.PaneStateDetail = r.State, ops.PaneStateDetail{}
			runtimeKnown = false
			reportState(ctx, native, lookup, &info, scr, 123, r.Background)
			if wantState == ops.PaneStateBusy && (info.State != wantState || info.StateSource != ops.PaneStateSourceScreen || info.StateReason != wantReason) {
				t.Fatalf("positive footer evidence was lost without a native runtime registry: %+v", info)
			}
			runtimeKnown = true

			info.State, info.PaneStateDetail = r.State, ops.PaneStateDetail{}
			lookup.pid = 43
			reportState(ctx, native, lookup, &info, scr, 123, r.Background)
			if info.State != wantState || info.StateSource != ops.PaneStateSourceScreen || info.StateReason != wantReason {
				t.Fatalf("a different foreground harness applied native state: %+v", info)
			}

		})
	}
}

func TestNativeStateReasonOnlyChangePublishes(t *testing.T) {
	at := time.Date(2026, 11, 23, 23, 9, 17, 0, time.UTC)
	old := SessionState{Name: "work", Kind: "agent:claude", State: ops.PaneStateBusy, ScreenHash: "visible-screen"}
	next := old
	next.PaneStateDetail = ops.PaneStateDetail{StateSource: "claude-hooks", StateReason: "1 subagent", LastEvent: ops.Timestamp(at)}
	changes := Diff(View{"work": old}, View{"work": next})
	if len(changes) != 1 || changes[0].Event != EventStateChanged {
		t.Fatalf("state detail change = %+v", changes)
	}
	frame := eventFrameOf(1, at, changes[0])
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["state_source"] != "claude-hooks" || fields["state_reason"] != "1 subagent" || fields["last_event"] != "2026-11-23T23:09:17.000Z" {
		t.Fatalf("wire event = %s", raw)
	}
}

func TestReportedDialogKeepsScreenBinding(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(124, 0).UTC()
	screenDialog := ops.PaneInfo{Session: "work", ID: "%3", Kind: "agent:claude", InputKind: "agent:claude", State: ops.PaneStateDialog, DialogDigest: "reviewed"}
	locator := reportStateLocator{Result: agentdetect.Result{Kind: screenDialog.Kind}, pid: 42}
	nativeState := ops.PaneStateBusy
	reader := reportStateReader(func(context.Context, string, string, string, int64, time.Time) (store.HarnessState, bool, error) {
		return store.HarnessState{State: nativeState, Source: "claude-hooks", LastEvent: at, ProcessPID: 42}, true, nil
	})
	info := screenDialog
	reportState(ctx, reader, locator, &info, ops.Screen{}, 123, false)
	if info.State != ops.PaneStateDialog || info.DialogDigest != screenDialog.DialogDigest ||
		info.StateSource != ops.PaneStateSourceScreen || !info.LastEvent.IsZero() {
		t.Fatalf("conflicting native state replaced the screen dialog: %+v", info)
	}
	nativeState = ops.PaneStateDialog
	info = screenDialog
	reportState(ctx, reader, locator, &info, ops.Screen{}, 123, false)
	if info.State != ops.PaneStateDialog || info.DialogDigest != screenDialog.DialogDigest ||
		info.StateSource != "claude-hooks" || !info.LastEvent.Time().Equal(at) {
		t.Fatalf("matching native state did not augment the screen dialog: %+v", info)
	}
}

func TestNativeInventorySurvivesUnavailableScreen(t *testing.T) {
	at := time.Now()
	reader := reportStateReader(func(context.Context, string, string, string, int64, time.Time) (store.HarnessState, bool, error) {
		return store.HarnessState{ProcessPID: 42, State: ops.PaneStateBusy, Source: "claude-hooks", Reason: "1 shell", LastEvent: at}, true, nil
	})
	info := ops.PaneInfo{Session: "test", ID: "%1", Command: "claude", PID: 42, Kind: ops.PaneKindUnknown, State: ops.PaneStateUnknown}
	reportUnreadablePane(t.Context(), reader, reportStateLocator{Result: agentdetect.Result{Kind: "agent:claude"}, pid: 42}, &info, 123)
	if info.State != ops.PaneStateBusy || info.StateSource != "claude-hooks" || info.StateReason != "1 shell" {
		t.Fatalf("native state without a screen = %+v", info)
	}
}
