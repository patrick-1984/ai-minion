//go:build windows && psmux

package tmux

// The psmux spike (v0.9.0): the tmux adapter drives psmux, the tmux-compatible
// server for Windows, through its public SessionHost methods. Run on a Windows
// machine with psmux on PATH:
//
//	go test -tags psmux -run Psmux -v ./internal/worker/term/tmux/
//
// MINION_REQUIRE_PSMUX=1 turns a missing psmux into a failure (the CI job).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// psmuxHost starts a Host on a private psmux server below t.TempDir() and kills
// that server (that socket only) in t.Cleanup.
func psmuxHost(t *testing.T) (*Host, string, string) {
	t.Helper()
	bin, err := exec.LookPath("psmux")
	if err != nil {
		if os.Getenv("MINION_REQUIRE_PSMUX") == "1" {
			t.Fatalf("psmux is required: %v", err)
		}
		t.Skipf("psmux is not installed: %v", err)
	}
	sock := filepath.Join(t.TempDir(), "psmux.sock")
	h, err := New(sock, WithBinary(bin))
	if err != nil {
		t.Fatalf("New on psmux: %v", err)
	}
	t.Logf("psmux %s at %s, socket %s", h.Version(), bin, sock)
	t.Cleanup(func() { _ = exec.Command(bin, "-S", sock, "kill-server").Run() })
	return h, bin, sock
}

func waitFor(t *testing.T, what string, timeout time.Duration, ok func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		done, state := ok()
		if done {
			return
		}
		last = state
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s: not reached within %s; last state:\n%s", what, timeout, last)
}

func TestPsmuxSessionLifecycle(t *testing.T) {
	h, _, _ := psmuxHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if _, err := h.CreateSession(ctx, "spike", "", nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	waitFor(t, "session listed", 20*time.Second, func() (bool, string) {
		list, err := h.ListSessions(ctx)
		if err != nil {
			return false, err.Error()
		}
		for _, s := range list {
			if s.Name == "spike" {
				return true, ""
			}
		}
		return false, "sessions: " + names(list)
	})

	if err := h.SetOption(ctx, "spike", "@minion_task", "psmux spike"); err != nil {
		t.Fatalf("SetOption: %v", err)
	}
	list, err := h.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.Name == "spike" && s.Task != "psmux spike" {
			t.Errorf("task = %q, want the value just set", s.Task)
		}
	}

	pane, err := h.ResolvePane(ctx, ops.Target{SessionName: "spike"})
	if err != nil {
		t.Fatalf("ResolvePane: %v", err)
	}
	// Give the shell time to print its prompt before typing.
	waitFor(t, "shell prompt", 30*time.Second, func() (bool, string) {
		sc, err := h.Snapshot(ctx, pane)
		if err != nil {
			return false, err.Error()
		}
		return strings.TrimSpace(strings.Join(sc.Lines, "")) != "", strings.Join(sc.Lines, "\n")
	})
	if err := h.PasteText(ctx, pane, "echo minion-psmux-ok", pane); err != nil {
		t.Fatalf("PasteText: %v", err)
	}
	if n, err := h.SendKeys(ctx, pane, []string{"Enter"}, pane); err != nil || n != 1 {
		t.Fatalf("SendKeys: sent %d, %v", n, err)
	}
	waitFor(t, "echo output in snapshot", 30*time.Second, func() (bool, string) {
		sc, err := h.Snapshot(ctx, pane)
		if err != nil {
			return false, err.Error()
		}
		return strings.Count(strings.Join(sc.Lines, "\n"), "minion-psmux-ok") >= 2, strings.Join(sc.Lines, "\n")
	})
	sc, err := h.Capture(ctx, pane, 200)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !strings.Contains(strings.Join(sc.Lines, "\n"), "minion-psmux-ok") {
		t.Errorf("capture lacks the output:\n%s", strings.Join(sc.Lines, "\n"))
	}
	if sc.Width <= 0 || sc.Height <= 0 {
		t.Errorf("capture size %dx%d", sc.Width, sc.Height)
	}
}

// A session belongs to the psmux server, not to the Worker: a new Host (a
// restarted Worker) on the same socket finds it and its output again. This is
// what makes a Windows Worker update keep its sessions.
func TestPsmuxSessionSurvivesANewHost(t *testing.T) {
	h, bin, sock := psmuxHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := h.CreateSession(ctx, "keep", "", nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	h2, err := New(sock, WithBinary(bin))
	if err != nil {
		t.Fatalf("second Host: %v", err)
	}
	waitFor(t, "session seen by the new host", 20*time.Second, func() (bool, string) {
		list, err := h2.ListSessions(ctx)
		if err != nil {
			return false, err.Error()
		}
		for _, s := range list {
			if s.Name == "keep" {
				return true, ""
			}
		}
		return false, "sessions: " + names(list)
	})
}

// The socket the Worker uses by default must be the server psmux itself uses
// by default, or "--share-existing all" would not see the user's sessions.
func TestPsmuxReportsItsSocketPath(t *testing.T) {
	_, bin, sock := psmuxHost(t)
	out, err := exec.Command(bin, "-S", sock, "new-session", "-d", "-s", "probe").CombinedOutput()
	if err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	out, err = exec.Command(bin, "-S", sock, "display-message", "-p", "-t", "probe", "#{socket_path}").CombinedOutput()
	if err != nil {
		t.Fatalf("display-message: %v: %s", err, out)
	}
	t.Logf("socket_path for -S %s: %s", sock, strings.TrimSpace(string(out)))
	out, _ = exec.Command(bin, "display-message", "-p", "#{socket_path}").CombinedOutput()
	t.Logf("default socket_path (no -S, may fail without a default server): %s", strings.TrimSpace(string(out)))
}

func names(list []term.Session) string {
	var out []string
	for _, s := range list {
		out = append(out, s.Name)
	}
	return strings.Join(out, ", ")
}
