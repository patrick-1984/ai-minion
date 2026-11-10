package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
	"github.com/patrick-1984/ai-minion/internal/worker/term/tmux"
)

const secretText = "SECRET-TEXT-4711"

func TestNewExecutorRequiresItsParts(t *testing.T) {
	e := newEnv(t)
	good := Config{Host: e.host, Classifier: e.cls, State: e.st, Policy: e.policy}
	for name, mut := range map[string]func(*Config){
		"host":       func(c *Config) { c.Host = nil },
		"classifier": func(c *Config) { c.Classifier = nil },
		"state":      func(c *Config) { c.State = nil },
		"policy":     func(c *Config) { c.Policy = nil },
	} {
		c := good
		mut(&c)
		if _, err := NewExecutor(c); err == nil {
			t.Errorf("executor without %s was accepted", name)
		}
	}
	if _, err := NewExecutor(good); err != nil {
		t.Fatal(err)
	}
}

// --- AC 1: message versus operate -------------------------------------------

func TestMessageOnlyCallerIsForbiddenNotRefused(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.host.setScreen(ref, "> ")

	for _, force := range []bool{false, true} {
		_, err := e.writeAs(ops.OpSendText, messageOnly, sendText("work", "hi", force))
		wantAPIError(t, err, "forbidden", "")
		_, err = e.writeAs(ops.OpSendKeys, messageOnly, sendKeys("work", force, "Enter"))
		wantAPIError(t, err, "forbidden", "")
	}
	if n := len(e.host.pastes()) + len(e.host.keys()); n != 0 {
		t.Fatalf("a forbidden write reached the pane (%d writes)", n)
	}
}

func TestAuthorizeWriteMessageVersusOperate(t *testing.T) {
	both := ops.MustActionSet(ops.ActionMessage, ops.ActionOperate)
	cases := []struct {
		name string
		held []ops.Action
		kind string
		ok   bool
	}{
		{"message to agent", messageOnly, "agent:claude", true},
		{"message to shell", messageOnly, ops.PaneKindShell, false},
		{"message to other", messageOnly, ops.PaneKindOther, false},
		{"message to unknown", messageOnly, ops.PaneKindUnknown, false},
		{"message to bare agent prefix", messageOnly, "agent:", false},
		{"operate to shell", operateActions, ops.PaneKindShell, true},
		{"operate to agent", operateActions, "agent:codex", true},
		{"message and operate to shell", []ops.Action{ops.ActionMessage, ops.ActionOperate}, ops.PaneKindShell, true},
		{"manage only", []ops.Action{ops.ActionManage}, "agent:claude", false},
		{"read only", []ops.Action{ops.ActionRead}, "agent:claude", false},
		{"no action", nil, "agent:claude", false},
	}
	for _, c := range cases {
		err := authorizeWrite(both, c.held, c.kind)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
		if err != nil {
			wantAPIError(t, err, "forbidden", "")
		}
	}
	// An op that accepts only operate never lets message through, whatever
	// the pane is.
	if err := authorizeWrite(ops.MustActionSet(ops.ActionOperate), messageOnly, "agent:claude"); err == nil {
		t.Fatal("message delivered through an operate-only op")
	}
}

// --- AC 2: the guard matrix --------------------------------------------------

func TestGuardMatrix(t *testing.T) {
	type tc struct {
		name         string
		kind, state  string
		refusal      proto.Code // empty when delivered without force
		warnReadines bool
	}
	cases := []tc{
		{"agent stable", "agent:claude", ops.PaneStateStable, "", false},
		{"agent busy", "agent:claude", ops.PaneStateBusy, proto.CodeBusy, false},
		{"agent dialog", "agent:claude", ops.PaneStateDialog, proto.CodeDialogOpen, false},
		{"agent unknown state", "agent:claude", ops.PaneStateUnknown, "", true},
		{"shell stable", ops.PaneKindShell, ops.PaneStateStable, proto.CodeShellPane, false},
		{"shell busy", ops.PaneKindShell, ops.PaneStateBusy, proto.CodeShellPane, false},
		{"shell unknown state", ops.PaneKindShell, ops.PaneStateUnknown, proto.CodeShellPane, true},
		{"other unknown", ops.PaneKindOther, ops.PaneStateUnknown, "", true},
		{"unknown unknown", ops.PaneKindUnknown, ops.PaneStateUnknown, "", true},
	}
	for _, op := range []string{ops.OpSendText, ops.OpSendKeys} {
		for _, c := range cases {
			for _, force := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/force=%v", op, c.name, force), func(t *testing.T) {
					e := newEnv(t)
					ref := e.host.addSession("work", "prog")
					e.host.setScreen(ref, "screen")
					e.cls.set("prog", c.kind, c.state)

					var res ops.WriteResult
					if op == ops.OpSendText {
						res = e.mustWrite(op, sendText("work", "hello", force))
					} else {
						res = e.mustWrite(op, sendKeys("work", force, "Enter"))
					}
					sent := len(e.host.pastes()) + len(e.host.keys())

					if res.Pane == nil || res.Pane.Kind != c.kind || res.Pane.State != c.state || res.Pane.ID != ref.ID {
						t.Fatalf("pane = %+v", res.Pane)
					}
					if hasWarning(res, "readiness_unknown") != c.warnReadines {
						t.Fatalf("warnings = %+v, want readiness_unknown=%v", res.Warnings, c.warnReadines)
					}
					switch {
					case c.refusal != "" && !force:
						if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != string(c.refusal) || !res.Forceable || res.Forced || sent != 0 {
							t.Fatalf("refusal = %+v, sent %d", res, sent)
						}
					case c.refusal != "":
						if res.Outcome != ops.OutcomeDelivered || !res.Forced || res.Forceable || sent != 1 {
							t.Fatalf("forced = %+v, sent %d", res, sent)
						}
					default:
						// Nothing blocks: delivered, and force is not "used".
						if res.Outcome != ops.OutcomeDelivered || res.Forced || res.Forceable || sent != 1 {
							t.Fatalf("plain delivery = %+v, sent %d", res, sent)
						}
					}
				})
			}
		}
	}
}

func hasWarning(r ops.WriteResult, code string) bool {
	for _, w := range r.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func TestForcedRetryNeedsANewRequest(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateBusy)

	refused := sendText("work", "go", false)
	if res := e.mustWrite(ops.OpSendText, refused); !res.Forceable {
		t.Fatalf("not refused: %+v", res)
	}
	// The same request id with force=true is a different request: conflict.
	again := refused
	again.Force = true
	_, err := e.write(ops.OpSendText, again)
	wantAPIError(t, err, "conflict", "")
	// A fresh request with force=true sends.
	res := e.mustWrite(ops.OpSendText, sendText("work", "go", true))
	if res.Outcome != ops.OutcomeDelivered || !res.Forced {
		t.Fatalf("forced = %+v", res)
	}
}

// --- AC 3: pane change -------------------------------------------------------

func TestPaneChangedIsKeepForceCannotOverride(t *testing.T) {
	for _, force := range []bool{false, true} {
		e := newEnv(t)
		e.host.addSession("work", "claude")
		state := ops.PaneStateStable
		if force {
			state = ops.PaneStateBusy // force=true overrides the busy guard, and still cannot override KEEP
		}
		e.cls.set("claude", "agent:claude", state)
		e.host.pasteErr = fmt.Errorf("paste: %w", term.ErrPaneChanged)

		res := e.mustWrite(ops.OpSendText, sendText("work", "x", force))
		if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "pane_changed" || res.Forceable {
			t.Fatalf("force=%v: %+v", force, res)
		}
		if res.Forced != force {
			t.Fatalf("force=%v: forced=%v (the busy guard was overridden only with force)", force, res.Forced)
		}
	}
}

func TestSendKeysPaneChangedMidwayIsPartial(t *testing.T) {
	cases := []struct {
		after int
		want  ops.Outcome
	}{{0, ops.OutcomeFailedNotSent}, {1, ops.OutcomePartial}, {3, ops.OutcomePartial}}
	for _, c := range cases {
		e := newEnv(t)
		e.host.addSession("work", "claude")
		e.cls.set("claude", "agent:claude", ops.PaneStateStable)
		e.host.keysErr, e.host.keysAfter = fmt.Errorf("keys: %w", term.ErrPaneChanged), c.after

		res := e.mustWrite(ops.OpSendKeys, sendKeys("work", true, "a", "b", "c", "d"))
		if res.Outcome != c.want || res.Reason != "pane_changed" || res.Forceable {
			t.Fatalf("after %d keys: %+v, want %s", c.after, res, c.want)
		}
	}
}

func TestSendKeysOtherErrorNeverReportsNotSentAfterAKeyWent(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.host.keysErr, e.host.keysAfter = errors.New("tmux exploded"), 2

	res := e.mustWrite(ops.OpSendKeys, sendKeys("work", false, "a", "b", "c"))
	if res.Outcome != ops.OutcomePartial || res.Reason != "internal" {
		t.Fatalf("result = %+v", res)
	}
}

func TestPaneSwappedBeforeTheWriteIsDetected(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	// The second resolution (after the writer lock) sees another process.
	e.host.resolveHook = func(call int, _ ops.Target) (term.PaneRef, bool) {
		if call == 2 {
			swapped := ref
			swapped.PID++
			return swapped, true
		}
		return term.PaneRef{}, false
	}
	res := e.mustWrite(ops.OpSendText, sendText("work", "x", true))
	if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "pane_changed" || res.Forceable {
		t.Fatalf("result = %+v", res)
	}
	if len(e.host.pastes()) != 0 {
		t.Fatal("text went to a pane that changed")
	}
}

func TestUnknownHostErrorsOnPasteAreUnknownOutcome(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.host.pasteErr = errors.New("tmux exploded")
	res := e.mustWrite(ops.OpSendText, sendText("work", "x", false))
	if res.Outcome != ops.OutcomeUnknown || res.Reason != "internal" {
		t.Fatalf("result = %+v", res)
	}
}

func TestMissingTargetIsAnAPIErrorNotAnOutcome(t *testing.T) {
	e := newEnv(t)
	_, err := e.write(ops.OpSendText, sendText("ghost", "x", false))
	wantAPIError(t, err, "target_not_found", "")
	_, err = e.write(ops.OpSetTask, ops.SetTaskArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("ghost"), Task: "t"})
	wantAPIError(t, err, "target_not_found", "")
}

// --- AC 4: duplicate request ids ---------------------------------------------

func TestDuplicateRequestIDReturnsTheCachedResult(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)

	args := sendText("work", secretText, false)
	first := e.mustWrite(ops.OpSendText, args)
	e.host.mu.Lock()
	snaps, resolves := e.host.snapshots, e.host.resolveCall
	e.host.mu.Unlock()

	second := e.mustWrite(ops.OpSendText, args)
	if got := mustJSON(t, second); string(got) != string(mustJSON(t, first)) {
		t.Fatalf("second = %s, first = %s", got, mustJSON(t, first))
	}
	if n := len(e.host.pastes()); n != 1 {
		t.Fatalf("the text was pasted %d times", n)
	}
	e.host.mu.Lock()
	touched := e.host.snapshots != snaps || e.host.resolveCall != resolves
	e.host.mu.Unlock()
	if touched {
		t.Fatal("a duplicate touched the pane")
	}
	if n := len(e.audit()); n != 1 {
		t.Fatalf("audit rows = %d, want 1 (a cached answer is not a new write)", n)
	}
}

func TestDuplicateRequestIDAnswersCachedRefusalToo(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateBusy)
	args := sendText("work", "x", false)
	first := e.mustWrite(ops.OpSendText, args)
	e.cls.set("claude", "agent:claude", ops.PaneStateStable) // now it would be delivered
	second := e.mustWrite(ops.OpSendText, args)
	if string(mustJSON(t, first)) != string(mustJSON(t, second)) || second.Outcome != ops.OutcomeFailedNotSent {
		t.Fatalf("first %+v, second %+v", first, second)
	}
}

func TestRequestIDReusedForAnotherRequestConflicts(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.host.addSession("other", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)

	id := ops.NewUUIDv7()
	a := ops.SendTextArgs{WriteArgs: ops.WriteArgs{RequestID: id}, Target: target("work"), Text: "x"}
	e.mustWrite(ops.OpSendText, a)

	b := a
	b.Target = target("other")
	_, err := e.write(ops.OpSendText, b)
	wantAPIError(t, err, "conflict", "")
	_, err = e.write(ops.OpSendKeys, ops.SendKeysArgs{WriteArgs: ops.WriteArgs{RequestID: id}, Target: target("work"), Keys: []string{"Enter"}})
	wantAPIError(t, err, "conflict", "")
	if n := len(e.host.pastes()) + len(e.host.keys()); n != 1 {
		t.Fatalf("writes = %d, want 1", n)
	}
}

func TestResultCacheExpiresAfterTenMinutes(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	args := sendText("work", "x", false)

	e.mustWrite(ops.OpSendText, args)
	e.clk.Advance(store.ResultTTL - time.Second)
	e.mustWrite(ops.OpSendText, args)
	if n := len(e.host.pastes()); n != 1 {
		t.Fatalf("pastes within 10 min = %d, want 1", n)
	}
	e.clk.Advance(2 * time.Second)
	e.mustWrite(ops.OpSendText, args)
	if n := len(e.host.pastes()); n != 2 {
		t.Fatalf("pastes after 10 min = %d, want 2", n)
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	e := newEnv(t)
	args := sendText("work", "x", false)
	_, err := e.write(ops.OpSendText, args)
	wantAPIError(t, err, "target_not_found", "")
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	res := e.mustWrite(ops.OpSendText, args)
	if res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("retry after a not-found error = %+v", res)
	}
}

func TestConcurrentDuplicatesSendOnce(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.host.onPaste = func() { time.Sleep(50 * time.Millisecond) }
	args := sendText("work", "x", false)

	var wg sync.WaitGroup
	results := make([]ops.WriteResult, 4)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.write(ops.OpSendText, args)
			if err != nil {
				t.Error(err)
			}
			results[i] = res
		}()
	}
	wg.Wait()
	if n := len(e.host.pastes()); n != 1 {
		t.Fatalf("pastes = %d, want 1", n)
	}
	for _, r := range results {
		if r.Outcome != ops.OutcomeDelivered {
			t.Fatalf("results = %+v", results)
		}
	}
}

// --- AC 5: writer locks ------------------------------------------------------

func TestWriterLockTimeoutIsBusyAndNotForceable(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LockWait = 80 * time.Millisecond })
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)

	entered, release := make(chan struct{}), make(chan struct{})
	e.host.onPaste = func() {
		select {
		case entered <- struct{}{}:
			<-release
		default:
		}
	}
	done := make(chan ops.WriteResult, 1)
	go func() { done <- e.mustWriteQuiet(ops.OpSendText, sendText("work", "first", false)) }()
	<-entered

	for _, force := range []bool{false, true} {
		res := e.mustWrite(ops.OpSendText, sendText("work", "second", force))
		if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "busy" || res.Forceable {
			t.Fatalf("force=%v: %+v", force, res)
		}
	}
	close(release)
	if res := <-done; res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("first = %+v", res)
	}
	if got := e.host.pastes(); len(got) != 1 || got[0].Text != "first" {
		t.Fatalf("pastes = %+v", got)
	}
	if n := e.ex.Locks().held(); n != 0 {
		t.Fatalf("%d lock entries leaked", n)
	}
}

// mustWriteQuiet is mustWrite for goroutines other than the test's own.
func (e *env) mustWriteQuiet(op string, args any) ops.WriteResult {
	raw, err := e.call(op, allActions, args)
	if err != nil {
		e.t.Error(err)
		return ops.WriteResult{}
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil {
		e.t.Error(err)
	}
	return res
}

func TestWritersToOnePaneAreSerialised(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LockWait = 5 * time.Second })
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)

	var inside, maxInside atomic.Int32
	e.host.onPaste = func() {
		n := inside.Add(1)
		for {
			m := maxInside.Load()
			if n <= m || maxInside.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inside.Add(-1)
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := e.mustWriteQuiet(ops.OpSendText, sendText("work", fmt.Sprint("m", i), false)); res.Outcome != ops.OutcomeDelivered {
				t.Errorf("result = %+v", res)
			}
		}()
	}
	wg.Wait()
	if maxInside.Load() != 1 || len(e.host.pastes()) != 6 {
		t.Fatalf("max concurrent writers = %d, pastes = %d", maxInside.Load(), len(e.host.pastes()))
	}
}

func TestPaneLockIsSharedWithOtherEntryPoints(t *testing.T) {
	locks := NewLocks()
	e := newEnv(t, func(c *Config) { c.Locks = locks; c.LockWait = 30 * time.Millisecond })
	ref := e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)

	// Another entry point (the pipe, the local socket) holds the pane lock.
	unlock, err := locks.Lock(context.Background(), PaneKey(ref.ID), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res := e.mustWrite(ops.OpSendText, sendText("work", "x", true))
	if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "busy" || res.Forceable {
		t.Fatalf("while locked: %+v", res)
	}
	unlock()
	if res := e.mustWrite(ops.OpSendText, sendText("work", "x", false)); res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("after unlock: %+v", res)
	}
}

func TestCanceledContextBeforeTheWriteIsAnError(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.ex.Execute(ctx, Request{Op: ops.OpSendText, Actions: allActions, Args: mustJSON(t, sendText("work", "x", false))})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(e.host.pastes()) != 0 {
		t.Fatal("text sent on a canceled context")
	}
}

// --- AC 6: local policy ------------------------------------------------------

func TestPreviewOnlyRefusesEveryWrite(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.policy.SetPreviewOnly(true)

	wa := func() ops.WriteArgs { return ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: true} }
	writes := map[string]any{
		ops.OpSendText: ops.SendTextArgs{WriteArgs: wa(), Target: target("work"), Text: "x"},
		ops.OpSendKeys: ops.SendKeysArgs{WriteArgs: wa(), Target: target("work"), Keys: []string{"Enter"}},
		ops.OpSetTask:  ops.SetTaskArgs{WriteArgs: wa(), Target: target("work"), Task: "t"},
		ops.OpAnswer: ops.AnswerArgs{WriteArgs: wa(), Target: target("work"), Choice: "1", Expect: ops.ReviewedDialog{
			PaneID: "%1", PID: 1001, Command: "claude", SessionCreated: 1, DialogDigest: strings.Repeat("a", 64)}},
		ops.OpSessionCreate: ops.SessionCreateArgs{WriteArgs: wa()},
	}
	for op, args := range writes {
		// Whatever the caller holds: operate and manage together.
		_, err := e.writeAs(op, []ops.Action{ops.ActionOperate, ops.ActionManage, ops.ActionRead}, args)
		wantAPIError(t, err, "forbidden", "preview_only")
	}
	if len(e.host.pastes())+len(e.host.keys()) != 0 || len(e.host.options) != 0 || len(e.host.sessions) != 1 {
		t.Fatal("a preview-only Worker changed something")
	}
	// Reads still work.
	if _, err := e.call(ops.OpSessionSnapshot, allActions, ops.SessionSnapshotArgs{Target: target("work")}); err != nil {
		t.Fatalf("snapshot under preview_only: %v", err)
	}
	// And turning it off restores writes.
	e.policy.SetPreviewOnly(false)
	if res := e.mustWrite(ops.OpSendText, sendText("work", "x", false)); res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("after preview_only off: %+v", res)
	}
}

func TestUnsharedSessionIsHiddenAndNotWritable(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"shared", "private"} {
		ref := e.host.addSession(n, "claude")
		e.host.setScreen(ref, "content of "+n)
	}
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"shared"}}); err != nil {
		t.Fatal(err)
	}

	listed := e.list()
	if len(listed) != 1 || listed[0].Name != "shared" {
		t.Fatalf("sessions_list = %+v, want only the shared session", listed)
	}
	// Writes: forbidden/preview_only whatever the caller holds and force.
	_, err := e.write(ops.OpSendText, sendText("private", "x", true))
	wantAPIError(t, err, "forbidden", "preview_only")
	_, err = e.write(ops.OpSetTask, ops.SetTaskArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: true}, Target: target("private"), Task: "t"})
	wantAPIError(t, err, "forbidden", "preview_only")
	// Reads expose no content.
	_, err = e.call(ops.OpSessionSnapshot, allActions, ops.SessionSnapshotArgs{Target: target("private")})
	wantAPIError(t, err, "forbidden", "not_shared")
	_, err = e.call(ops.OpSessionCapture, allActions, ops.SessionCaptureArgs{Target: target("private")})
	wantAPIError(t, err, "forbidden", "not_shared")
	if len(e.host.pastes()) != 0 {
		t.Fatal("text reached an unshared session")
	}
	// The shared one works.
	if res := e.mustWrite(ops.OpSendText, sendText("shared", "x", false)); res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("shared write = %+v", res)
	}

	// Modes none and later share nothing; all shares both.
	for mode, want := range map[store.ShareMode]int{store.ShareNone: 0, store.ShareLater: 0, store.ShareAll: 2} {
		if err := e.st.SaveShare(store.ShareSelection{Mode: mode}); err != nil {
			t.Fatal(err)
		}
		if got := len(e.list()); got != want {
			t.Fatalf("mode %s lists %d sessions, want %d", mode, got, want)
		}
	}
}

func (e *env) list() []ops.SessionInfo {
	e.t.Helper()
	raw, err := e.call(ops.OpSessionsList, []ops.Action{ops.ActionRead}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	var res ops.SessionsListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		e.t.Fatal(err)
	}
	if res.Sessions == nil {
		e.t.Fatal("sessions must be an array")
	}
	return res.Sessions
}

// --- sessions, snapshot, capture ---------------------------------------------

func TestSessionsListShowsKindStateAndTask(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, "> ")
	e.cls.set("claude", "agent:claude", ops.PaneStateBusy)
	e.host.addSession("plain", "weird")
	e.host.sessions[0].Task = "fix the build"
	e.host.sessions[0].Attached = true

	got := e.list()
	if len(got) != 2 {
		t.Fatalf("sessions = %+v", got)
	}
	w := got[0]
	if w.Name != "work" || w.Task != "fix the build" || !w.Attached || w.Windows != 1 || w.Pane == nil ||
		w.Pane.Kind != "agent:claude" || w.Pane.State != "busy" || w.Pane.ID != ref.ID || w.Pane.Command != "claude" {
		t.Fatalf("work = %+v pane %+v", w, w.Pane)
	}
	if p := got[1].Pane; p == nil || p.Kind != ops.PaneKindOther || p.State != ops.PaneStateUnknown {
		t.Fatalf("plain pane = %+v", p)
	}
}

// TestSessionsListEchoesTheProbeOfSharedSessionsOnly: the probe marker is
// reported next to the task, never in place of it, and an unshared session
// exposes nothing, the marker included.
func TestSessionsListEchoesTheProbeOfSharedSessionsOnly(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"shared", "private", "bare"} {
		e.host.addSession(n, "claude")
	}
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.host.sessions[0].Task = "minion-probe-debugging"
	e.host.sessions[0].Probe = "nonce-1"
	e.host.sessions[1].Probe = "nonce-private"
	if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareSelect, Sessions: []string{"shared", "bare"}}); err != nil {
		t.Fatal(err)
	}
	got := e.list()
	if len(got) != 2 {
		t.Fatalf("sessions = %+v", got)
	}
	if got[0].Name != "shared" || got[0].Probe != "nonce-1" || got[0].Task != "minion-probe-debugging" {
		t.Errorf("shared = %+v", got[0])
	}
	if got[1].Name != "bare" || got[1].Probe != "" {
		t.Errorf("bare = %+v", got[1])
	}
	raw, err := json.Marshal(got[1])
	if err != nil || strings.Contains(string(raw), "probe") {
		t.Errorf("an unset probe must be omitted from the wire form: %s %v", raw, err)
	}
}

func TestReadsNeedTheReadAction(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	for _, op := range []string{ops.OpSessionsList, ops.OpSessionSnapshot, ops.OpSessionCapture} {
		args := any(nil)
		if op != ops.OpSessionsList {
			args = map[string]any{"target": target("work")}
		}
		_, err := e.call(op, operateActions, args)
		wantAPIError(t, err, "forbidden", "")
	}
}

func TestSnapshotAndCapture(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, "hello")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)

	raw, err := e.call(ops.OpSessionSnapshot, allActions, ops.SessionSnapshotArgs{Target: target("work")})
	if err != nil {
		t.Fatal(err)
	}
	var snap ops.SessionSnapshotResult
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Screen.Lines[0] != "hello" || snap.Pane.State != "stable" || snap.Pane.Session != "work" {
		t.Fatalf("snapshot = %+v", snap)
	}

	for lines, want := range map[int]string{0: "capture 200", 5: "capture 5"} {
		raw, err = e.call(ops.OpSessionCapture, allActions, ops.SessionCaptureArgs{Target: target("work"), Lines: lines})
		if err != nil {
			t.Fatal(err)
		}
		var c ops.SessionCaptureResult
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		if c.Screen.Lines[0] != want || c.Pane.Kind != "agent:claude" {
			t.Fatalf("lines=%d: %+v", lines, c)
		}
	}
	_, err = e.call(ops.OpSessionSnapshot, allActions, ops.SessionSnapshotArgs{Target: target("ghost")})
	wantAPIError(t, err, "target_not_found", "")
}

// --- session_create, set_task, answer ----------------------------------------

func TestSessionCreateNamesFromTheCounter(t *testing.T) {
	e := newEnv(t)
	create := func(args ops.SessionCreateArgs) ops.WriteResult {
		args.RequestID = ops.NewUUIDv7()
		return e.mustWrite(ops.OpSessionCreate, args)
	}
	a := create(ops.SessionCreateArgs{})
	b := create(ops.SessionCreateArgs{})
	if a.Outcome != ops.OutcomeDelivered || a.Pane == nil || a.Pane.Session != "minion-1" || b.Pane.Session != "minion-2" {
		t.Fatalf("created %+v / %+v", a.Pane, b.Pane)
	}
	if a.Pane.ID == "" || a.Pane.Kind == "" {
		t.Fatalf("pane = %+v", a.Pane)
	}
	named := create(ops.SessionCreateArgs{Name: "proj"})
	if named.Pane.Session != "proj" {
		t.Fatalf("named = %+v", named.Pane)
	}
	// A named session does not use up a number.
	if n, _ := e.st.NextSessionNumber(); n != 3 {
		t.Fatalf("next number = %d, want 3", n)
	}
}

func TestSessionCreateNeverTouchesAnExistingSession(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	_, err := e.write(ops.OpSessionCreate, ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Name: "work"})
	wantAPIError(t, err, "conflict", "")
	if len(e.host.sessions) != 1 {
		t.Fatalf("sessions = %d", len(e.host.sessions))
	}
}

func TestSessionCreateSkipsAdoptedMinionNames(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("minion-1", "claude") // adopted, same shape as ours
	res := e.mustWrite(ops.OpSessionCreate, ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}})
	if res.Pane == nil || res.Pane.Session != "minion-2" {
		t.Fatalf("pane = %+v, want minion-2", res.Pane)
	}
}

func TestSessionCreateLosingARaceForTheNameIsAConflict(t *testing.T) {
	e := newEnv(t)
	e.host.createRace, e.host.createErr = "proj", fmt.Errorf("session %q: %w", "proj", tmux.ErrSessionExists)
	_, err := e.write(ops.OpSessionCreate, ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Name: "proj"})
	wantAPIError(t, err, "conflict", "")
}

// A creation failure that does not prove that nothing started is unknown, not
// an API error: the program may have run (see the exit test below).
func TestSessionCreateHostFailureIsUnknownAndCached(t *testing.T) {
	e := newEnv(t)
	e.host.createErr = errors.New("boom")
	args := ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}}
	res := e.mustWrite(ops.OpSessionCreate, args)
	if res.Outcome != ops.OutcomeUnknown || res.Reason != "internal" {
		t.Fatalf("result = %+v", res)
	}
	if b := string(mustJSON(t, res)); strings.Contains(b, "boom") {
		t.Fatalf("host error text leaked into the result: %s", b)
	}
}

// TestSessionCreateProgramThatExitedBeforeReadbackIsNotStartedAgain: the
// program does its work and exits before the adapter reads the session back,
// so the session is gone and the adapter says not found. A repeat of the
// request must still not start it again.
func TestSessionCreateProgramThatExitedBeforeReadbackIsNotStartedAgain(t *testing.T) {
	for _, name := range []string{"", "job"} {
		e := newEnv(t)
		e.host.createErr = fmt.Errorf("session created, but its pane is already gone: %w", term.ErrNotFound)
		e.host.createGone = true
		args := ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Name: name, Argv: []string{"job"}}
		res := e.mustWrite(ops.OpSessionCreate, args)
		if res.Outcome != ops.OutcomeUnknown || e.host.creates != 1 || len(e.host.sessions) != 0 {
			t.Fatalf("name %q: result %+v, %d launches, %d sessions", name, res, e.host.creates, len(e.host.sessions))
		}
		e.host.createErr, e.host.createGone = nil, false
		again := e.mustWrite(ops.OpSessionCreate, args)
		if again.Outcome != ops.OutcomeUnknown || e.host.creates != 1 {
			t.Fatalf("name %q: duplicate %+v, %d launches", name, again, e.host.creates)
		}
	}
}

func TestSessionCreatePassesCwdAndArgvAndDedups(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.DefaultCwd = "/srv/default" })
	args := ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Argv: []string{"claude", "--x"}}
	first := e.mustWrite(ops.OpSessionCreate, args)
	if e.host.createCwd != "/srv/default" || len(e.host.createArgv) != 2 {
		t.Fatalf("cwd %q argv %v", e.host.createCwd, e.host.createArgv)
	}
	again := e.mustWrite(ops.OpSessionCreate, args)
	if len(e.host.sessions) != 1 || again.Pane.Session != first.Pane.Session {
		t.Fatalf("a duplicate created another session: %d sessions", len(e.host.sessions))
	}
	args.RequestID, args.Cwd = ops.NewUUIDv7(), "/tmp/x"
	e.mustWrite(ops.OpSessionCreate, args)
	if e.host.createCwd != "/tmp/x" {
		t.Fatalf("cwd = %q", e.host.createCwd)
	}
}

func TestSetTask(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	set := func(task string) ops.WriteResult {
		return e.mustWrite(ops.OpSetTask, ops.SetTaskArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Task: task})
	}
	if res := set("review PR 12"); res.Outcome != ops.OutcomeDelivered || res.Pane == nil || res.Pane.Session != "work" {
		t.Fatalf("result = %+v", res)
	}
	if got := e.host.options["work|@minion_task"]; got != "review PR 12" {
		t.Fatalf("option = %q", got)
	}
	set("")
	if got := e.host.options["work|@minion_task"]; got != "" {
		t.Fatalf("cleared option = %q", got)
	}
	_, err := e.writeAs(ops.OpSetTask, messageOnly, ops.SetTaskArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Task: "x"})
	wantAPIError(t, err, "forbidden", "")
}

var dialogChoices = []agentdetect.Choice{
	{Label: "Yes", Keys: []string{"1"}},
	{Label: "Yes, and don't ask again", Keys: []string{"2"}},
	{Label: "No", Keys: []string{"3"}},
}

// answerArgs binds the answer to the dialog the pane shows now, as a caller
// that has just taken a snapshot does (see reviewedOf).
func (e *env) answerArgs(choice string, force bool) ops.AnswerArgs {
	e.t.Helper()
	var exp ops.ReviewedDialog
	if err := json.Unmarshal(mustJSON(e.t, e.reviewedOf("work")), &exp); err != nil {
		e.t.Fatal(err)
	}
	return ops.AnswerArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: force}, Target: target("work"), Choice: choice, Expect: exp}
}

func TestAnswerSendsTheChosenOptionsKeys(t *testing.T) {
	cases := map[string][]string{
		"Yes": {"1"}, "yes": {"1"}, "NO": {"3"}, "2": {"2"}, "3": {"3"},
		"Yes, and don't ask again": {"2"},
	}
	for choice, want := range cases {
		e := newEnv(t)
		e.host.addSession("work", "claude")
		e.cls.set("claude", "agent:claude", ops.PaneStateDialog, dialogChoices...)
		res := e.mustWrite(ops.OpAnswer, e.answerArgs(choice, false))
		if res.Outcome != ops.OutcomeDelivered || res.Forced {
			t.Fatalf("%q: %+v", choice, res)
		}
		if got := e.host.keys(); len(got) != 1 || strings.Join(got[0], ",") != strings.Join(want, ",") {
			t.Fatalf("%q sent %v, want %v", choice, got, want)
		}
	}
}

func TestAnswerSendsMultiKeyOptions(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "opencode")
	e.cls.set("opencode", "agent:opencode", ops.PaneStateDialog,
		agentdetect.Choice{Label: "Allow once", Keys: []string{"Enter"}},
		agentdetect.Choice{Label: "Allow always", Keys: []string{"Right", "Enter"}})
	e.mustWrite(ops.OpAnswer, e.answerArgs("allow always", false))
	if got := e.host.keys(); len(got) != 1 || strings.Join(got[0], ",") != "Right,Enter" {
		t.Fatalf("keys = %v", got)
	}
	e2 := newEnv(t)
	e2.host.addSession("work", "aider")
	e2.cls.set("aider", "agent:aider", ops.PaneStateDialog, agentdetect.Choice{Label: "Yes", Keys: []string{"y", "Enter"}}, agentdetect.Choice{Label: "No", Keys: []string{"n", "Enter"}})
	e2.mustWrite(ops.OpAnswer, e2.answerArgs("n", false)) // by the key letter
	if got := e2.host.keys(); len(got) != 1 || strings.Join(got[0], ",") != "n,Enter" {
		t.Fatalf("keys = %v", got)
	}
}

// An answer needs the dialog it was reviewed against. Once the dialog is gone
// nothing is sent, and force does not change that: the binding is KEEP, not
// FORCE (there is no forced literal key into a pane without a dialog any
// more).
func TestAnswerWithoutADialogIsRefusedAndNotForceable(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateDialog, dialogChoices...)
	reviewed := e.answerArgs("1", false)
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)

	for _, force := range []bool{false, true} {
		a := reviewed
		a.RequestID, a.Force = ops.NewUUIDv7(), force
		res := e.mustWrite(ops.OpAnswer, a)
		if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "dialog_changed" || res.Forceable || res.Forced || len(e.host.keys()) != 0 {
			t.Fatalf("force=%v: refusal = %+v keys = %v", force, res, e.host.keys())
		}
	}
}

// A dialog whose options could not be parsed takes a single character as the
// key; a word cannot be mapped to keys: invalid argument, and nothing is
// cached or sent.
func TestAnswerToADialogWithoutParsedOptions(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateDialog)

	res := e.mustWrite(ops.OpAnswer, e.answerArgs("y", false))
	if res.Outcome != ops.OutcomeDelivered || res.Forced || res.Forceable {
		t.Fatalf("answer = %+v", res)
	}
	if got := e.host.keys(); len(got) != 1 || got[0][0] != "y" {
		t.Fatalf("keys = %v", got)
	}
	_, err := e.write(ops.OpAnswer, e.answerArgs("yes", false))
	wantAPIError(t, err, "invalid_argument", "")
	if len(e.host.keys()) != 1 {
		t.Fatal("an unmappable answer sent keys")
	}
}

func TestAnswerWithAnUnknownChoiceIsAnArgumentError(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateDialog, dialogChoices...)
	for _, c := range []string{"maybe", "0", "4"} {
		_, err := e.write(ops.OpAnswer, e.answerArgs(c, false))
		wantAPIError(t, err, "invalid_argument", "")
	}
	if len(e.host.keys()) != 0 {
		t.Fatal("keys sent for an unknown choice")
	}
}

func TestAnswerMessageOnlyCallerIsForbidden(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateDialog, dialogChoices...)
	_, err := e.writeAs(ops.OpAnswer, messageOnly, e.answerArgs("1", true))
	wantAPIError(t, err, "forbidden", "")
}

// --- audit -------------------------------------------------------------------

func TestEveryWriteWritesOneAuditRowWithoutContent(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateBusy)

	refused := sendText("work", secretText, false)
	e.mustWrite(ops.OpSendText, refused)
	forced := sendText("work", secretText, true)
	e.mustWrite(ops.OpSendText, forced)
	keys := sendKeys("work", true, "Enter")
	e.mustWrite(ops.OpSendKeys, keys)
	task := ops.SetTaskArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Task: secretText}
	e.mustWrite(ops.OpSetTask, task)
	create := ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}}
	e.mustWrite(ops.OpSessionCreate, create)
	_, _ = e.write(ops.OpSendText, sendText("ghost", secretText, false)) // an API error is audited too
	e.policy.SetPreviewOnly(true)
	_, _ = e.write(ops.OpSendText, sendText("work", secretText, false))

	rows := e.audit()
	if len(rows) != 7 {
		t.Fatalf("audit rows = %d: %+v", len(rows), rows)
	}
	byReq := map[string]store.AuditRecord{}
	for _, r := range rows {
		byReq[r.RequestID] = r
		if r.TS.IsZero() || r.Op == "" || r.RequestID == "" {
			t.Fatalf("incomplete row %+v", r)
		}
	}
	want := []struct {
		id      string
		op      string
		target  string
		outcome string
		reason  string
		forced  bool
	}{
		{refused.RequestID, "send_text", ":work", "failed_not_sent", "busy", false},
		{forced.RequestID, "send_text", ":work", "delivered", "", true},
		{keys.RequestID, "send_keys", ":work", "delivered", "", true},
		{task.RequestID, "set_task", ":work", "delivered", "", false},
		{create.RequestID, "session_create", ":minion-1", "delivered", "", false},
	}
	for _, w := range want {
		r, ok := byReq[w.id]
		if !ok || r.Op != w.op || r.Target != w.target || r.Outcome != w.outcome || r.Reason != w.reason || r.Forced != w.forced {
			t.Errorf("row for %s = %+v, want %+v", w.op, r, w)
		}
	}
	var errRows []string
	for _, r := range rows {
		if r.Outcome == "error" {
			errRows = append(errRows, r.Reason)
		}
	}
	if strings.Join(errRows, ",") != "preview_only,target_not_found" {
		t.Fatalf("error rows = %v", errRows)
	}

	// No content anywhere: not in the audit, not in the result cache (read
	// through the store API, AD-16).
	dump := fmt.Sprintf("%+v", rows)
	if strings.Contains(dump, secretText) {
		t.Fatalf("content leaked into the audit: %s", dump)
	}
	for name, id := range map[string]string{
		"refused": refused.RequestID, "forced": forced.RequestID, "keys": keys.RequestID,
		"task": task.RequestID, "create": create.RequestID,
	} {
		rec, ok, err := e.st.GetResult(context.Background(), id)
		if err != nil || !ok {
			t.Fatalf("%s: cached result missing (ok %v, err %v)", name, ok, err)
		}
		if len(rec.Result) == 0 || strings.Contains(string(rec.Result), secretText) {
			t.Fatalf("%s: cached result %q is empty or holds content", name, rec.Result)
		}
	}
}

// --- dispatcher ---------------------------------------------------------------

func TestDispatcher(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)

	_, err := e.call("no_such_op", allActions, nil)
	wantAPIError(t, err, "unsupported_op", "")
	// Declared in the catalog but not an executor op: the pipe reads are
	// served by the local pipe and never executed for the Hub (Story 7.3).
	for _, op := range []string{ops.OpPipeRead, ops.OpPipeWait} {
		_, err = e.call(op, allActions, nil)
		wantAPIError(t, err, "unsupported_op", "")
	}

	bad := map[string]string{
		"not json":         `{`,
		"bad request id":   `{"request_id":"nope","target":{"session_name":"work"},"text":"x"}`,
		"two values":       `{"request_id":"` + ops.NewUUIDv7() + `","target":{"session_name":"work"},"text":"x"} {}`,
		"bad session name": `{"request_id":"` + ops.NewUUIDv7() + `","target":{"session_name":"a:b"},"text":"x"}`,
		"wrong field type": `{"request_id":"` + ops.NewUUIDv7() + `","target":{"session_name":"work"},"text":5}`,
	}
	for name, raw := range bad {
		_, err := e.ex.Execute(context.Background(), Request{Op: ops.OpSendText, Actions: allActions, Args: json.RawMessage(raw)})
		if ae := apiErrorOf(t, err, "invalid_argument", ""); strings.Contains(ae.Message, "bogus") {
			t.Errorf("%s: %v", name, ae)
		}
	}
	if n := len(e.audit()); n != 0 {
		t.Fatalf("invalid requests wrote %d audit rows", n)
	}

	// An argument this Worker does not know is ignored, not refused: a minor
	// change of an op only adds optional fields (AD-4).
	extra := `{"request_id":"` + ops.NewUUIDv7() + `","target":{"session_name":"work"},"text":"extra-field","bogus":1}`
	xraw, err := e.ex.Execute(context.Background(), Request{Op: ops.OpSendText, Actions: allActions, Args: json.RawMessage(extra)})
	if err != nil || !strings.Contains(string(xraw), `"outcome":"delivered"`) {
		t.Fatalf("a request with an unknown field: %s, %v", xraw, err)
	}
	if ps := e.host.pastes(); len(ps) != 1 || ps[0].Text != "extra-field" {
		t.Fatalf("pastes = %+v", ps)
	}

	raw, err := e.call(ops.OpSendText, allActions, sendText("work", "x", false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"warnings":[]`) || !strings.Contains(string(raw), `"outcome":"delivered"`) {
		t.Fatalf("result JSON = %s", raw)
	}
}

func TestHandleAddsAndReplacesOps(t *testing.T) {
	e := newEnv(t)
	e.ex.Handle("share_get", func(ctx context.Context, rq Request) (any, error) {
		return map[string]string{"op": rq.Op}, nil
	})
	raw, err := e.ex.Execute(context.Background(), Request{Op: "share_get"})
	if err != nil || string(raw) != `{"op":"share_get"}` {
		t.Fatalf("raw %s err %v", raw, err)
	}
}

func TestAPIErrorShape(t *testing.T) {
	inner := errors.New("tmux said no")
	err := fmt.Errorf("wrapped: %w", &APIError{Code: proto.CodeForbidden, Reason: proto.CodePreviewOnly, Message: "m", Err: inner})
	ae, ok := AsAPIError(err)
	if !ok || ae.Code != proto.CodeForbidden || !errors.Is(err, inner) {
		t.Fatalf("AsAPIError = %v, %v", ae, ok)
	}
	if got := ae.Error(); got != "forbidden (preview_only): m: tmux said no" {
		t.Fatalf("Error() = %q", got)
	}
	if _, ok := AsAPIError(errors.New("x")); ok {
		t.Fatal("a plain error is not an API error")
	}
}

// --- supervisor and review fixes -----------------------------------------------

func TestDuplicateWhileTheOriginalRunsIsUnknownNeverNotSent(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.LockWait = 80 * time.Millisecond })
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)

	entered, release := make(chan struct{}), make(chan struct{})
	e.host.onPaste = func() {
		select {
		case entered <- struct{}{}:
			<-release
		default:
		}
	}
	args := sendText("work", "first", false)
	done := make(chan ops.WriteResult, 1)
	go func() { done <- e.mustWriteQuiet(ops.OpSendText, args) }()
	<-entered

	// The duplicate waits past LockWait while the first paste is blocked: the
	// input may still go out, so "not sent" would be a lie.
	dup := e.mustWrite(ops.OpSendText, args)
	if dup.Outcome != ops.OutcomeUnknown || dup.Reason != "busy" || dup.Forceable {
		t.Fatalf("duplicate = %+v, want unknown/busy", dup)
	}
	close(release)
	if res := <-done; res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("original = %+v", res)
	}
	// The unknown answer was not cached: the original's result is.
	if again := e.mustWrite(ops.OpSendText, args); again.Outcome != ops.OutcomeDelivered {
		t.Fatalf("after the original finished = %+v", again)
	}
	if n := len(e.host.pastes()); n != 1 {
		t.Fatalf("pastes = %d, want 1", n)
	}
	// Both calls that ran wrote their own audit row.
	var outcomes []string
	for _, r := range e.audit() {
		if r.RequestID == args.RequestID {
			outcomes = append(outcomes, r.Outcome+"/"+r.Reason)
		}
	}
	sort.Strings(outcomes)
	if strings.Join(outcomes, ",") != "delivered/,unknown/busy" {
		t.Fatalf("audit rows of the request = %v", outcomes)
	}
	if n := e.ex.Locks().held(); n != 0 {
		t.Fatalf("%d lock entries leaked", n)
	}
}

func TestSendKeysGenericErrorWithoutAKeyIsUnknown(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	// keysAfter stays -1: no key was counted, yet a timed-out key may have gone.
	e.host.keysErr = errors.New("tmux: exit status 1")
	res := e.mustWrite(ops.OpSendKeys, sendKeys("work", false, "a", "b"))
	if res.Outcome != ops.OutcomeUnknown || res.Reason != "internal" {
		t.Fatalf("result = %+v, want unknown/internal", res)
	}
	e.host.keysErr = context.DeadlineExceeded
	res = e.mustWrite(ops.OpSendKeys, sendKeys("work", false, "a", "b"))
	if res.Outcome != ops.OutcomeUnknown {
		t.Fatalf("deadline result = %+v, want unknown", res)
	}
}

func TestAdapterArgumentErrorsAreAPIErrorsAndNotCached(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	bad := fmt.Errorf("text too large: %w", ops.ErrInvalidArgument)

	text := sendText("work", "x", false)
	e.host.pasteErr = bad
	_, err := e.write(ops.OpSendText, text)
	wantAPIError(t, err, "invalid_argument", "")

	keys := sendKeys("work", false, "Enter")
	e.host.keysErr = bad
	_, err = e.write(ops.OpSendKeys, keys)
	wantAPIError(t, err, "invalid_argument", "")

	// Nothing was cached: the same ids work once the host accepts them.
	e.host.pasteErr, e.host.keysErr = nil, nil
	if res := e.mustWrite(ops.OpSendText, text); res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("retry = %+v", res)
	}
	if res := e.mustWrite(ops.OpSendKeys, keys); res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("retry = %+v", res)
	}
	var errRows int
	for _, r := range e.audit() {
		if r.Outcome == "error" && r.Reason == "invalid_argument" {
			errRows++
		}
	}
	if errRows != 2 {
		t.Fatalf("invalid_argument audit rows = %d, want 2", errRows)
	}
}

func TestPartialKeysWithAnArgumentErrorStayPartial(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	e.host.keysErr, e.host.keysAfter = fmt.Errorf("bad key: %w", ops.ErrInvalidArgument), 1
	res := e.mustWrite(ops.OpSendKeys, sendKeys("work", false, "a", "b"))
	if res.Outcome != ops.OutcomePartial {
		t.Fatalf("result = %+v, want partial", res)
	}
}

// failingStateEnv is an env whose State fails result writes on demand and a
// shared "claude" pane to write to.
func failingStateEnv(t *testing.T) (*env, *putFailState) {
	var ps *putFailState
	e := newEnv(t, func(c *Config) {
		ps = &putFailState{State: c.State}
		c.State = ps
	})
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	return e, ps
}

func TestResultStoreFailureAfterDeliveryStillDeduplicates(t *testing.T) {
	e, ps := failingStateEnv(t)
	ps.failReplace.Store(true)

	args := sendText("work", "once", false)
	first := e.mustWrite(ops.OpSendText, args)
	second := e.mustWrite(ops.OpSendText, args)
	if first.Outcome != ops.OutcomeDelivered || string(mustJSON(t, first)) != string(mustJSON(t, second)) {
		t.Fatalf("first %+v second %+v", first, second)
	}
	if n := len(e.host.pastes()); n != 1 {
		t.Fatalf("pastes = %d, want 1: a duplicate sent again after the cache write failed", n)
	}
	// Once the memory entry is gone the database still answers, as unknown.
	e.ex.memMu.Lock()
	e.ex.mem = nil
	e.ex.memMu.Unlock()
	ps.failReplace.Store(false)
	third := e.mustWrite(ops.OpSendText, args)
	if third.Outcome != ops.OutcomeUnknown || len(e.host.pastes()) != 1 {
		t.Fatalf("third %+v, pastes %d", third, len(e.host.pastes()))
	}
}

// TestResultStoreFailureAfterDeliverySurvivesARestart: the delivery went out,
// the completed result could not be stored and the Worker restarted, so the
// memory entry is gone. Even after the storage recovered, the request id must
// not deliver again.
func TestResultStoreFailureAfterDeliverySurvivesARestart(t *testing.T) {
	e, ps := failingStateEnv(t)
	ps.failReplace.Store(true)
	args := sendText("work", "once", false)
	if first := e.mustWrite(ops.OpSendText, args); first.Outcome != ops.OutcomeDelivered {
		t.Fatalf("first = %+v", first)
	}

	// A new executor over the same database and the same tmux session.
	restarted, err := NewExecutor(Config{Host: e.host, Classifier: e.cls, State: e.st, Policy: e.policy, LockWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	e.ex = restarted
	ps.failReplace.Store(false)
	for i := 0; i < 2; i++ {
		again := e.mustWrite(ops.OpSendText, args)
		if again.Outcome != ops.OutcomeUnknown || again.Reason != "internal" {
			t.Fatalf("duplicate %d after the restart = %+v, want unknown", i, again)
		}
	}
	if n := len(e.host.pastes()); n != 1 {
		t.Fatalf("pastes = %d, want 1: the request was delivered again after the restart", n)
	}
	// A different request id is not affected.
	other := sendText("work", "another", false)
	if res := e.mustWrite(ops.OpSendText, other); res.Outcome != ops.OutcomeDelivered || len(e.host.pastes()) != 2 {
		t.Fatalf("new request: %+v, pastes %d", res, len(e.host.pastes()))
	}
}

// TestRequestIsRecordedBeforeItRuns: when the record cannot be written the
// request is refused and has no effect.
func TestRequestIsRecordedBeforeItRuns(t *testing.T) {
	e, ps := failingStateEnv(t)
	ps.failPut.Store(true)
	args := sendText("work", "never", false)
	_, err := e.write(ops.OpSendText, args)
	wantAPIError(t, err, "internal", "")
	if n := len(e.host.pastes()); n != 0 {
		t.Fatalf("pastes = %d, want 0: the request ran without a record", n)
	}
	// The same request id can run once the storage is back.
	ps.failPut.Store(false)
	if res := e.mustWrite(ops.OpSendText, args); res.Outcome != ops.OutcomeDelivered || len(e.host.pastes()) != 1 {
		t.Fatalf("retry: %+v, pastes %d", res, len(e.host.pastes()))
	}
}

// TestFailedRequestLeavesNoRecord: an API error means nothing was sent, so the
// same request id may be repeated and runs.
func TestFailedRequestLeavesNoRecord(t *testing.T) {
	e, _ := failingStateEnv(t)
	args := sendText("late", "hello", false)
	_, err := e.write(ops.OpSendText, args)
	wantAPIError(t, err, "target_not_found", "")
	e.host.addSession("late", "claude")
	if res := e.mustWrite(ops.OpSendText, args); res.Outcome != ops.OutcomeDelivered || len(e.host.pastes()) != 1 {
		t.Fatalf("retry: %+v, pastes %d", res, len(e.host.pastes()))
	}
}

func TestSessionCreateFailureAfterCreationIsUnknownAndNeverRetriedUnderAnotherName(t *testing.T) {
	for _, name := range []string{"", "proj"} {
		e := newEnv(t)
		e.host.createErr, e.host.createAfter = errors.New("tmux: list-panes failed"), true
		args := ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Name: name}
		res := e.mustWrite(ops.OpSessionCreate, args)
		if res.Outcome != ops.OutcomeUnknown || res.Reason != "internal" || res.Pane == nil {
			t.Fatalf("name %q: result = %+v", name, res)
		}
		if len(e.host.sessions) != 1 || res.Pane.Session != e.host.sessions[0].Name {
			t.Fatalf("name %q: sessions = %+v, pane %+v", name, e.host.sessions, res.Pane)
		}
		// The request id keeps the answer; a duplicate starts nothing.
		e.host.createErr = nil
		again := e.mustWrite(ops.OpSessionCreate, args)
		if again.Outcome != ops.OutcomeUnknown || len(e.host.sessions) != 1 {
			t.Fatalf("name %q: duplicate = %+v, %d sessions", name, again, len(e.host.sessions))
		}
		rows := e.audit()
		if len(rows) != 1 || rows[0].Target != ":"+res.Pane.Session || rows[0].Outcome != "unknown" {
			t.Fatalf("name %q: audit = %+v", name, rows)
		}
	}
}

func TestSessionCreateInvalidArgumentIsAnAPIError(t *testing.T) {
	e := newEnv(t)
	e.host.createErr = fmt.Errorf("cwd: %w", ops.ErrInvalidArgument)
	_, err := e.write(ops.OpSessionCreate, ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}})
	wantAPIError(t, err, "invalid_argument", "")
	if len(e.host.sessions) != 0 {
		t.Fatalf("sessions = %d", len(e.host.sessions))
	}
}

// TestPaneWriteChecksPermissionBeforeTheForceGuards pins the order a pipe op
// relies on: for an op that accepts message and operate, the pane-kind
// dependent permission check comes before the FORCE guards, so force never
// turns a forbidden write into a delivered one (AD-12).
func TestPaneWriteChecksPermissionBeforeTheForceGuards(t *testing.T) {
	setup := func() *env {
		e := newEnv(t)
		e.ex.Handle(ops.OpPipeSend, func(ctx context.Context, rq Request) (any, error) {
			op, args, err := prepare[ops.SendTextArgs](rq)
			if err != nil {
				return nil, err
			}
			return e.ex.paneWrite(ctx, rq, op, args.WriteArgs, args.Target, true, inputGuards,
				func(ctx context.Context, p *pane) (ops.Outcome, proto.Code, error) {
					if err := e.host.PasteText(ctx, p.ref, args.Text, p.ref); err != nil {
						return "", "", err
					}
					return ops.OutcomeDelivered, "", nil
				})
		})
		return e
	}
	send := func(e *env, held []ops.Action, force bool) (ops.WriteResult, error) {
		return e.writeAs(ops.OpPipeSend, held, sendText("work", "hi", force))
	}

	for _, kind := range []string{ops.PaneKindShell, ops.PaneKindOther, ops.PaneKindUnknown} {
		for _, force := range []bool{false, true} {
			e := setup()
			e.host.addSession("work", "prog")
			e.cls.set("prog", kind, ops.PaneStateStable)
			_, err := send(e, messageOnly, force)
			wantAPIError(t, err, "forbidden", "")
			if n := len(e.host.pastes()); n != 0 {
				t.Fatalf("%s force=%v: %d pastes reached the pane", kind, force, n)
			}
		}
	}

	// A message-only caller reaches a stable agent pane.
	e := setup()
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	res, err := send(e, messageOnly, false)
	if err != nil || res.Outcome != ops.OutcomeDelivered || len(e.host.pastes()) != 1 {
		t.Fatalf("message to an agent: %+v, %v", res, err)
	}

	// An operate caller passes on a shell pane only with force.
	e = setup()
	e.host.addSession("work", "bash")
	e.cls.set("bash", ops.PaneKindShell, ops.PaneStateStable)
	res, err = send(e, operateActions, false)
	if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "shell_pane" || !res.Forceable {
		t.Fatalf("operate without force: %+v, %v", res, err)
	}
	res, err = send(e, operateActions, true)
	if err != nil || res.Outcome != ops.OutcomeDelivered || !res.Forced || len(e.host.pastes()) != 1 {
		t.Fatalf("operate with force: %+v, %v", res, err)
	}
}

// --- round-1 review fixes ----------------------------------------------------------

func TestSessionNamedAlreadyExistsThatExitedIsNotStartedAgain(t *testing.T) {
	e := newEnv(t)
	// The program started and exited before the readback; the adapter error
	// carries the session name, which says "already exists".
	args := ops.SessionCreateArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Name: "x-already exists"}
	e.host.createErr = errors.New("tmux: session \"x-already exists\": readback failed")
	e.host.createGone = true
	for i := 0; i < 2; i++ {
		res := e.mustWrite(ops.OpSessionCreate, args)
		if res.Outcome != ops.OutcomeUnknown {
			t.Fatalf("attempt %d: outcome %s, want unknown", i, res.Outcome)
		}
	}
	if e.host.creates != 1 {
		t.Fatalf("the program was started %d times", e.host.creates)
	}
}

func TestNameTakenIsTypedNotTextual(t *testing.T) {
	if !nameTaken(fmt.Errorf("session %q: %w", "a", tmux.ErrSessionExists), "a") {
		t.Fatal("tmux.ErrSessionExists must count")
	}
	if !nameTaken(fmt.Errorf("conpty: session %q already exists", "a"), "a") {
		t.Fatal("the ConPTY message must count")
	}
	if nameTaken(fmt.Errorf("list-panes of %q failed: already exists", "a"), "a") || nameTaken(nil, "a") {
		t.Fatal("a mere mention of \"already exists\" must not count")
	}
}

func TestSetTaskFailureAfterApplyingIsUnknownAndCached(t *testing.T) {
	st := &optionErrHost{fakeHost: newFakeHost(), err: context.DeadlineExceeded}
	e := newEnv(t, func(c *Config) { c.Host = st })
	st.addSession("work", "claude")
	args := ops.SetTaskArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Task: "first"}
	res := e.mustWrite(ops.OpSetTask, args)
	if res.Outcome != ops.OutcomeUnknown || res.Reason != "internal" {
		t.Fatalf("result = %+v, want unknown", res)
	}
	st.err = nil
	again := e.mustWrite(ops.OpSetTask, args)
	if again.Outcome != ops.OutcomeUnknown || st.calls != 1 {
		t.Fatalf("duplicate = %+v after %d SetOption calls", again, st.calls)
	}
	// A failure that proves nothing changed stays an API error and is not cached.
	st.err = fmt.Errorf("x: %w", term.ErrNotFound)
	other := ops.SetTaskArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Task: "t"}
	_, err := e.write(ops.OpSetTask, other)
	wantAPIError(t, err, "target_not_found", "")
	if _, ok, _ := e.st.GetResult(context.Background(), other.RequestID); ok {
		t.Fatal("an API error was cached")
	}
}

// optionErrHost fails SetOption with err (when set) and counts the calls.
type optionErrHost struct {
	*fakeHost
	err   error
	calls int
}

func (h *optionErrHost) SetOption(ctx context.Context, session, key, value string) error {
	h.calls++
	if h.err != nil {
		return h.err
	}
	return h.fakeHost.SetOption(ctx, session, key, value)
}

func TestQueuedWriteRechecksPolicyAfterTheLock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(e *env)
	}{
		{"preview only", func(e *env) { e.policy.SetPreviewOnly(true) }},
		{"share revoked", func(e *env) {
			if err := e.st.SaveShare(store.ShareSelection{Mode: store.ShareNone}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ref := e.host.addSession("work", "claude")
			e.cls.set("claude", "agent:claude", ops.PaneStateStable)
			unlock, err := e.ex.Locks().Lock(context.Background(), PaneKey(ref.ID), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := e.write(ops.OpSendText, sendText("work", "queued", false)); done <- err }()
			time.Sleep(100 * time.Millisecond) // the write now waits for the pane lock
			tc.change(e)
			unlock()
			wantAPIError(t, <-done, "forbidden", "preview_only")
			if len(e.host.pastes()) != 0 {
				t.Fatal("a queued write was delivered after the policy changed")
			}
		})
	}
}

func TestAuthorizationDeniedWriteIsAudited(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	args := ops.SetTaskArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Target: target("work"), Task: secretText}
	_, err := e.writeAs(ops.OpSetTask, []ops.Action{ops.ActionRead}, args)
	wantAPIError(t, err, "forbidden", "")
	rows := e.audit()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %+v", rows)
	}
	r := rows[0]
	if r.Op != "set_task" || r.Outcome != "error" || r.Reason != "forbidden" || r.RequestID != args.RequestID || r.Target != ":work" {
		t.Fatalf("row = %+v", r)
	}
	if strings.Contains(fmt.Sprintf("%+v", r), secretText) {
		t.Fatal("content in the audit")
	}
}
