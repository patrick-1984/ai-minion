//go:build windows

package conpty

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// These tests run on the Windows runner (AD-25, story 8.1): they start real
// programs in a real pseudo console.

func winEventually(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func winScreen(t *testing.T, h *Host, ref term.PaneRef) string {
	t.Helper()
	scr, err := h.Snapshot(context.Background(), ref)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return strings.Join(scr.Lines, "\n")
}

func newWinHost(t *testing.T) *Host {
	t.Helper()
	h := New()
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// A short command exits at once; its output stays readable.
func TestWindowsCmdEcho(t *testing.T) {
	h := newWinHost(t)
	ctx := context.Background()
	s, err := h.CreateSession(ctx, "", "", []string{"cmd", "/c", "echo", "minion-conpty-ok"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.Name != "minion-1" || len(s.Panes) != 1 {
		t.Fatalf("session = %+v", s)
	}
	ref := s.Panes[0].Ref
	if ref.PID == 0 || ref.Command != "cmd" {
		t.Fatalf("ref = %+v", ref)
	}
	winEventually(t, "echo output", 15*time.Second, func() bool {
		return strings.Contains(winScreen(t, h, ref), "minion-conpty-ok")
	})
	// The process is gone: a write is refused by the KEEP re-check.
	// Poll the exit flag, not PasteText: a poll before the exit is noticed
	// would itself type into the program.
	sess := h.sessions[s.Name]
	winEventually(t, "exit seen", 10*time.Second, func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.exited
	})
	if err := h.PasteText(ctx, ref, "x", ref); !errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("PasteText after exit: %v", err)
	}
}

// A long-lived PowerShell (or cmd when pwsh is not installed) takes text and
// keys and shows the result.
func TestWindowsInteractiveShell(t *testing.T) {
	h := newWinHost(t)
	ctx := context.Background()
	s, err := h.CreateSession(ctx, "minion-9", "", nil)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	ref := s.Panes[0].Ref
	if ref.Command != "pwsh" && ref.Command != "cmd" {
		t.Fatalf("default program = %q", ref.Command)
	}
	// Give the shell time to print its prompt before typing.
	time.Sleep(2 * time.Second)

	if err := h.PasteText(ctx, ref, "echo minion-paste-ok", ref); err != nil {
		t.Fatalf("PasteText: %v", err)
	}
	if n, err := h.SendKeys(ctx, ref, []string{"Enter"}, ref); err != nil || n != 1 {
		t.Fatalf("SendKeys: %d, %v", n, err)
	}
	winEventually(t, "echoed text on the screen", 20*time.Second, func() bool {
		return strings.Count(winScreen(t, h, ref), "minion-paste-ok") >= 2 // the typed line and its output
	})

	// KEEP: a stale process id writes nothing and reports the change.
	stale := ref
	stale.PID++
	if err := h.PasteText(ctx, ref, "echo must-not-appear", stale); !errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("stale pid: err = %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if strings.Contains(winScreen(t, h, ref), "must-not-appear") {
		t.Fatal("text reached the shell despite the KEEP re-check")
	}

	// The paste contract on the real pseudo console: a CR or an ESC in the text
	// is refused before anything is sent, so it cannot press Enter or start a
	// control sequence in the shell.
	for _, text := range []string{"echo cr-must-not-run\r", "echo esc-must-not-run\x1b[201~"} {
		if err := h.PasteText(ctx, ref, text, ref); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Fatalf("PasteText(%q): err = %v, want ErrInvalidArgument", text, err)
		}
	}
	time.Sleep(500 * time.Millisecond)
	if screen := winScreen(t, h, ref); strings.Contains(screen, "cr-must-not-run") || strings.Contains(screen, "esc-must-not-run") {
		t.Fatal("a refused paste reached the shell")
	}

	list, err := h.ListSessions(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "minion-9" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

// A new host knows no session: a Worker restart ends them all.
func TestWindowsRestartHasNoSessions(t *testing.T) {
	h := newWinHost(t)
	ctx := context.Background()
	if _, err := h.CreateSession(ctx, "", "", []string{"cmd", "/c", "echo", "x"}); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	list, err := New().ListSessions(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("new host lists %+v, %v", list, err)
	}
}

// A bare program name together with a working directory runs the program found
// through PATH, in that directory; go-pty alone would look for it inside the
// directory.
func TestWindowsBareProgramWithCwd(t *testing.T) {
	h := newWinHost(t)
	ctx := context.Background()
	dir := t.TempDir()
	s, err := h.CreateSession(ctx, "", dir, []string{"cmd", "/c", "cd"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	ref := s.Panes[0].Ref
	if ref.Command != "cmd" {
		t.Fatalf("ref = %+v", ref)
	}
	winEventually(t, "the working directory on the screen", 15*time.Second, func() bool {
		return strings.Contains(winScreen(t, h, ref), dir)
	})
}

func TestWindowsStartFailure(t *testing.T) {
	h := newWinHost(t)
	if _, err := h.CreateSession(context.Background(), "", "", []string{"definitely-not-a-program-xyz"}); err == nil {
		t.Fatal("starting a missing program succeeded")
	}
}

// readKeyReport starts a program that reads one console key event and prints
// it, sends the tmux key name and returns the printed "Key:Modifiers" report.
func readKeyReport(t *testing.T, key string) string {
	t.Helper()
	h := newWinHost(t)
	ctx := context.Background()
	script := `Write-Host ('RE'+'ADY'); $k=[Console]::ReadKey($true); Write-Host ('KEY'+'REPORT[{0}|{1}]' -f $k.Key,$k.Modifiers)`
	s, err := h.CreateSession(ctx, "", "", []string{"powershell", "-NoProfile", "-NonInteractive", "-Command", script})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	ref := s.Panes[0].Ref
	winEventually(t, "the program waiting for a key", 30*time.Second, func() bool {
		return strings.Contains(winScreen(t, h, ref), "READY")
	})
	if n, err := h.SendKeys(ctx, ref, []string{key}, ref); err != nil || n != 1 {
		t.Fatalf("SendKeys(%s): %d, %v", key, n, err)
	}
	var report string
	winEventually(t, "the key report for "+key, 20*time.Second, func() bool {
		scr := winScreen(t, h, ref)
		i := strings.Index(scr, "KEYREPORT[")
		if i < 0 {
			return false
		}
		j := strings.Index(scr[i:], "]")
		if j < 0 {
			return false
		}
		report = scr[i+len("KEYREPORT[") : i+j]
		return true
	})
	return report
}

// Shift+Enter reaches the program as Enter with the Shift modifier, not as a
// plain Enter (which would submit pending input instead of adding a newline).
func TestWindowsShiftEnterIsNotEnter(t *testing.T) {
	plain := readKeyReport(t, "Enter")
	shifted := readKeyReport(t, "S-Enter")
	if plain != "Enter|0" {
		t.Errorf("Enter reported %q, want Enter|0", plain)
	}
	if shifted != "Enter|Shift" {
		t.Errorf("S-Enter reported %q, want Enter|Shift", shifted)
	}
}
