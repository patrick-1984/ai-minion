package conpty

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

var ctx = context.Background()

// eventually polls cond for up to two seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitExited waits until the watcher has marked the session exited.
func waitExited(t *testing.T, s *session) {
	t.Helper()
	eventually(t, "session marked exited", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.exited
	})
}

func snapshotText(t *testing.T, h *Host, ref term.PaneRef) string {
	t.Helper()
	scr, err := h.Snapshot(ctx, ref)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return strings.Join(scr.Lines, "\n")
}

func waitScreen(t *testing.T, h *Host, ref term.PaneRef, want string) {
	t.Helper()
	eventually(t, fmt.Sprintf("screen to contain %q", want), func() bool {
		return strings.Contains(snapshotText(t, h, ref), want)
	})
}

func create(t *testing.T, h *Host, name string, argv ...string) (term.Session, term.PaneRef) {
	t.Helper()
	s, err := h.CreateSession(ctx, name, "", argv)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(s.Panes) != 1 {
		t.Fatalf("panes = %d, want 1", len(s.Panes))
	}
	return s, s.Panes[0].Ref
}

// AC1: the host implements the port, one pane per session, minion-N names.
func TestCreateSessionOnePaneAndNames(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	var _ term.SessionHost = h

	a, refA := create(t, h, "", "cmd", "/c", "echo", "hi")
	b, _ := create(t, h, "")
	c, _ := create(t, h, "minion-7", "pwsh")
	if a.Name != "minion-1" || b.Name != "minion-2" || c.Name != "minion-7" {
		t.Fatalf("names = %q %q %q", a.Name, b.Name, c.Name)
	}
	if a.Windows != 1 || a.Attached || !a.Panes[0].Active || a.Panes[0].Window != 0 || a.Panes[0].Index != 0 {
		t.Fatalf("session shape = %+v", a)
	}
	if refA.PID != 1000 || refA.Command != "cmd" || refA.ID == "" {
		t.Fatalf("pane ref = %+v", refA)
	}
	if got := fs.specs[0]; got.path != `C:\fakebin\cmd.exe` || strings.Join(got.args, " ") != "/c echo hi" {
		t.Fatalf("argv not passed through directly: %+v", got)
	}
	if _, err := h.CreateSession(ctx, "minion-7", "", nil); err == nil {
		t.Fatal("duplicate session name was accepted")
	}
	list, _ := h.ListSessions(ctx)
	if len(list) != 3 || list[0].Name != "minion-1" || list[2].Name != "minion-7" {
		t.Fatalf("list = %+v", list)
	}
	// An auto name never reuses a taken one.
	d, _ := create(t, h, "")
	if d.Name != "minion-3" {
		t.Fatalf("auto name = %q", d.Name)
	}
}

func TestDefaultProgram(t *testing.T) {
	h, fs := newTestHost(true)
	defer h.Close()
	_, ref := create(t, h, "")
	if p := fs.specs[0].path; !strings.HasSuffix(p, "pwsh.exe") || ref.Command != "pwsh" {
		t.Fatalf("default with pwsh: path %q command %q", p, ref.Command)
	}

	h2, fs2 := newTestHost(false)
	defer h2.Close()
	_, ref2 := create(t, h2, "")
	if fs2.specs[0].path != `C:\fakebin\cmd.exe` || ref2.Command != "cmd" {
		t.Fatalf("default without pwsh: %+v %q", fs2.specs[0], ref2.Command)
	}
}

func TestCreateSessionPassesDirAndSize(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	if _, err := h.CreateSession(ctx, "", `C:\work`, []string{"cmd"}); err != nil {
		t.Fatal(err)
	}
	if got := fs.specs[0]; got.dir != `C:\work` || got.cols != defaultCols || got.rows != defaultRows {
		t.Fatalf("spec = %+v", got)
	}
}

func TestCreateSessionStartFailure(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	fs.fail = errors.New("boom")
	if _, err := h.CreateSession(ctx, "", "", []string{"nope"}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if list, _ := h.ListSessions(ctx); len(list) != 0 {
		t.Fatalf("failed start left a session: %+v", list)
	}
}

func TestCommandName(t *testing.T) {
	for in, want := range map[string]string{
		"pwsh": "pwsh", `C:\Program Files\PowerShell\7\pwsh.exe`: "pwsh", "cmd.EXE": "cmd",
		"/usr/bin/x": "x", `C:\a\run.bat`: "run", "noext": "noext",
	} {
		if got := commandName(in); got != want {
			t.Errorf("commandName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolvePane(t *testing.T) {
	h, _ := newTestHost(false)
	defer h.Close()
	s, ref := create(t, h, "")
	for _, pane := range []string{"", "0.0", "0"} {
		got, err := h.ResolvePane(ctx, ops.Target{SessionName: s.Name, Pane: pane})
		if err != nil || got != ref {
			t.Fatalf("pane %q: %+v, %v", pane, got, err)
		}
	}
	for _, tg := range []ops.Target{{SessionName: "nope"}, {SessionName: s.Name, Pane: "0.1"}, {SessionName: s.Name, Pane: "1.0"}} {
		if _, err := h.ResolvePane(ctx, tg); !errors.Is(err, term.ErrNotFound) {
			t.Errorf("%+v: err = %v, want ErrNotFound", tg, err)
		}
	}
}

// AC2: the screen comes from the VT buffer fed by the pseudo console, through
// the sanitiser, as ops.Screen.
func TestSnapshotFromPseudoConsoleOutput(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "cmd")
	p := fs.proc(0)
	p.show("\x1b[?25l\x1b[2J\x1b[H\x1b]0;window title\x07")
	p.show("C:\\> \x1b[1;31mred\x1b[0m done\x1b]52;c;c2VjcmV0\x07\r\nsecond")
	waitScreen(t, h, ref, "second")

	scr, err := h.Snapshot(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if scr.Width != defaultCols || scr.Height != defaultRows {
		t.Fatalf("size = %dx%d", scr.Width, scr.Height)
	}
	if len(scr.Lines) != 2 || scr.Lines[0] != `C:\> red done` || scr.Lines[1] != "second" {
		t.Fatalf("lines = %q", scr.Lines)
	}
	for _, l := range scr.Lines {
		if strings.ContainsRune(l, 0x1b) || strings.Contains(l, "window title") || strings.Contains(l, "c2VjcmV0") {
			t.Fatalf("unsanitised line %q", l)
		}
	}
	if len(scr.Spans) != 1 {
		t.Fatalf("spans = %+v", scr.Spans)
	}
	if sp := scr.Spans[0]; sp.Line != 0 || sp.Start != 5 || sp.End != 8 || !sp.Bold || sp.FG != "p1" {
		t.Fatalf("span = %+v", sp)
	}
	if scr.Cursor != nil {
		t.Fatalf("cursor should be hidden: %+v", scr.Cursor)
	}
	p.show("\x1b[?25h")
	eventually(t, "cursor shown", func() bool {
		s, _ := h.Snapshot(ctx, ref)
		return s.Cursor != nil && s.Cursor.Row == 1 && s.Cursor.Col == 6
	})
}

func TestSnapshotUnknownPane(t *testing.T) {
	h, _ := newTestHost(false)
	defer h.Close()
	if _, err := h.Snapshot(ctx, term.PaneRef{ID: "%99"}); !errors.Is(err, term.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.Capture(ctx, term.PaneRef{ID: "%99"}, 5); !errors.Is(err, term.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestCaptureIncludesScrollback(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "cmd")
	var sb strings.Builder
	for i := 1; i <= 100; i++ { // 30 rows on screen, the rest scrolls off
		fmt.Fprintf(&sb, "line %d\r\n", i)
	}
	fs.proc(0).show(sb.String() + "last")
	waitScreen(t, h, ref, "last")

	scr, err := h.Capture(ctx, ref, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(scr.Lines) != 50 || scr.Lines[0] != "line 52" || scr.Lines[49] != "last" {
		t.Fatalf("capture: %d lines, first %q last %q", len(scr.Lines), scr.Lines[0], scr.Lines[len(scr.Lines)-1])
	}
	if scr.Cursor != nil {
		t.Fatal("capture must not carry a cursor")
	}
	all, _ := h.Capture(ctx, ref, 100000)
	if len(all.Lines) != 101 || all.Lines[0] != "line 1" {
		t.Fatalf("full capture: %d lines, first %q", len(all.Lines), all.Lines[0])
	}
	none, _ := h.Capture(ctx, ref, 0)
	if len(none.Lines) != 0 {
		t.Fatalf("capture 0 = %q", none.Lines)
	}
}

// AC3: input goes to the pseudo console; the KEEP re-check compares the
// session's process id. The text is delivered verbatim: LF stays LF, TAB stays
// TAB, and the host adds no Enter.
func TestPasteTextWritesInput(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	if err := h.PasteText(ctx, ref, "echo hi\nsecond\tline\nthird", ref); err != nil {
		t.Fatal(err)
	}
	if got := fs.proc(0).input(); got != "echo hi\nsecond\tline\nthird" {
		t.Fatalf("input = %q", got)
	}
}

// TestPasteTextNeverTurnsLFIntoCR is the regression of the audit finding: the
// adapter used to rewrite every LF (and CRLF) to CR, the Enter key, so a
// multi-line text submitted its lines. No CR may reach the program from a paste,
// and a trailing LF must not become an Enter either.
func TestPasteTextNeverTurnsLFIntoCR(t *testing.T) {
	for _, bracketed := range []bool{false, true} {
		h, fs := newTestHost(false)
		_, ref := create(t, h, "", "pwsh")
		if bracketed {
			fs.proc(0).show("\x1b[?2004h")
			eventually(t, "bracketed paste on", func() bool { return h.sessions["minion-1"].bracketedPaste() })
		}
		if err := h.PasteText(ctx, ref, "a\nb\n", ref); err != nil {
			t.Fatal(err)
		}
		got := fs.proc(0).input()
		if strings.ContainsRune(got, '\r') {
			t.Errorf("bracketed=%v: a CR reached the program: %q", bracketed, got)
		}
		want := "a\nb\n"
		if bracketed {
			want = bracketStart + want + bracketEnd
		}
		if got != want {
			t.Errorf("bracketed=%v: input = %q, want %q", bracketed, got, want)
		}
		h.Close()
	}
}

func TestPasteTextBracketed(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	fs.proc(0).show("\x1b[?2004h")
	eventually(t, "bracketed paste on", func() bool {
		s := h.sessions["minion-1"]
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.vt.bracketed
	})
	if err := h.PasteText(ctx, ref, "a\nb\tc[201~evil", ref); err != nil {
		t.Fatal(err)
	}
	if got, want := fs.proc(0).input(), "\x1b[200~a\nb\tc[201~evil\x1b[201~"; got != want {
		t.Fatalf("input = %q, want %q", got, want)
	}
}

// TestPasteTextRefusesInputControls: no control character other than TAB and LF
// is delivered by a paste, bracketed or not, and a refusal sends nothing, not
// even the bracket markers. An ESC inside the text, which used to be silently
// dropped, is refused so that the text the caller sent is the text delivered.
func TestPasteTextRefusesInputControls(t *testing.T) {
	cases := map[string]string{
		"CR":                  "a\rb",
		"CRLF":                "a\r\nb",
		"ESC":                 "a\x1b[201~evil",
		"lone ESC":            "\x1b",
		"NUL":                 "a\x00b",
		"BEL":                 "a\ab",
		"BS":                  "a\bb",
		"DEL":                 "a\x7fb",
		"C1 CSI (U+009B)":     "a\u009bb",
		"C1 NEL (U+0085)":     "a\u0085b",
		"VT":                  "a\vb",
		"FF":                  "a\fb",
		"invalid UTF-8":       "a\xffb",
		"empty":               "",
		"over the size limit": strings.Repeat("x", term.MaxPasteBytes+1),
	}
	for _, bracketed := range []bool{false, true} {
		for name, text := range cases {
			h, fs := newTestHost(false)
			_, ref := create(t, h, "", "pwsh")
			if bracketed {
				fs.proc(0).show("\x1b[?2004h")
				eventually(t, "bracketed paste on", func() bool { return h.sessions["minion-1"].bracketedPaste() })
			}
			err := h.PasteText(ctx, ref, text, ref)
			if !errors.Is(err, ops.ErrInvalidArgument) {
				t.Errorf("bracketed=%v %s: err = %v, want ErrInvalidArgument", bracketed, name, err)
			}
			if got := fs.proc(0).input(); got != "" {
				t.Errorf("bracketed=%v %s: a refused paste wrote %q", bracketed, name, got)
			}
			h.Close()
		}
	}
}

// The control validation runs before the KEEP check and before any pane is
// looked up: an invalid text is an argument error even for a stale pane.
func TestPasteTextValidatesBeforeTheKeepCheck(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	stale := term.PaneRef{ID: ref.ID, PID: ref.PID + 1, Command: ref.Command}
	if err := h.PasteText(ctx, ref, "a\rb", stale); !errors.Is(err, ops.ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
	if got := fs.proc(0).input(); got != "" {
		t.Fatalf("wrote %q", got)
	}
}

// The host announces the verbatim paste, so that the Worker advertises it to
// the Hub (spine AM-13).
func TestHostIsVerbatim(t *testing.T) {
	h, _ := newTestHost(false)
	defer h.Close()
	if !term.IsVerbatim(h) {
		t.Fatal("the ConPTY host does not announce the verbatim paste")
	}
}

func TestPasteTextLarge(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	text := strings.Repeat("x", 3*pasteChunk+17)
	if err := h.PasteText(ctx, ref, text, ref); err != nil {
		t.Fatal(err)
	}
	if got := fs.proc(0).input(); got != text {
		t.Fatalf("input length %d, want %d", len(got), len(text))
	}
}

func TestKeepRecheckPasteText(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")

	for name, expect := range map[string]term.PaneRef{
		"other pid":     {ID: ref.ID, PID: ref.PID + 1, Command: ref.Command},
		"other command": {ID: ref.ID, PID: ref.PID, Command: "cmd"},
		"other id":      {ID: "%77", PID: ref.PID, Command: ref.Command},
	} {
		err := h.PasteText(ctx, ref, "secret", expect)
		if !errors.Is(err, term.ErrPaneChanged) {
			t.Errorf("%s: err = %v, want ErrPaneChanged", name, err)
		}
	}
	if got := fs.proc(0).input(); got != "" {
		t.Fatalf("a KEEP failure wrote %q", got)
	}
}

func TestKeepRecheckSendKeys(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	stale := term.PaneRef{ID: ref.ID, PID: ref.PID + 1, Command: ref.Command}
	n, err := h.SendKeys(ctx, ref, []string{"a", "Enter"}, stale)
	if n != 0 || !errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := fs.proc(0).input(); got != "" {
		t.Fatalf("wrote %q", got)
	}
}

func TestSendKeysEncodesAndCounts(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	n, err := h.SendKeys(ctx, ref, []string{"d", "i", "r", "Enter", "C-c", "Up"}, ref)
	if n != 6 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := fs.proc(0).input(); got != "dir\r\x03\x1b[A" {
		t.Fatalf("input = %q", got)
	}
}

func TestSendKeysRejectsBadKeyBeforeSending(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	n, err := h.SendKeys(ctx, ref, []string{"a", "NoSuchKey"}, ref)
	if n != 0 || err == nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := fs.proc(0).input(); got != "" {
		t.Fatalf("wrote %q before rejecting", got)
	}
}

// A pane that changes in the middle of SendKeys reports the keys already sent.
// A process that has already exited when the call gets the write lock: the
// first re-check refuses, nothing was sent, so the count is 0.
func TestSendKeysRefusedWhenProcessExitedBeforeFirstKey(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	s := h.sessions["minion-1"]
	// Hold the write lock, start the call, let the process exit, release: the
	// first key's re-check already sees the exit.
	s.wmu.Lock()
	type res struct {
		n   int
		err error
	}
	done := make(chan res, 1)
	go func() { n, err := h.SendKeys(ctx, ref, []string{"a", "b"}, ref); done <- res{n, err} }()
	fs.proc(0).exitNow()
	waitExited(t, s)
	s.wmu.Unlock()
	r := <-done
	if r.n != 0 || !errors.Is(r.err, term.ErrPaneChanged) {
		t.Fatalf("n=%d err=%v", r.n, r.err)
	}
	if got := fs.proc(0).input(); got != "" {
		t.Fatalf("input = %q", got)
	}
}

// AD-15: once a key is sent the outcome is never "not sent". The pane changes
// after the first key: the call reports the one key it sent together with
// ErrPaneChanged.
func TestSendKeysPartialCountWhenPaneChangesAfterFirstKey(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	s := h.sessions["minion-1"]
	p := fs.proc(0)
	p.setAfterWrite(func(n int) {
		if n == 1 {
			p.exitNow()
			waitExited(t, s)
		}
	})
	n, err := h.SendKeys(ctx, ref, []string{"a", "b", "c"}, ref)
	if n != 1 || !errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := p.input(); got != "a" {
		t.Fatalf("input = %q, want only the first key", got)
	}
}

func TestWriteErrorIsReportedWithSentCount(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	fs.proc(0).setWriteErr(errors.New("pipe closed"))
	n, err := h.SendKeys(ctx, ref, []string{"a"}, ref)
	if n != 0 || err == nil || errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if err := h.PasteText(ctx, ref, "x", ref); err == nil || errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("err=%v", err)
	}
}

// An exited program keeps its last screen, and writes to it are refused.
func TestExitedSessionKeepsScreen(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "cmd", "/c", "echo", "hi")
	p := fs.proc(0)
	p.show("hi\r\n")
	p.exitNow()
	waitScreen(t, h, ref, "hi")
	// Poll the exit flag, not PasteText: a poll before the watcher marks the
	// exit would itself write to the program.
	waitExited(t, h.sessions["minion-1"])
	if err := h.PasteText(ctx, ref, "x", ref); !errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("PasteText after exit: %v", err)
	}
	if got := p.input(); got != "" {
		t.Fatalf("wrote %q to an exited program", got)
	}
	if got := snapshotText(t, h, ref); got != "hi" {
		t.Fatalf("screen after exit = %q", got)
	}
	list, _ := h.ListSessions(ctx)
	if len(list) != 1 {
		t.Fatalf("exited session vanished: %+v", list)
	}
	// The pseudo console is closed after the output drained.
	eventually(t, "reader finished", func() bool {
		select {
		case <-h.sessions["minion-1"].readDone:
			return true
		default:
			return false
		}
	})
}

// Output that arrives right before the exit is not lost.
func TestLateOutputBeforeCloseIsKept(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "cmd")
	p := fs.proc(0)
	p.exitNow()
	time.Sleep(5 * time.Millisecond)
	p.show("late")
	waitScreen(t, h, ref, "late")
}

// The host answers the queries ConPTY sends itself, so the program is not left
// waiting for a terminal.
func TestAnswersCursorPositionQuery(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	create(t, h, "", "pwsh")
	p := fs.proc(0)
	p.show("abc\x1b[6n")
	eventually(t, "DSR answer", func() bool { return p.input() == "\x1b[1;4R" })
}

func TestSetOptionTask(t *testing.T) {
	h, _ := newTestHost(false)
	defer h.Close()
	s, _ := create(t, h, "")
	if err := h.SetOption(ctx, s.Name, "@minion_task", "fix the build"); err != nil {
		t.Fatal(err)
	}
	list, _ := h.ListSessions(ctx)
	if list[0].Task != "fix the build" {
		t.Fatalf("task = %q", list[0].Task)
	}
	if err := h.SetOption(ctx, "nope", "@minion_task", "x"); !errors.Is(err, term.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := h.SetOption(ctx, s.Name, "@minion_task", strings.Repeat("x", maxOptionLen+1)); err == nil {
		t.Fatal("oversized option accepted")
	}
	for i := 0; i < maxOptions+1; i++ {
		if err := h.SetOption(ctx, s.Name, fmt.Sprintf("@k%d", i), "v"); err != nil {
			if i < maxOptions-1 {
				t.Fatalf("option %d refused early: %v", i, err)
			}
			return
		}
	}
	t.Fatal("option count is not bounded")
}

func TestCancelledContext(t *testing.T) {
	h, _ := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.ListSessions(c); err == nil {
		t.Error("ListSessions ignored a cancelled context")
	}
	if _, err := h.Snapshot(c, ref); err == nil {
		t.Error("Snapshot ignored a cancelled context")
	}
	if err := h.PasteText(c, ref, "x", ref); err == nil {
		t.Error("PasteText ignored a cancelled context")
	}
	if n, err := h.SendKeys(c, ref, []string{"a"}, ref); err == nil || n != 0 {
		t.Error("SendKeys ignored a cancelled context")
	}
	if _, err := h.CreateSession(c, "", "", nil); err == nil {
		t.Error("CreateSession ignored a cancelled context")
	}
}

// AC4: sessions live in the Worker process only. A new host (a Worker restart)
// knows no session.
func TestRestartLosesSessions(t *testing.T) {
	h, _ := newTestHost(false)
	create(t, h, "")
	create(t, h, "")
	if list, _ := h.ListSessions(ctx); len(list) != 2 {
		t.Fatalf("sessions = %d", len(list))
	}
	_ = h.Close()

	h2, _ := newTestHost(false)
	defer h2.Close()
	list, err := h2.ListSessions(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("after restart: %+v, %v", list, err)
	}
	if _, err := h2.ResolvePane(ctx, ops.Target{SessionName: "minion-1"}); !errors.Is(err, term.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestCloseKillsProgramsAndRefusesNewSessions(t *testing.T) {
	h, fs := newTestHost(false)
	create(t, h, "")
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if fs.proc(0).kills != 1 {
		t.Fatalf("kills = %d", fs.proc(0).kills)
	}
	if _, err := h.CreateSession(ctx, "", "", nil); err == nil {
		t.Fatal("closed host created a session")
	}
}

// The race detector covers concurrent output, snapshots and writes.
func TestConcurrentUse(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	p := fs.proc(0)
	stop := make(chan struct{})
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				p.show(fmt.Sprintf("row %d\r\n\x1b[6n", i))
			}
		}
	}()
	for i := 0; i < 50; i++ {
		_, _ = h.Snapshot(ctx, ref)
		_, _ = h.Capture(ctx, ref, 10)
		_ = h.PasteText(ctx, ref, "x", ref)
		_, _ = h.SendKeys(ctx, ref, []string{"Enter"}, ref)
		_, _ = h.ListSessions(ctx)
	}
	close(stop)
}

// The pseudo console is closed exactly once per session, however the session
// ends: a second close of a real pseudo console is a use-after-free.
func TestPseudoConsoleClosedExactlyOnce(t *testing.T) {
	h, fs := newTestHost(false)
	_, _ = create(t, h, "", "pwsh") // ends by exiting
	_, _ = create(t, h, "", "pwsh") // is still live at Close
	fs.proc(0).exitNow()
	waitExited(t, h.sessions["minion-1"])
	eventually(t, "exited session closed by the watcher", func() bool { return fs.proc(0).closeCount() == 1 })
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Let the watcher of the killed session finish its own close attempt.
	for _, name := range []string{"minion-1", "minion-2"} {
		select {
		case <-h.sessions[name].stop:
		case <-time.After(2 * time.Second):
			t.Fatalf("watcher of %s did not finish", name)
		}
	}
	for i := 0; i < 2; i++ {
		if n := fs.proc(i).closeCount(); n != 1 {
			t.Errorf("session %d: Close called %d times, want 1", i+1, n)
		}
	}
}

// A bare program name is resolved through PATH to an absolute path before the
// working directory is applied; the pseudo console library would otherwise
// look for it inside the working directory.
func TestBareProgramWithCwdIsResolvedThroughLook(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	if _, err := h.CreateSession(ctx, "", `C:\work`, []string{"cmd", "/c", "cd"}); err != nil {
		t.Fatal(err)
	}
	got := fs.specs[0]
	if got.path != `C:\fakebin\cmd.exe` || got.dir != `C:\work` || strings.Join(got.args, " ") != "/c cd" {
		t.Fatalf("spec = %+v", got)
	}
	// The default shell is resolved too.
	if _, err := h.CreateSession(ctx, "", `C:\work`, nil); err != nil {
		t.Fatal(err)
	}
	if got := fs.specs[1]; got.path != `C:\fakebin\cmd.exe` {
		t.Fatalf("default shell spec = %+v", got)
	}
	// A program with a path is used as given.
	if _, err := h.CreateSession(ctx, "", `C:\work`, []string{`C:\tools\x.exe`}); err != nil {
		t.Fatal(err)
	}
	if got := fs.specs[2]; got.path != `C:\tools\x.exe` {
		t.Fatalf("spec = %+v", got)
	}
	// An unknown name is a clear error and starts nothing.
	if _, err := h.CreateSession(ctx, "", `C:\work`, []string{"missing-prog"}); err == nil ||
		!strings.Contains(err.Error(), `"missing-prog" not found`) {
		t.Fatalf("err = %v", err)
	}
	if len(fs.specs) != 3 {
		t.Fatalf("a missing program was started: %d specs", len(fs.specs))
	}
}

// A request cancelled while it waited for the write lock sends nothing.
func TestCancelledWhileQueuedSendsNothing(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	s := h.sessions["minion-1"]
	cctx, cancel := context.WithCancel(ctx)
	s.wmu.Lock()
	errs := make(chan error, 2)
	go func() { errs <- h.PasteText(cctx, ref, "danger", ref) }()
	go func() { _, err := h.SendKeys(cctx, ref, []string{"Enter"}, ref); errs <- err }()
	time.Sleep(50 * time.Millisecond) // both calls are queued on the lock
	cancel()
	s.wmu.Unlock()
	for i := 0; i < 2; i++ {
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	}
	if got := fs.proc(0).input(); got != "" {
		t.Fatalf("cancelled requests sent %q", got)
	}
}

// A bracketed paste that is cancelled between chunks still ends its bracket,
// so the program does not stay in paste mode.
func TestCancelledBracketedPasteIsTerminated(t *testing.T) {
	h, fs := newTestHost(false)
	defer h.Close()
	_, ref := create(t, h, "", "pwsh")
	p := fs.proc(0)
	p.show("\x1b[?2004h")
	eventually(t, "bracketed paste enabled", func() bool { return h.sessions["minion-1"].bracketedPaste() })
	cctx, cancel := context.WithCancel(ctx)
	p.setAfterWrite(func(n int) {
		if n == 2 { // the start marker and the first chunk are out
			cancel()
		}
	})
	err := h.PasteText(cctx, ref, strings.Repeat("x", 3*pasteChunk), ref)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	got := p.input()
	if !strings.HasPrefix(got, bracketStart) || !strings.HasSuffix(got, bracketEnd) || strings.Count(got, bracketEnd) != 1 {
		t.Fatalf("paste not framed: %d bytes, starts %q ends %q", len(got), got[:min(8, len(got))], got[max(0, len(got)-8):])
	}
	if len(got) >= 3*pasteChunk {
		t.Fatalf("paste was not cancelled: %d bytes", len(got))
	}
}
