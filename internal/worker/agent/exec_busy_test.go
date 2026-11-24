package agent

import (
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
)

// A103: send_text into an agent busy with a turn, without force.

func busySubmit(session, text string) ops.SendTextArgs {
	a := sendText(session, text, false)
	a.Submit = true
	return a
}

// A busy Claude Code gets the text typed, never the submit key: a permission
// dialog could open between any screen check and the Enter, and the Enter
// would answer it. Shell-mode text ("!") is no different.
func TestBusyClaudeIsTypedOnlyWithoutEnter(t *testing.T) {
	for _, text := range []string{"hello", " 　look at this", "a ! b", "!ls", "  !x", "\t\n!y", "　 "} {
		e := newEnv(t)
		e.host.addSession("work", "claude")
		e.cls.set("claude", "agent:claude", ops.PaneStateBusy)
		res := e.mustWrite(ops.OpSendText, busySubmit("work", text))
		wantTypedOnly(t, e, res, "claude "+text)
		if p := e.host.pastes(); p[0].Text != text {
			t.Fatalf("%q: pastes = %+v", text, p)
		}
	}
}

// Every other busy agent gets the text typed, never the submit key.
func TestBusyOtherAgentsAreTypedOnlyWithoutEnter(t *testing.T) {
	for _, kind := range []string{"agent:codex", "agent:gemini", "agent:qwen", "agent:aider", "agent:opencode", "agent:something", ops.PaneKindUnknown, ops.PaneKindOther} {
		e := newEnv(t)
		e.host.addSession("work", "prog")
		e.cls.set("prog", kind, ops.PaneStateBusy)
		res := e.mustWrite(ops.OpSendText, busySubmit("work", "do it"))
		wantTypedOnly(t, e, res, kind)
	}
}

func wantTypedOnly(t *testing.T, e *env, res ops.WriteResult, what string) {
	t.Helper()
	if res.Outcome != ops.OutcomePartial || res.Reason != "busy" || res.Forceable || res.Forced {
		t.Fatalf("%s: result = %+v", what, res)
	}
	if res.Note != ops.BusyNoteTypedNotSubmitted || !hasWarning(res, "session_busy") {
		t.Fatalf("%s: note %q, warnings %+v", what, res.Note, res.Warnings)
	}
	if p := e.host.pastes(); len(p) != 1 {
		t.Fatalf("%s: pastes = %+v, want one", what, p)
	}
	if k := e.host.keys(); len(k) != 0 {
		t.Fatalf("%s: keys = %+v, want none (no submit key)", what, k)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// Without submit the text is typed as asked, delivered with the busy note.
func TestBusyWithoutSubmitIsDeliveredTyped(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "prog")
	e.cls.set("prog", "agent:codex", ops.PaneStateBusy)
	res := e.mustWrite(ops.OpSendText, sendText("work", "draft", false))
	if res.Outcome != ops.OutcomeDelivered || res.Note != ops.BusyNoteTyped || !hasWarning(res, "session_busy") || len(e.host.keys()) != 0 || len(e.host.pastes()) != 1 {
		t.Fatalf("result = %+v, keys %v", res, e.host.keys())
	}
}

// force=true keeps the v0.20.5 behaviour exactly: the FORCE guard is
// overridden, the text and its Enter go out, no busy note.
func TestBusyForcedIsUnchanged(t *testing.T) {
	for _, kind := range []string{"agent:claude", "agent:codex", "agent:gemini"} {
		e := newEnv(t)
		e.host.addSession("work", "prog")
		e.cls.set("prog", kind, ops.PaneStateBusy)
		a := busySubmit("work", "!go")
		a.Force = true
		res := e.mustWrite(ops.OpSendText, a)
		if res.Outcome != ops.OutcomeDelivered || !res.Forced || res.Note != "" || hasWarning(res, "session_busy") {
			t.Fatalf("%s: result = %+v", kind, res)
		}
		if k := e.host.keys(); len(k) != 1 || k[0][0] != "Enter" {
			t.Fatalf("%s: keys = %+v", kind, k)
		}
	}
}

// send_keys into a busy agent still refuses (forceable), now with a note;
// other refusals carry their notes too.
func TestRefusalsCarryNotes(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateBusy)
	res := e.mustWrite(ops.OpSendKeys, sendKeys("work", false, "Enter"))
	if res.Outcome != ops.OutcomeFailedNotSent || !res.Forceable || res.Note != ops.RefusalNote("busy", true) || len(e.host.keys()) != 0 {
		t.Fatalf("send_keys = %+v", res)
	}

	e.cls.set("claude", "agent:claude", ops.PaneStateDialog)
	res = e.mustWrite(ops.OpSendText, busySubmit("work", "x"))
	if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "dialog_open" || res.Note != ops.RefusalNote("dialog_open", true) {
		t.Fatalf("dialog = %+v", res)
	}

	e.host.addSession("sh", "bash")
	e.cls.set("bash", ops.PaneKindShell, ops.PaneStateBusy)
	res = e.mustWrite(ops.OpSendText, busySubmit("sh", "x"))
	if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "shell_pane" || res.Note == "" {
		t.Fatalf("shell = %+v", res)
	}
}

// The local audit report of a busy send is delivered with reason busy.
func TestBusySendReportsDeliveredBusy(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateBusy)
	var mu sync.Mutex
	var got []LocalWrite
	e.ex.OnLocalWrite(func(lw LocalWrite) { mu.Lock(); got = append(got, lw); mu.Unlock() })
	e.mustWrite(ops.OpSendText, sendText("work", "x", false))
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Outcome != "delivered" || got[0].Reason != "busy" {
		t.Fatalf("reports = %+v", got)
	}
	rows := e.audit()
	if len(rows) != 1 || rows[0].Outcome != "delivered" || rows[0].Reason != "busy" {
		t.Fatalf("audit = %+v", rows)
	}
}

// claudeBashDialog is the screen of a Claude Code Bash permission dialog.
var claudeBashDialog = []string{
	"╭───────────────────────────────────────────────────────────────────╮",
	"│ Bash command                                                      │",
	"│                                                                   │",
	"│   rm -rf build/                                                   │",
	"│   Remove the build directory                                      │",
	"│                                                                   │",
	"│ Do you want to proceed?                                           │",
	"│ ❯ 1. Yes                                                          │",
	"│   2. Yes, and don't ask again for rm commands in /home/user/proj  │",
	"│   3. No, and tell Claude what to do differently (esc)             │",
	"╰───────────────────────────────────────────────────────────────────╯",
}

// claudeBusyScreen is the screen of a Claude Code turn in progress.
var claudeBusyScreen = []string{
	"> Refactor the parser",
	"",
	"● I'll start by reading the parser module.",
	"",
	"✻ Thinking… (12s · ↑ 1.2k tokens · esc to interrupt)",
	"",
	"╭───────────────────────────────────────────────────────────────────────╮",
	"│ >                                                                     │",
	"╰───────────────────────────────────────────────────────────────────────╯",
	"  ? for shortcuts",
}

// A Bash permission dialog that opens while the text is pasted into a busy
// Claude Code never gets an Enter (it would approve the command): no key goes
// out on the busy path at all, and the screen is not read a second time.
func TestBusyClaudeDialogAfterPasteGetsNoEnter(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Classifier = agentdetect.New() })
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, claudeBusyScreen...)
	e.host.onPaste = func() { e.host.setScreen(ref, claudeBashDialog...) }
	res := e.mustWrite(ops.OpSendText, busySubmit("work", "next step"))
	wantTypedOnly(t, e, res, "claude dialog")
	if got := e.host.eventLog(); len(got) != 1 || got[0] != "paste "+ref.ID {
		t.Fatalf("events = %v, want the paste only", got)
	}
	e.host.mu.Lock()
	n := e.host.snapshots
	e.host.mu.Unlock()
	if n != 1 {
		t.Fatalf("snapshots = %d, want 1", n)
	}
}

// The idle and the forced paths are unchanged: no second look at the screen
// between the paste and the Enter.
func TestIdleAndForcedSendTakeNoSecondSnapshot(t *testing.T) {
	for _, tc := range []struct {
		state string
		force bool
	}{{ops.PaneStateStable, false}, {ops.PaneStateBusy, true}} {
		e := newEnv(t)
		e.host.addSession("work", "claude")
		e.cls.set("claude", "agent:claude", tc.state)
		// Even a dialog after the paste is not looked for on these paths.
		e.host.onPaste = func() { e.cls.set("claude", "agent:claude", ops.PaneStateDialog) }
		a := busySubmit("work", "go")
		a.Force = tc.force
		res := e.mustWrite(ops.OpSendText, a)
		if res.Outcome != ops.OutcomeDelivered || res.Note != "" || len(e.host.keys()) != 1 {
			t.Fatalf("%+v: result = %+v, keys %v", tc, res, e.host.keys())
		}
		e.host.mu.Lock()
		n := e.host.snapshots
		e.host.mu.Unlock()
		if n != 1 {
			t.Fatalf("%+v: snapshots = %d, want 1", tc, n)
		}
	}
}
