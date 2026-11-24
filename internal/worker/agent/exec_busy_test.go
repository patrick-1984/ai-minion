package agent

import (
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/ops"
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
