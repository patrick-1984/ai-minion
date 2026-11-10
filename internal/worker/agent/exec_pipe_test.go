package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// --- fakes -------------------------------------------------------------------

// anchorHost is the fake host with the optional Anchorer port: a pane of 24
// rows without scrollback whose rows the test sets, read the way tmux 3.8
// reports them.
type anchorHost struct {
	*fakeHost
	am       sync.Mutex
	view     []string
	noCount  bool // tmux before 3.8: no row counters
	gen      int64
	markErr  error
	readErr  func(call int) error
	reads    int
	marks    int
	lastFrom int64
}

const fakeHeight = 24

func (h *anchorHost) setView(rows []string) {
	h.am.Lock()
	defer h.am.Unlock()
	h.view = make([]string, fakeHeight)
	copy(h.view, rows)
}

func (h *anchorHost) readCount() int {
	h.am.Lock()
	defer h.am.Unlock()
	return h.reads
}

func (h *anchorHost) evidence(p term.PaneRef) term.PaneEvidence {
	ev := term.PaneEvidence{
		PaneID: p.ID, Command: p.Command, PID: p.PID, Width: 100, Height: fakeHeight, HistoryLimit: 2000,
		Collected: 0, Generation: h.gen,
	}
	if h.noCount {
		ev.Collected, ev.Generation = -1, -1
	}
	return ev
}

func (h *anchorHost) PasteMarked(_ context.Context, p term.PaneRef, text string, _ term.PaneRef) (term.DeliveryMark, error) {
	for _, r := range text {
		if r < 0x20 && r != '\t' && r != '\n' {
			return term.DeliveryMark{}, fmt.Errorf("%w: control character", ops.ErrInvalidArgument)
		}
	}
	h.am.Lock()
	defer h.am.Unlock()
	if h.markErr != nil {
		return term.DeliveryMark{}, h.markErr
	}
	h.marks++
	h.mu.Lock()
	h.pasted = append(h.pasted, pasteCall{Pane: p.ID, Text: text})
	h.mu.Unlock()
	m := term.DeliveryMark{PaneEvidence: h.evidence(p), From: 0}
	if !h.noCount {
		m.Rows = append([]string(nil), h.view...)
	}
	return m, nil
}

func (h *anchorHost) ReadFrame(ctx context.Context, p term.PaneRef, from int64) (term.Frame, error) {
	h.am.Lock()
	defer h.am.Unlock()
	h.reads++
	h.lastFrom = from
	if h.readErr != nil {
		if err := h.readErr(h.reads); err != nil {
			return term.Frame{}, err
		}
	}
	f := term.Frame{PaneEvidence: h.evidence(p), From: from}
	if !h.noCount {
		f.Rows = append([]string(nil), h.view...)
	}
	return f, nil
}

var _ term.Anchorer = (*anchorHost)(nil)

// fakePipeCaller is the call client of the Hub mode.
type fakePipeCaller struct {
	mu     sync.Mutex
	calls  []fakePipeCall
	doFn   func(op string, args any) (json.RawMessage, error)
	stream func(op string, args any, emit func(json.RawMessage) error) error
}

type fakePipeCall struct {
	Op   string
	Args string
}

func (c *fakePipeCaller) record(op string, args any) {
	b, _ := json.Marshal(args)
	c.mu.Lock()
	c.calls = append(c.calls, fakePipeCall{op, string(b)})
	c.mu.Unlock()
}

func (c *fakePipeCaller) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *fakePipeCaller) Do(_ context.Context, op string, args, out any) error {
	c.record(op, args)
	raw, err := c.doFn(op, args)
	if err != nil {
		return err
	}
	if p, ok := out.(*json.RawMessage); ok {
		*p = raw
	}
	return nil
}

func (c *fakePipeCaller) Stream(_ context.Context, op string, args any, onElement func(json.RawMessage) error) error {
	c.record(op, args)
	return c.stream(op, args, onElement)
}

// fakeTurnSink is the turn emitter of the Hub mode.
type fakeTurnSink struct {
	mu    sync.Mutex
	turns []PendingTurn
	err   error
}

func (s *fakeTurnSink) Emit(t PendingTurn) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	s.turns = append(s.turns, t)
	return t.TurnID, nil
}

func (s *fakeTurnSink) got() []PendingTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PendingTurn(nil), s.turns...)
}

// fakePipeHub is the Hub seam of the pipe.
type fakePipeHub struct {
	mu        sync.Mutex
	enrolled  bool
	enrollErr error
	caller    *fakePipeCaller
	sink      *fakeTurnSink
}

func (h *fakePipeHub) Enrolled(context.Context) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.enrolled, h.enrollErr
}

func (h *fakePipeHub) Calls() PipeCaller {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.caller == nil {
		return nil
	}
	return h.caller
}

func (h *fakePipeHub) Turns() TurnSink {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sink == nil {
		return nil
	}
	return h.sink
}

func (h *fakePipeHub) setEnrolled(on bool) {
	h.mu.Lock()
	h.enrolled = on
	h.mu.Unlock()
}

// pipeEnv is an executor over the anchor host with the pipe service.
type pipeEnv struct {
	*env
	ah  *anchorHost
	svc *PipeService
	hub *fakePipeHub
	ref term.PaneRef
}

var localAll = []ops.Action{ops.ActionRead, ops.ActionOperate, ops.ActionMessage, ops.ActionManage}

func claudeView(content []string, status string) []string {
	chrome := []string{
		"",
		"╭──────────────────────────────────────────────────────────────────────╮",
		"│ >                                                                    │",
		"╰──────────────────────────────────────────────────────────────────────╯",
		"  ? for shortcuts",
	}
	if status != "" {
		chrome = append([]string{"", status}, chrome...)
	}
	return append(append([]string{}, content...), chrome...)
}

const claudeBusyLine = "✻ Thinking… (3s · ↑ 1.2k tokens · esc to interrupt)"

func newPipeEnv(t *testing.T, mut ...func(*PipeConfig)) *pipeEnv {
	t.Helper()
	ah := &anchorHost{fakeHost: newFakeHost()}
	e := newEnv(t, func(c *Config) { c.Host = ah })
	e.host = ah.fakeHost
	ref := ah.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	ah.setView(claudeView(nil, ""))
	hub := &fakePipeHub{}
	cfg := PipeConfig{
		Executor: e.ex, Store: e.st, Hub: hub, Now: e.clk.Now,
		Poll: 5 * time.Millisecond, Settle: 1, WaitPoll: 10 * time.Millisecond, MaxReadFailures: 4,
	}
	for _, m := range mut {
		m(&cfg)
	}
	svc, err := NewPipeService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	return &pipeEnv{env: e, ah: ah, svc: svc, hub: hub, ref: ref}
}

func pipeSendArgs(session, text string, force bool) ops.PipeSendArgs {
	return ops.PipeSendArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7(), Force: force}, Target: target(session), Text: text}
}

// hubSend is a Hub-origin pipe_send: the executor's own op.
func (p *pipeEnv) hubSend(actions []ops.Action, args ops.PipeSendArgs) (ops.WriteResult, error) {
	p.t.Helper()
	if args.MessageID == 0 {
		args.MessageID = 7 // the Hub always names the delivery (an old Hub, which does not, is tested on its own)
	}
	return p.writeAs(ops.OpPipeSend, actions, args)
}

// localCall is a call from this host: the local socket's entry.
func (p *pipeEnv) localCall(op string, args any) (json.RawMessage, error) {
	p.t.Helper()
	return p.localCallCtx(context.Background(), op, args)
}

func (p *pipeEnv) localCallCtx(ctx context.Context, op string, args any) (json.RawMessage, error) {
	p.t.Helper()
	var raw json.RawMessage
	if args != nil {
		raw = mustJSON(p.t, args)
	}
	return p.svc.Local(ctx, Request{Op: op, Actions: localAll, Args: raw})
}

func (p *pipeEnv) localSend(args ops.PipeSendArgs) (ops.WriteResult, error) {
	p.t.Helper()
	raw, err := p.localCall(ops.OpPipeSend, args)
	if err != nil {
		return ops.WriteResult{}, err
	}
	var res ops.WriteResult
	if err := json.Unmarshal(raw, &res); err != nil {
		p.t.Fatalf("result %s: %v", raw, err)
	}
	if err := res.Validate(); err != nil {
		p.t.Fatalf("result %s breaks the action contract: %v", raw, err)
	}
	return res, nil
}

func (p *pipeEnv) read(after string) ops.PipeReadResult {
	p.t.Helper()
	raw, err := p.localCall(ops.OpPipeRead, ops.PipeReadArgs{Target: target("work"), AfterTurnID: after})
	if err != nil {
		p.t.Fatalf("pipe_read: %v", err)
	}
	var res ops.PipeReadResult
	if err := json.Unmarshal(raw, &res); err != nil {
		p.t.Fatal(err)
	}
	return res
}

func (p *pipeEnv) wait(after string, timeout time.Duration) ([]ops.PipeTurn, error) {
	p.t.Helper()
	var mu sync.Mutex
	var got []ops.PipeTurn
	ctx := WithProgress(context.Background(), func(m json.RawMessage) {
		var pt ops.PipeTurn
		if err := json.Unmarshal(m, &pt); err != nil {
			p.t.Errorf("progress %s: %v", m, err)
		}
		mu.Lock()
		got = append(got, pt)
		mu.Unlock()
	})
	_, err := p.localCallCtx(ctx, ops.OpPipeWait, ops.PipeWaitArgs{Target: target("work"), AfterTurnID: after, TimeoutMS: int(timeout / time.Millisecond)})
	mu.Lock()
	defer mu.Unlock()
	return got, err
}

func pipeEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (p *pipeEnv) pipeEntries() []store.PipeEntry {
	p.t.Helper()
	es, err := p.st.Entries(context.Background(), soloPeer("work"), "", 0)
	if err != nil {
		p.t.Fatal(err)
	}
	return es
}

// --- registration ------------------------------------------------------------

func TestPipeSendIsAnnouncedByTheExecutorFromTheStart(t *testing.T) {
	e := newEnv(t) // no pipe module: only the executor
	got := e.ex.Ops()
	has := func(op string) bool {
		for _, o := range got {
			if o == op {
				return true
			}
		}
		return false
	}
	if !has(ops.OpPipeSend) {
		t.Fatalf("pipe_send is not announced: %v", got)
	}
	if has(ops.OpPipeRead) || has(ops.OpPipeWait) {
		t.Fatalf("pipe_read and pipe_wait are never executed for the Hub, yet announced: %v", got)
	}
}

func TestPipeModuleRegistersBeforeTheSocketAndTheConnector(t *testing.T) {
	if OrderPipe <= OrderCore || OrderPipe >= OrderLocalAPI || OrderPipe >= OrderConnector {
		t.Fatalf("pipe order %d must be between core %d and local socket %d / connector %d", OrderPipe, OrderCore, OrderLocalAPI, OrderConnector)
	}
	found := false
	for _, n := range DefaultRegistry.Names() {
		found = found || n == ModulePipe
	}
	if !found {
		t.Fatalf("module %q is not registered: %v", ModulePipe, DefaultRegistry.Names())
	}
}

// --- AC: delivery and guards (Hub-origin write, the executor's op) -----------

func TestPipeSendStableAgentIsPastedWithEnterAndDelivered(t *testing.T) {
	p := newPipeEnv(t)
	res, err := p.hubSend(allActions, pipeSendArgs("work", "hello agent", false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ops.OutcomeDelivered || res.Forced || res.Pane == nil || res.Pane.Kind != "agent:claude" {
		t.Fatalf("result = %+v pane %+v", res, res.Pane)
	}
	if got := p.host.pastes(); len(got) != 1 || got[0].Text != "hello agent" {
		t.Fatalf("pastes = %+v", got)
	}
	if keys := p.host.keys(); len(keys) != 1 || len(keys[0]) != 1 || keys[0][0] != "Enter" {
		t.Fatalf("keys = %v, want one Enter after the paste", keys)
	}
	if p.ah.marks != 1 {
		t.Fatalf("the message must be pasted through the Anchorer once, got %d", p.ah.marks)
	}
}

func TestPipeSendBusyOrDialogIsFailedNotSentForceableAndNothingIsQueued(t *testing.T) {
	for _, tc := range []struct{ state, reason string }{
		{ops.PaneStateBusy, "busy"},
		{ops.PaneStateDialog, "dialog_open"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			p := newPipeEnv(t)
			p.cls.set("claude", "agent:claude", tc.state)
			res, err := p.hubSend(allActions, pipeSendArgs("work", "are you there", false))
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != tc.reason || !res.Forceable {
				t.Fatalf("result = %+v", res)
			}
			if len(p.host.pastes()) != 0 || len(p.host.keys()) != 0 {
				t.Fatalf("a refused message reached the pane: %v %v", p.host.pastes(), p.host.keys())
			}
			// Nothing is queued: once the agent is stable the message is not sent
			// by itself, and the Worker keeps nothing of it.
			p.cls.set("claude", "agent:claude", ops.PaneStateStable)
			time.Sleep(50 * time.Millisecond)
			if len(p.host.pastes()) != 0 || len(p.pipeEntries()) != 0 {
				t.Fatalf("a refused message was queued: %v / %v", p.host.pastes(), p.pipeEntries())
			}
			// FORCE: a new request with force goes through.
			p.cls.set("claude", "agent:claude", tc.state)
			forced, err := p.hubSend(allActions, pipeSendArgs("work", "are you there", true))
			if err != nil || forced.Outcome != ops.OutcomeDelivered || !forced.Forced {
				t.Fatalf("forced = %+v, %v", forced, err)
			}
		})
	}
}

func TestPipeSendShellNeedsOperateAndIsForceGuarded(t *testing.T) {
	p := newPipeEnv(t)
	p.ah.addSession("sh", "bash")
	p.cls.set("bash", ops.PaneKindShell, ops.PaneStateStable)

	// message alone: forbidden, an API error and never an outcome.
	_, err := p.hubSend(messageOnly, pipeSendArgs("sh", "ls", true))
	wantAPIError(t, err, "forbidden", "")
	if len(p.host.pastes()) != 0 {
		t.Fatal("a message-only caller reached a shell")
	}
	// operate: FORCE-guarded.
	res, err := p.hubSend(operateActions, pipeSendArgs("sh", "ls", false))
	if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "shell_pane" || !res.Forceable {
		t.Fatalf("operate without force = %+v, %v", res, err)
	}
	res, err = p.hubSend(operateActions, pipeSendArgs("sh", "ls", true))
	if err != nil || res.Outcome != ops.OutcomeDelivered || !res.Forced {
		t.Fatalf("operate with force = %+v, %v", res, err)
	}
	// A message-only caller still delivers to an agent pane.
	if res, err := p.hubSend(messageOnly, pipeSendArgs("work", "hi", false)); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("message to an agent = %+v, %v", res, err)
	}
}

func TestPipeSendPreviewOnlyAndUnsharedRefuseLikeEveryWrite(t *testing.T) {
	p := newPipeEnv(t)
	p.policy.SetPreviewOnly(true)
	_, err := p.hubSend(allActions, pipeSendArgs("work", "x", false))
	wantAPIError(t, err, "forbidden", "preview_only")
	// preview-only also drops the caller's actions at the op server; the
	// executor refuses it itself.
	_, err = p.localSend(pipeSendArgs("work", "x", false))
	wantAPIError(t, err, "forbidden", "preview_only")
	p.policy.SetPreviewOnly(false)

	if err := p.st.SaveShare(store.ShareSelection{Mode: store.ShareNone}); err != nil {
		t.Fatal(err)
	}
	_, err = p.hubSend(allActions, pipeSendArgs("work", "x", false))
	wantAPIError(t, err, "forbidden", "preview_only")
	if len(p.host.pastes()) != 0 || len(p.pipeEntries()) != 0 {
		t.Fatal("a refused pipe_send had an effect")
	}
}

func TestPipeSendRequestIDIsAnsweredFromTheCacheWithoutASecondWrite(t *testing.T) {
	p := newPipeEnv(t)
	args := pipeSendArgs("work", "once", false)
	first, err := p.hubSend(allActions, args)
	if err != nil || first.Outcome != ops.OutcomeDelivered {
		t.Fatalf("first = %+v, %v", first, err)
	}
	second, err := p.hubSend(allActions, args)
	if err != nil || second.Outcome != ops.OutcomeDelivered {
		t.Fatalf("second = %+v, %v", second, err)
	}
	if len(p.host.pastes()) != 1 || len(p.host.keys()) != 1 {
		t.Fatalf("a duplicate request id wrote again: %v %v", p.host.pastes(), p.host.keys())
	}
	other := args
	other.Text = "different"
	_, err = p.hubSend(allActions, other)
	if err != nil { // same id, same op and session: the cache answers
		t.Fatalf("same id with other text: %v", err)
	}
	if len(p.host.pastes()) != 1 {
		t.Fatal("the cache must answer a repeated request id whatever its text")
	}
}

func TestPipeSendFailuresAreOutcomesAndNeverRepeatTheWrite(t *testing.T) {
	t.Run("paste fails: nobody can tell", func(t *testing.T) {
		p := newPipeEnv(t)
		p.ah.markErr = errors.New("tmux timed out")
		res, err := p.hubSend(allActions, pipeSendArgs("work", "x", false))
		if err != nil || res.Outcome != ops.OutcomeUnknown {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if len(p.host.keys()) != 0 {
			t.Fatal("Enter was sent after an unconfirmed paste")
		}
	})
	t.Run("pane changed at the paste", func(t *testing.T) {
		p := newPipeEnv(t)
		p.ah.markErr = term.ErrPaneChanged
		res, err := p.hubSend(allActions, pipeSendArgs("work", "x", false))
		if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "pane_changed" {
			t.Fatalf("result = %+v, %v", res, err)
		}
	})
	t.Run("control characters are refused before anything is sent", func(t *testing.T) {
		p := newPipeEnv(t)
		_, err := p.hubSend(allActions, pipeSendArgs("work", "bad\x1b[31m", false))
		wantAPIError(t, err, "invalid_argument", "")
		if len(p.host.pastes()) != 0 || len(p.host.keys()) != 0 {
			t.Fatal("a refused message reached the pane")
		}
	})
	t.Run("Enter fails after the paste: partial, no resend", func(t *testing.T) {
		p := newPipeEnv(t)
		p.host.keysErr, p.host.keysAfter = term.ErrPaneChanged, 0
		res, err := p.hubSend(allActions, pipeSendArgs("work", "x", false))
		if err != nil || res.Outcome != ops.OutcomePartial || res.Reason != "pane_changed" {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if len(p.host.pastes()) != 1 {
			t.Fatalf("pastes = %v", p.host.pastes())
		}
		time.Sleep(30 * time.Millisecond)
		if p.ah.readCount() != 0 {
			t.Fatal("a message that was not submitted is followed")
		}
	})
	t.Run("Enter with an unclear error is unknown", func(t *testing.T) {
		p := newPipeEnv(t)
		p.host.keysErr, p.host.keysAfter = errors.New("boom"), 0
		res, err := p.hubSend(allActions, pipeSendArgs("work", "x", false))
		if err != nil || res.Outcome != ops.OutcomeUnknown {
			t.Fatalf("result = %+v, %v", res, err)
		}
	})
}

// --- AC: old tmux and hosts without evidence ---------------------------------

func TestPipeSendOnOldTmuxIsDeliveredWithAnExplicitReasonAndNoTurn(t *testing.T) {
	p := newPipeEnv(t)
	p.ah.noCount = true
	res, err := p.hubSend(allActions, pipeSendArgs("work", "hello", false))
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %+v, %v", res, err)
	}
	var w *ops.Warning
	for i := range res.Warnings {
		if res.Warnings[i].Code == "turn_unavailable" {
			w = &res.Warnings[i]
		}
	}
	if w == nil || !strings.Contains(w.Message, "tmux_too_old") {
		t.Fatalf("warnings = %+v, want turn_unavailable naming tmux_too_old", res.Warnings)
	}
	time.Sleep(40 * time.Millisecond)
	if p.ah.readCount() != 0 {
		t.Fatalf("frames were read for a delivery that cannot give a turn: %d", p.ah.readCount())
	}
	if len(p.host.pastes()) != 1 || len(p.host.keys()) != 1 {
		t.Fatalf("the message must be written exactly once: %v %v", p.host.pastes(), p.host.keys())
	}
}

func TestPipeSendOnAHostWithoutAnAnchorerPastesTextAndSaysNoEvidence(t *testing.T) {
	e := newEnv(t) // a plain fake host: no Anchorer, like the ConPTY host today
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	args := pipeSendArgs("work", "hello", false)
	args.MessageID = 7
	res, err := e.writeAs(ops.OpPipeSend, allActions, args)
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(res.Warnings) == 0 || !strings.Contains(res.Warnings[len(res.Warnings)-1].Message, "no_evidence") {
		t.Fatalf("warnings = %+v", res.Warnings)
	}
	if len(e.host.pastes()) != 1 {
		t.Fatalf("pastes = %v", e.host.pastes())
	}
}

// --- Hub-origin writes never reach the broker --------------------------------

func TestHubOriginWriteDoesLocalDeliveryOnlyAndNeverTouchesBrokerStoreOrCallClient(t *testing.T) {
	p := newPipeEnv(t)
	p.hub.setEnrolled(true)
	p.hub.caller = &fakePipeCaller{doFn: func(string, any) (json.RawMessage, error) {
		t.Error("a Hub-origin write called the CallClient")
		return nil, errors.New("must not be called")
	}}
	p.hub.sink = &fakeTurnSink{}

	res, err := p.hubSend(allActions, pipeSendArgs("work", "from the hub", false))
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if p.hub.caller.count() != 0 {
		t.Fatal("call client used")
	}
	if len(p.pipeEntries()) != 0 {
		t.Fatalf("the Worker stored a message the Hub already stored: %+v", p.pipeEntries())
	}
	n, err := p.st.CountMessagesSince(context.Background(), soloPeer("work"), p.clk.Now().Add(-time.Hour))
	if err != nil || n != 0 {
		t.Fatalf("budget used on the Worker: %d, %v", n, err)
	}
	// The reply goes to the Hub, through the turn emitter, and not to the Worker store.
	p.ah.setView(claudeView([]string{"> from the hub", "", "● pong"}, ""))
	pipeEventually(t, "the reply to reach the turn emitter", func() bool { return len(p.hub.sink.got()) == 1 })
	got := p.hub.sink.got()[0]
	if got.Session != "work" || got.Text != "pong" || !pipe.IsTurnID(got.TurnID) {
		t.Fatalf("emitted %+v", got)
	}
	time.Sleep(30 * time.Millisecond)
	if len(p.pipeEntries()) != 0 {
		t.Fatalf("a Hub reply was also stored on the Worker: %+v", p.pipeEntries())
	}
}

func TestHubOriginRequestsCannotReachThePipeReadsOrTheLocalEntry(t *testing.T) {
	p := newPipeEnv(t)
	for _, op := range []string{ops.OpPipeRead, ops.OpPipeWait} {
		_, err := p.call(op, allActions, map[string]any{"target": target("work")})
		wantAPIError(t, err, "unsupported_op", "")
	}
	// JSON cannot claim a local origin: whatever the arguments hold, the
	// executor's pipe_send is the Hub's write.
	forged := map[string]any{
		"request_id": ops.NewUUIDv7(), "target": target("work"), "text": "x", "message_id": 7,
		"origin": "local", "local": true, "caller": "local_socket",
	}
	p.hub.setEnrolled(false)
	res, err := p.writeAs(ops.OpPipeSend, allActions, forged)
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(p.pipeEntries()) != 0 {
		t.Fatal("a forged origin reached the Solo broker")
	}
}

// --- Solo broker (local entry) ----------------------------------------------

func TestSoloPipeSendStoresDeliversAndReadsBack(t *testing.T) {
	p := newPipeEnv(t)
	res, err := p.localSend(pipeSendArgs("work", "hello solo", false))
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if got := p.host.pastes(); len(got) != 1 || got[0].Text != "hello solo" || len(p.host.keys()) != 1 {
		t.Fatalf("pastes %v keys %v", got, p.host.keys())
	}
	read := p.read("")
	if len(read.Turns) != 1 || read.Turns[0].Direction != ops.PipeSent || read.Turns[0].Text != "hello solo" || read.Withheld != 0 || read.Reason != "" || read.Notice != "" {
		t.Fatalf("pipe_read = %+v (Solo withholds nothing)", read)
	}
	if !pipe.IsTurnID(read.Turns[0].TurnID) {
		t.Fatalf("the id of a sent message must be a cursor: %q", read.Turns[0].TurnID)
	}
}

func TestSoloPipeSendRefusedByTheGuardsStoresNothingAndKeepsTheBudget(t *testing.T) {
	p := newPipeEnv(t)
	p.cls.set("claude", "agent:claude", ops.PaneStateBusy)
	res, err := p.localSend(pipeSendArgs("work", "busy now", false))
	if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "busy" || !res.Forceable {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(p.pipeEntries()) != 0 {
		t.Fatalf("a refused message stayed in the store: %+v", p.pipeEntries())
	}
	if n, _ := p.st.CountMessagesSince(context.Background(), soloPeer("work"), p.clk.Now().Add(-time.Hour)); n != 0 {
		t.Fatalf("a refused message used budget: %d", n)
	}
	// A guard that errors before anything was sent also retracts.
	p.cls.set("claude", "agent:claude", ops.PaneStateStable)
	_, err = p.localSend(pipeSendArgs("work", "bad\x1b", false))
	wantAPIError(t, err, "invalid_argument", "")
	if len(p.pipeEntries()) != 0 {
		t.Fatalf("a message that was never sent stayed: %+v", p.pipeEntries())
	}
	// Forced delivery stores it.
	p.cls.set("claude", "agent:claude", ops.PaneStateBusy)
	forced, err := p.localSend(pipeSendArgs("work", "busy now", true))
	if err != nil || forced.Outcome != ops.OutcomeDelivered || !forced.Forced {
		t.Fatalf("forced = %+v, %v", forced, err)
	}
	if len(p.pipeEntries()) != 1 {
		t.Fatalf("entries = %+v", p.pipeEntries())
	}
}

func TestSoloPipeSendKeepsAMessageWhoseOutcomeIsUnknown(t *testing.T) {
	p := newPipeEnv(t)
	p.ah.markErr = errors.New("tmux timed out")
	res, err := p.localSend(pipeSendArgs("work", "maybe", false))
	if err != nil || res.Outcome != ops.OutcomeUnknown {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if len(p.pipeEntries()) != 1 {
		t.Fatalf("the text may have gone out: the message must stay: %+v", p.pipeEntries())
	}
}

func TestSoloPipeSendRequestIDIsOneSendAndOneCharge(t *testing.T) {
	p := newPipeEnv(t)
	args := pipeSendArgs("work", "once", false)
	for i := 0; i < 3; i++ {
		res, err := p.localSend(args)
		if err != nil || res.Outcome != ops.OutcomeDelivered {
			t.Fatalf("send %d = %+v, %v", i, res, err)
		}
	}
	if len(p.host.pastes()) != 1 || len(p.pipeEntries()) != 1 {
		t.Fatalf("a repeated request_id sent %d times and stored %d", len(p.host.pastes()), len(p.pipeEntries()))
	}
	if n, _ := p.st.CountMessagesSince(context.Background(), soloPeer("work"), p.clk.Now().Add(-time.Hour)); n != 1 {
		t.Fatalf("budget charged %d times", n)
	}
}

func TestSoloPipeSendLimits(t *testing.T) {
	p := newPipeEnv(t)
	_, err := p.localSend(pipeSendArgs("work", strings.Repeat("x", pipe.MaxMessageBytes+1), false))
	wantAPIError(t, err, "message_too_large", "")

	// Burst of 5, then the rate limit with a retry hint and nothing stored.
	for i := 0; i < pipe.SendBurst; i++ {
		if res, err := p.localSend(pipeSendArgs("work", fmt.Sprintf("m%d", i), false)); err != nil || res.Outcome != ops.OutcomeDelivered {
			t.Fatalf("send %d = %+v, %v", i, res, err)
		}
	}
	_, err = p.localSend(pipeSendArgs("work", "too fast", false))
	ae := apiErrorOf(t, err, "pipe_rate_limited", "")
	if !strings.Contains(string(ae.ProtoError().Details), "retry_after_ms") {
		t.Fatalf("details = %s", ae.ProtoError().Details)
	}
	if len(p.pipeEntries()) != pipe.SendBurst || len(p.host.pastes()) != pipe.SendBurst {
		t.Fatalf("a rejected message was stored or sent: %d entries, %d pastes", len(p.pipeEntries()), len(p.host.pastes()))
	}
}

func TestSoloPipeBudgetIsPerPeerAndPersistent(t *testing.T) {
	p := newPipeEnv(t, func(c *PipeConfig) { c.BudgetPerDay = 2 })
	for i := 0; i < 2; i++ {
		p.clk.Advance(2 * time.Second)
		if res, err := p.localSend(pipeSendArgs("work", fmt.Sprintf("m%d", i), false)); err != nil || res.Outcome != ops.OutcomeDelivered {
			t.Fatalf("send %d = %+v, %v", i, res, err)
		}
	}
	p.clk.Advance(2 * time.Second)
	_, err := p.localSend(pipeSendArgs("work", "third", false))
	wantAPIError(t, err, "pipe_budget_exceeded", "")
	// Another session has its own budget.
	p.ah.addSession("other", "claude")
	if res, err := p.localSend(pipeSendArgs("other", "hi", false)); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("other session = %+v, %v", res, err)
	}
}

// --- Solo reply extraction and pipe_wait --------------------------------------

func TestSoloPipeSendThenPipeWaitReturnsTheTurn(t *testing.T) {
	p := newPipeEnv(t)
	if _, err := p.localSend(pipeSendArgs("work", "ping", false)); err != nil {
		t.Fatal(err)
	}
	cursor := p.read("").Turns[0].TurnID

	type waitRes struct {
		turns []ops.PipeTurn
		err   error
	}
	done := make(chan waitRes, 1)
	go func() {
		turns, err := p.wait(cursor, 5*time.Second)
		done <- waitRes{turns, err}
	}()
	time.Sleep(30 * time.Millisecond) // the wait is running; the agent answers now
	p.ah.setView(claudeView([]string{"> ping", "", "● pong"}, ""))
	r := <-done
	if r.err != nil || len(r.turns) != 1 || r.turns[0].Text != "pong" || r.turns[0].Direction != ops.PipeReceived || r.turns[0].Session != "work" {
		t.Fatalf("wait = %+v, %v", r.turns, r.err)
	}
	read := p.read("")
	if len(read.Turns) != 2 || read.Turns[0].Direction != ops.PipeSent || read.Turns[1].Direction != ops.PipeReceived || read.Turns[1].TurnID != r.turns[0].TurnID {
		t.Fatalf("pipe_read = %+v", read)
	}
	// Frames are read only while a delivery is armed: the turn is final.
	reads := p.ah.readCount()
	time.Sleep(60 * time.Millisecond)
	if p.ah.readCount() != reads {
		t.Fatalf("frames were read after the turn: %d -> %d", reads, p.ah.readCount())
	}
	// A second pipe_wait after the turn ends at the timeout with no turn.
	turns, err := p.wait(r.turns[0].TurnID, 80*time.Millisecond)
	if err != nil || len(turns) != 0 {
		t.Fatalf("wait after the last turn = %+v, %v", turns, err)
	}
}

func TestSoloPipeWaitWithoutCursorStartsAfterTheLatestTurn(t *testing.T) {
	p := newPipeEnv(t)
	id, _ := pipe.NewTurnID(p.clk.Now())
	if _, err := p.st.PutTurn(context.Background(), pipe.Turn{TurnID: id, Peer: soloPeer("work"), Text: "old", At: p.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	turns, err := p.wait("", 60*time.Millisecond)
	if err != nil || len(turns) != 0 {
		t.Fatalf("an old turn was returned as new: %+v, %v", turns, err)
	}
	// An unknown cursor is an error, not an empty answer.
	unknown, _ := pipe.NewTurnID(time.Now())
	_, err = p.wait(unknown, 50*time.Millisecond)
	wantAPIError(t, err, "not_found", "")
}

func TestSoloReadAndWaitRefuseAnUnsharedSession(t *testing.T) {
	p := newPipeEnv(t)
	if err := p.st.SaveShare(store.ShareSelection{Mode: store.ShareNone}); err != nil {
		t.Fatal(err)
	}
	_, err := p.localCall(ops.OpPipeRead, ops.PipeReadArgs{Target: target("work")})
	wantAPIError(t, err, "forbidden", "not_shared")
	_, err = p.wait("", 20*time.Millisecond)
	wantAPIError(t, err, "forbidden", "not_shared")
}

func TestFollowRetriesReadErrorsAndStopsAtTheBound(t *testing.T) {
	t.Run("transient errors are retried", func(t *testing.T) {
		p := newPipeEnv(t)
		p.ah.readErr = func(call int) error {
			if call <= 2 {
				return errors.New("tmux busy")
			}
			return nil
		}
		if _, err := p.localSend(pipeSendArgs("work", "ping", false)); err != nil {
			t.Fatal(err)
		}
		p.ah.setView(claudeView([]string{"> ping", "", "● pong"}, ""))
		pipeEventually(t, "the turn after two failed reads", func() bool { return len(p.read("").Turns) == 2 })
		if len(p.host.pastes()) != 1 {
			t.Fatal("the message was written again")
		}
	})
	t.Run("persistent errors give up and never write again", func(t *testing.T) {
		p := newPipeEnv(t)
		p.ah.readErr = func(int) error { return errors.New("tmux gone mad") }
		if _, err := p.localSend(pipeSendArgs("work", "ping", false)); err != nil {
			t.Fatal(err)
		}
		pipeEventually(t, "the follower to give up", func() bool { return p.ah.readCount() >= 4 })
		time.Sleep(80 * time.Millisecond)
		if got := p.ah.readCount(); got != 4 {
			t.Fatalf("reads = %d, want exactly the bound of 4", got)
		}
		if len(p.host.pastes()) != 1 || len(p.read("").Turns) != 1 {
			t.Fatalf("pastes %d, entries %+v", len(p.host.pastes()), p.read(""))
		}
	})
}

func TestFollowPaneGoneFlushesTheReplyReadSoFarAndStops(t *testing.T) {
	p := newPipeEnv(t)
	if _, err := p.localSend(pipeSendArgs("work", "go", false)); err != nil {
		t.Fatal(err)
	}
	p.ah.setView(claudeView([]string{"> go", "", "● half an answer"}, claudeBusyLine))
	reads := p.ah.readCount()
	pipeEventually(t, "a busy frame to be read", func() bool { return p.ah.readCount() >= reads+2 })
	p.ah.mu2Lock(func() { p.ah.readErr = func(int) error { return fmt.Errorf("pane: %w", term.ErrNotFound) } })
	pipeEventually(t, "the flushed turn", func() bool { return len(p.read("").Turns) == 2 })
	got := p.read("").Turns[1]
	if got.Text != "half an answer" || got.Direction != ops.PipeReceived {
		t.Fatalf("flushed turn = %+v", got)
	}
	reads = p.ah.readCount()
	time.Sleep(60 * time.Millisecond)
	if p.ah.readCount() != reads {
		t.Fatal("frames were read after the pane was gone")
	}
}

func (h *anchorHost) mu2Lock(f func()) {
	h.am.Lock()
	defer h.am.Unlock()
	f()
}

func TestFollowEvidenceInvalidatedGivesNoTurn(t *testing.T) {
	p := newPipeEnv(t)
	if _, err := p.localSend(pipeSendArgs("work", "ping", false)); err != nil {
		t.Fatal(err)
	}
	p.ah.mu2Lock(func() { p.ah.gen++ }) // the scrollback was cleared
	p.ah.setView(claudeView([]string{"> ping", "", "● pong"}, ""))
	pipeEventually(t, "the follower to end", func() bool {
		r := p.ah.readCount()
		time.Sleep(40 * time.Millisecond)
		return r > 0 && p.ah.readCount() == r
	})
	if read := p.read(""); len(read.Turns) != 1 {
		t.Fatalf("a turn was made up from invalid evidence: %+v", read)
	}
}

func TestANewDeliveryKeepsTheReplyOfTheOlderOne(t *testing.T) {
	p := newPipeEnv(t)
	if _, err := p.localSend(pipeSendArgs("work", "first", false)); err != nil {
		t.Fatal(err)
	}
	p.ah.setView(claudeView([]string{"> first", "", "● partial reply"}, claudeBusyLine))
	reads := p.ah.readCount()
	pipeEventually(t, "a busy frame", func() bool { return p.ah.readCount() >= reads+2 })
	p.clk.Advance(2 * time.Second)
	p.cls.set("claude", "agent:claude", ops.PaneStateBusy)
	forced, err := p.localSend(pipeSendArgs("work", "second", true)) // FORCE past the busy guard
	if err != nil || forced.Outcome != ops.OutcomeDelivered {
		t.Fatalf("second = %+v, %v", forced, err)
	}
	read := p.read("")
	var texts []string
	for _, e := range read.Turns {
		texts = append(texts, e.Direction+":"+e.Text)
	}
	if len(texts) != 3 || !strings.Contains(strings.Join(texts, "|"), "received:partial reply") {
		t.Fatalf("entries = %v, want the partial reply of the first message kept", texts)
	}
}

func TestPipeServiceStopEndsTheFollowers(t *testing.T) {
	p := newPipeEnv(t)
	if _, err := p.localSend(pipeSendArgs("work", "ping", false)); err != nil {
		t.Fatal(err)
	}
	if err := p.svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	reads := p.ah.readCount()
	time.Sleep(40 * time.Millisecond)
	if p.ah.readCount() != reads {
		t.Fatal("a follower survived Stop")
	}
	// After Stop a delivery is still written but no longer followed.
	if res, err := p.hubSend(allActions, pipeSendArgs("work", "late", false)); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("late send = %+v, %v", res, err)
	}
}

// --- Hub mode: local calls go to the Hub -------------------------------------

func hubModeEnv(t *testing.T) *pipeEnv {
	t.Helper()
	p := newPipeEnv(t)
	p.hub.setEnrolled(true)
	p.hub.caller = &fakePipeCaller{}
	return p
}

func TestHubModeLocalPipeSendIsForwardedAndNeverUsesTheLocalBrokerOrPane(t *testing.T) {
	p := hubModeEnv(t)
	hubResult := `{"outcome":"delivered","warnings":[{"code":"x_from_hub","message":"as the Hub said"}]}`
	p.hub.caller.doFn = func(op string, args any) (json.RawMessage, error) { return json.RawMessage(hubResult), nil }
	args := pipeSendArgs("work", "to another worker", false)
	args.Target.WorkerID = ops.NewUUIDv7()

	raw, err := p.localCall(ops.OpPipeSend, args)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != hubResult {
		t.Fatalf("the Hub's answer must be returned unchanged: %s", raw)
	}
	if len(p.hub.caller.calls) != 1 || p.hub.caller.calls[0].Op != ops.OpPipeSend {
		t.Fatalf("calls = %+v", p.hub.caller.calls)
	}
	var sent ops.PipeSendArgs
	if err := json.Unmarshal([]byte(p.hub.caller.calls[0].Args), &sent); err != nil || sent.RequestID != args.RequestID || sent.Text != args.Text || sent.Target != args.Target {
		t.Fatalf("forwarded arguments = %s (%v); the caller's request_id must stay", p.hub.caller.calls[0].Args, err)
	}
	if len(p.host.pastes()) != 0 || len(p.pipeEntries()) != 0 {
		t.Fatalf("Hub mode used the local pane or store: %v %v", p.host.pastes(), p.pipeEntries())
	}
	if n, _ := p.st.CountMessagesSince(context.Background(), soloPeer("work"), p.clk.Now().Add(-time.Hour)); n != 0 {
		t.Fatalf("the Worker charged budget in Hub mode: %d", n)
	}
}

func TestHubModeLocalReadPassesWithheldReasonAndNoticeUnchanged(t *testing.T) {
	p := hubModeEnv(t)
	hubResult := `{"turns":[{"turn_id":"` + ops.NewUUIDv7() + `","session":"work","direction":"sent","text":"hi","at":"2026-11-09T12:00:00.000Z"}],"withheld":2,"reason":"untrusted_peer","notice":"2 pipe turn(s) were withheld"}`
	p.hub.caller.doFn = func(op string, args any) (json.RawMessage, error) { return json.RawMessage(hubResult), nil }
	raw, err := p.localCall(ops.OpPipeRead, ops.PipeReadArgs{Target: target("work")})
	if err != nil {
		t.Fatal(err)
	}
	var got, want ops.PipeReadResult
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(hubResult), &want)
	if got.Withheld != 2 || got.Reason != "untrusted_peer" || got.Notice != want.Notice || len(got.Turns) != 1 {
		t.Fatalf("read = %+v", got)
	}
	if string(raw) != hubResult {
		t.Fatalf("the Hub's answer must be returned as it is: %s", raw)
	}
}

func TestHubModeLocalWaitStreamsTheHubsTurnsAsProgress(t *testing.T) {
	p := hubModeEnv(t)
	el := `{"turn_id":"` + ops.NewUUIDv7() + `","session":"work","direction":"received","text":"hello","at":"2026-11-09T12:00:00.000Z"}`
	p.hub.caller.stream = func(op string, args any, emit func(json.RawMessage) error) error {
		if op != ops.OpPipeWait {
			t.Errorf("op = %s", op)
		}
		return emit(json.RawMessage(el))
	}
	var got []string
	ctx := WithProgress(context.Background(), func(m json.RawMessage) { got = append(got, string(m)) })
	if _, err := p.localCallCtx(ctx, ops.OpPipeWait, ops.PipeWaitArgs{Target: target("work"), TimeoutMS: 1000}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != el {
		t.Fatalf("progress = %v", got)
	}
}

func TestHubModeWithoutAConnectionFailsAtOnceAndFallsBackToNothing(t *testing.T) {
	p := newPipeEnv(t)
	p.hub.setEnrolled(true) // enrolled, but no CallClient link: disconnected
	for _, op := range []string{ops.OpPipeSend, ops.OpPipeRead, ops.OpPipeWait} {
		var args any = map[string]any{"target": target("work")}
		if op == ops.OpPipeSend {
			args = pipeSendArgs("work", "x", false)
		}
		_, err := p.localCall(op, args)
		wantAPIError(t, err, "worker_offline", "")
	}
	if len(p.host.pastes()) != 0 || len(p.host.keys()) != 0 || len(p.pipeEntries()) != 0 {
		t.Fatal("a disconnected Hub-mode call fell back to the Solo broker or queued something")
	}
	// Offline never queues: once the link is back, nothing is sent by itself.
	p.hub.caller = &fakePipeCaller{doFn: func(string, any) (json.RawMessage, error) { return json.RawMessage(`{"outcome":"delivered"}`), nil }}
	time.Sleep(30 * time.Millisecond)
	if p.hub.caller.count() != 0 {
		t.Fatal("an offline call was replayed")
	}
}

func TestHubModeLostWriteIsUnknownAndNotRepeated(t *testing.T) {
	p := hubModeEnv(t)
	p.hub.caller.doFn = func(string, any) (json.RawMessage, error) { return nil, &CallLostError{Cause: errors.New("eof")} }
	res, err := p.localSend(pipeSendArgs("work", "maybe applied", false))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ops.OutcomeUnknown || res.Reason != "worker_offline" || len(res.Warnings) == 0 {
		t.Fatalf("result = %+v, want unknown/worker_offline with an explanation", res)
	}
	if p.hub.caller.count() != 1 {
		t.Fatalf("the write was repeated: %d calls", p.hub.caller.count())
	}
	if len(p.host.pastes()) != 0 || len(p.pipeEntries()) != 0 {
		t.Fatal("a lost Hub write touched the local pane or store")
	}
}

func TestHubModeErrorsOfTheHubKeepTheirCode(t *testing.T) {
	p := hubModeEnv(t)
	p.hub.caller.doFn = func(string, any) (json.RawMessage, error) {
		return nil, proto.NewError(proto.CodePipeRateLimited, "slow down")
	}
	_, err := p.localCall(ops.OpPipeSend, pipeSendArgs("work", "x", false))
	if proto.CodeOf(err) != proto.CodePipeRateLimited {
		t.Fatalf("err = %v", err)
	}
}

func TestEnrollmentReadFailureNeverFallsBackToSolo(t *testing.T) {
	p := newPipeEnv(t)
	p.hub.mu.Lock()
	p.hub.enrollErr = errors.New("disk")
	p.hub.mu.Unlock()
	_, err := p.localSend(pipeSendArgs("work", "x", false))
	wantAPIError(t, err, "internal", "")
	if len(p.host.pastes()) != 0 || len(p.pipeEntries()) != 0 {
		t.Fatal("an unknown mode fell back to Solo")
	}
}

func TestHubModeStillChecksTheLocalCallersActions(t *testing.T) {
	p := hubModeEnv(t)
	_, err := p.svc.Local(context.Background(), Request{Op: ops.OpPipeSend, Actions: []ops.Action{ops.ActionRead}, Args: mustJSON(t, pipeSendArgs("work", "x", false))})
	wantAPIError(t, err, "forbidden", "")
	if p.hub.caller.count() != 0 {
		t.Fatal("a forbidden local call reached the Hub")
	}
}

// --- local entry guards --------------------------------------------------------

func TestLocalExecutorRefusesPipeOpsWithoutAServiceAndNeverRunsThemAsAHubWrite(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	l := localExecutor{ex: e.ex, log: discardLogger()}
	args := mustJSON(t, pipeSendArgs("work", "x", false))
	for _, op := range []string{ops.OpPipeSend, ops.OpPipeRead, ops.OpPipeWait} {
		_, err := l.Execute(context.Background(), localapiRequest(op, args), func(json.RawMessage) {})
		if proto.CodeOf(err) != proto.CodeUnsupportedOp {
			t.Fatalf("%s: err = %v", op, err)
		}
	}
	if len(e.host.pastes()) != 0 {
		t.Fatal("a local pipe_send ran as the Hub's write")
	}
}

func TestPipeSendTextNeverReachesTheLocalAudit(t *testing.T) {
	p := newPipeEnv(t)
	const secret = "tok-SECRET-12345"
	if _, err := p.hubSend(allActions, pipeSendArgs("work", secret, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.localSend(pipeSendArgs("work", secret+"2", false)); err != nil {
		t.Fatal(err)
	}
	for _, a := range p.audit() {
		if strings.Contains(fmt.Sprint(a), "SECRET") {
			t.Fatalf("audit row holds the text: %+v", a)
		}
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func localapiRequest(op string, args json.RawMessage) localapi.Request {
	return localapi.Request{Op: op, Actions: localapi.FullActions(), Args: args}
}

// TestPipeSendFromAHubWithoutAMessageIDFailsBeforeAnyEffect: a Hub that predates
// the causal ids names no delivery. The reply could not be matched to it, and
// the emitter would refuse the turn, so the Worker must not report the message
// delivered. It refuses with unsupported_op, names the update, writes nothing,
// stores no request record (the request may be repeated after the update) and
// queues no turn.
func TestPipeSendFromAHubWithoutAMessageIDFailsBeforeAnyEffect(t *testing.T) {
	p := newPipeEnv(t)
	p.hub.setEnrolled(true)
	args := pipeSendArgs("work", "hello", false)
	args.MessageID = 0
	res, err := p.writeAs(ops.OpPipeSend, allActions, args)
	wantAPIError(t, err, "unsupported_op", "")
	if err == nil || !strings.Contains(err.Error(), "Hub must be updated") {
		t.Fatalf("err = %v, want the update to be named", err)
	}
	if res.Outcome != "" {
		t.Fatalf("an outcome was reported: %+v", res)
	}
	if got := p.ah.keys(); len(got) != 0 || len(p.ah.pasted) != 0 {
		t.Fatalf("something was written: pastes %v keys %v", p.ah.pasted, got)
	}
	if rec, hit, _ := p.st.GetResult(context.Background(), args.RequestID); hit {
		t.Fatalf("a request record was stored: %+v", rec)
	}
}
