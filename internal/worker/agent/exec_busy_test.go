package agent

import (
	"errors"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// A103: send_text into an agent busy with a turn, without force.

func busySubmit(session, text string) ops.SendTextArgs {
	a := sendText(session, text, false)
	a.Submit = true
	return a
}

// Claude Code queues input typed during a turn: the text and its Enter go
// out exactly as into an idle pane, with the busy note and warning.
func TestBusyClaudeSendIsDeliveredWithEnterAndNote(t *testing.T) {
	for _, text := range []string{"hello", " 　look at this", "a ! b"} {
		e := newEnv(t)
		e.host.addSession("work", "claude")
		e.cls.set("claude", "agent:claude", ops.PaneStateBusy)
		res := e.mustWrite(ops.OpSendText, busySubmit("work", text))
		if res.Outcome != ops.OutcomeDelivered || res.Reason != "" || res.Forced || res.Forceable {
			t.Fatalf("%q: result = %+v", text, res)
		}
		if res.Note != ops.BusyNoteClaudeQueued || !hasWarning(res, "session_busy") {
			t.Fatalf("%q: note %q, warnings %+v", text, res.Note, res.Warnings)
		}
		if p := e.host.pastes(); len(p) != 1 || p[0].Text != text {
			t.Fatalf("%q: pastes = %+v", text, p)
		}
		if k := e.host.keys(); len(k) != 1 || len(k[0]) != 1 || k[0][0] != "Enter" {
			t.Fatalf("%q: keys = %+v, want one Enter", text, k)
		}
	}
}

// Claude text starting with "!" (after any white space, Unicode included)
// switches Claude Code to shell mode: it is typed and not submitted.
func TestBusyClaudeBangIsTypedOnly(t *testing.T) {
	for _, text := range []string{"!ls", "  !x", "  !rm", "\t\n!y"} {
		e := newEnv(t)
		e.host.addSession("work", "claude")
		e.cls.set("claude", "agent:claude", ops.PaneStateBusy)
		res := e.mustWrite(ops.OpSendText, busySubmit("work", text))
		wantTypedOnly(t, e, res, text)
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

// Text of white space only (Unicode included) or empty must not panic.
func TestBusyWhitespaceOnlyTextDoesNotPanic(t *testing.T) {
	for _, text := range []string{"", " ", "　", "  \t"} {
		if busyQueues("agent:claude", text) != true {
			t.Errorf("%q: a blank text is not a shell-mode text", text)
		}
		if busyQueues("agent:codex", text) {
			t.Errorf("%q: codex never queues", text)
		}
	}
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateBusy)
	res := e.mustWrite(ops.OpSendText, busySubmit("work", "　 "))
	if res.Outcome != ops.OutcomeDelivered || res.Note != ops.BusyNoteClaudeQueued {
		t.Fatalf("result = %+v", res)
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
	e.mustWrite(ops.OpSendText, busySubmit("work", "x"))
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

// wantHeldBack checks a busy Claude send whose Enter was held back after the
// paste: the text went out once, no key at all, partial with the reason.
func wantHeldBack(t *testing.T, e *env, res ops.WriteResult, reason, note string) {
	t.Helper()
	if res.Outcome != ops.OutcomePartial || res.Reason != reason || res.Forced || res.Forceable {
		t.Fatalf("result = %+v, want partial %s", res, reason)
	}
	if res.Note != note || !hasWarning(res, "session_busy") {
		t.Fatalf("note %q, warnings %+v", res.Note, res.Warnings)
	}
	if p := e.host.pastes(); len(p) != 1 {
		t.Fatalf("pastes = %+v, want one", p)
	}
	if k := e.host.keys(); len(k) != 0 {
		t.Fatalf("keys = %+v, want none: the Enter would answer the dialog", k)
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
}

// A Bash permission dialog that opens while the text is pasted into a busy
// Claude Code must not get the Enter: it would approve the command. The real
// classifier reads the fresh screen.
func TestBusyClaudeDialogAfterPasteGetsNoEnter(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Classifier = agentdetect.New() })
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, claudeBusyScreen...)
	e.host.onPaste = func() { e.host.setScreen(ref, claudeBashDialog...) }
	res := e.mustWrite(ops.OpSendText, busySubmit("work", "next step"))
	wantHeldBack(t, e, res, "dialog_open", ops.BusyNoteDialogOpened)
}

// The same with the classifier switching to the dialog state after the paste.
func TestBusyClaudeDialogStateAfterPasteGetsNoEnter(t *testing.T) {
	e := newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateBusy)
	e.host.onPaste = func() { e.cls.set("claude", "agent:claude", ops.PaneStateDialog) }
	res := e.mustWrite(ops.OpSendText, busySubmit("work", "next step"))
	wantHeldBack(t, e, res, "dialog_open", ops.BusyNoteDialogOpened)
}

// A screen that cannot be read (completely) after the paste holds the Enter
// back: nobody can tell whether a dialog opened.
func TestBusyClaudeSnapshotErrorAfterPasteGetsNoEnter(t *testing.T) {
	for _, serr := range []error{term.ErrIncomplete, errors.New("tmux: broken pipe")} {
		e := newEnv(t, func(c *Config) { c.Classifier = agentdetect.New() })
		ref := e.host.addSession("work", "claude")
		e.host.setScreen(ref, claudeBusyScreen...)
		e.host.onPaste = func() { e.host.mu.Lock(); e.host.snapshotErr = serr; e.host.mu.Unlock() }
		res := e.mustWrite(ops.OpSendText, busySubmit("work", "next step"))
		wantHeldBack(t, e, res, "view_incomplete", ops.BusyNoteUnchecked)
	}
}

// A pane that changed or vanished during the paste gets no Enter.
func TestBusyClaudePaneChangedAfterPasteGetsNoEnter(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateBusy)
	e.host.onPaste = func() { e.host.mutatePane(ref.ID, func(p *term.Pane) { p.Ref.PID++ }) }
	res := e.mustWrite(ops.OpSendText, busySubmit("work", "next step"))
	wantHeldBack(t, e, res, "pane_changed", ops.BusyNoteUnchecked)

	e = newEnv(t)
	e.host.addSession("work", "claude")
	e.cls.set("claude", "agent:claude", ops.PaneStateBusy)
	e.host.onPaste = func() { e.host.mu.Lock(); e.host.sessions = nil; e.host.mu.Unlock() }
	res = e.mustWrite(ops.OpSendText, busySubmit("work", "next step"))
	wantHeldBack(t, e, res, "target_not_found", ops.BusyNoteUnchecked)
}

// A busy Claude Code whose screen still shows the running turn after the
// paste gets its Enter, read by the real classifier.
func TestBusyClaudeStillBusyAfterPasteGetsEnter(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Classifier = agentdetect.New() })
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, claudeBusyScreen...)
	res := e.mustWrite(ops.OpSendText, busySubmit("work", "next step"))
	if res.Outcome != ops.OutcomeDelivered || res.Note != ops.BusyNoteClaudeQueued || !hasWarning(res, "session_busy") {
		t.Fatalf("result = %+v", res)
	}
	if got := e.host.eventLog(); len(got) != 2 || got[0] != "paste "+ref.ID || got[1] != "keys "+ref.ID+" Enter" {
		t.Fatalf("events = %v", got)
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
