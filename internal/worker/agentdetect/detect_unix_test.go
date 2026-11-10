//go:build unix

package agentdetect

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
)

// childTree starts a child of the test process and returns the table's
// processes below the test's pid.
func childTree(t *testing.T, src ProcessSource) (Proc, []Proc) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	procs, err := src.Processes(context.Background())
	if err != nil {
		t.Skipf("process table unavailable: %v", err)
	}
	tree := descendants(procs, os.Getpid())
	for _, p := range tree {
		if p.PID == cmd.Process.Pid {
			return p, tree
		}
	}
	t.Fatalf("child %d not found below pid %d in %d processes", cmd.Process.Pid, os.Getpid(), len(procs))
	return Proc{}, nil
}

// TestProcSourceSeesChildren runs against the real /proc: a child of this
// test process is found below the test's pid with its exact argv and its
// job-control facts.
func TestProcSourceSeesChildren(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc is Linux only")
	}
	p, _ := childTree(t, ProcSource{})
	if !reflect.DeepEqual(p.Args, []string{"sleep", "30"}) || p.PPID != os.Getpid() || !p.Running() || p.PGRP <= 0 {
		t.Fatalf("child read as %+v", p)
	}
	if _, ok := DefaultProcessSource().(ProcSource); !ok {
		t.Fatalf("the Linux default source is %T, want ProcSource", DefaultProcessSource())
	}
}

// TestPSSourceSeesChildren runs against ps(1) where it provides the
// job-control columns (macOS, BSD, procps).
func TestPSSourceSeesChildren(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps is not installed")
	}
	p, _ := childTree(t, PSSource{})
	if len(p.Args) != 2 || p.Args[0] != "sleep" || p.PPID != os.Getpid() || !p.Running() || p.PGRP <= 0 {
		t.Fatalf("child read as %+v", p)
	}
}

// requireProcessTable skips the test when the platform's default process
// source cannot be read.
func requireProcessTable(t *testing.T) {
	t.Helper()
	if _, err := DefaultProcessSource().Processes(context.Background()); err != nil {
		t.Skipf("process table unavailable: %v", err)
	}
}

// fakeAgentDir writes an executable script named name that prints the
// recorded screen of testdata/<screen>.txt and then idles, like an agent CLI
// waiting for input. It does not exec: the script stays visible in the
// process table as "/bin/sh <dir>/<name>", as a real agent launcher would, so
// the tree search and not a parsed wrapper argument finds it. The directory
// goes first on the PATH of the pane shell.
func fakeAgentDir(t *testing.T, name, screenFile string) string {
	t.Helper()
	dir := t.TempDir()
	scr, err := filepath.Abs(filepath.Join("testdata", screenFile+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\ncat " + scr + "\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeExitingAgentDir is fakeAgentDir for an agent that exits when it reads a
// line, like a CLI that the user quits.
func fakeExitingAgentDir(t *testing.T, name, screenFile string) string {
	t.Helper()
	dir := t.TempDir()
	scr, err := filepath.Abs(filepath.Join("testdata", screenFile+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\ncat " + scr + "\nread line\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// paneShell starts an interactive bash pane (job control on, as a user's
// shell) and returns its pid, read from the shell itself, together with the
// session target.
func paneShell(t *testing.T, srv *tmuxtest.Server, name, path string) (target string, pid int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	target = srv.NewSession(name, "env", "PS1=$ ", "PATH="+path+":"+os.Getenv("PATH"), "bash", "--norc", "--noprofile")
	srv.SendKeys(target, "echo $$ > "+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile, "Enter")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return target, n
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the pane shell never reported its pid")
	return "", 0
}

func capturedScreen(srv *tmuxtest.Server, target string) ops.Screen {
	lines := strings.Split(strings.TrimRight(srv.Capture(target), "\n"), "\n")
	return ops.Screen{Width: 80, Height: 24, Lines: lines}
}

// classifyPane classifies the pane from the real process table and the
// captured screen.
func classifyPane(srv *tmuxtest.Server, target, command string, pid int) Result {
	return New().Classify(context.Background(), Input{Command: command, PID: pid, Screen: capturedScreen(srv, target)})
}

// eventually polls f until it returns true or the deadline passes.
func eventually(t *testing.T, what string, f func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ok, got := f()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: last result %s", what, got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestFakeAgentInTmux runs a fake agent script that prints a recorded screen
// inside a real tmux pane (private socket) and classifies the pane from the
// real process table and the captured screen: directly, under a "bash -c"
// wrapper that names the agent, and under a "-c" script that does not start
// with the agent (so the tree search must find the descendant).
func TestFakeAgentInTmux(t *testing.T) {
	requireProcessTable(t)
	for _, tc := range []struct {
		name, agent, command, launch, wantState string
	}{
		{"direct", "claude", "claude", "claude", ops.PaneStateStable},
		{"wrapper-names-agent", "claude", "bash", "bash -c 'claude; true'", ops.PaneStateStable},
		{"wrapper-gemini", "gemini", "bash", "bash -c 'gemini; true'", ops.PaneStateStable},
		{"wrapper-script-starts-elsewhere", "claude", "bash", "bash -c 'cd . && claude'", ops.PaneStateStable},
		{"wrapper-busy-codex", "codex", "bash", "bash -c 'cd . && codex; true'", ops.PaneStateBusy},
		{"direct-busy-codex", "codex", "codex", "codex", ops.PaneStateBusy},
	} {
		name := tc.name
		t.Run(name, func(t *testing.T) {
			screenFile := tc.agent + "_idle"
			if tc.wantState == ops.PaneStateBusy {
				screenFile = tc.agent + "_busy"
			}
			dir := fakeAgentDir(t, tc.agent, screenFile)
			srv := tmuxtest.New(t)
			target, pid := paneShell(t, srv, "agent"+strconv.Itoa(len(name)), dir)

			srv.SendKeys(target, tc.launch, "Enter")
			marker := map[string]string{"claude": "for shortcuts", "gemini": "Type your message", "codex": "esc to interrupt"}[tc.agent]
			srv.WaitFor(target, marker, 10*time.Second)

			// The command is what tmux reports as the pane's foreground
			// command: the agent script itself, or the wrapper shell.
			got := classifyPane(srv, target, tc.command, pid)
			if got.Kind != "agent:"+tc.agent || got.State != tc.wantState {
				t.Fatalf("got %s/%s, want agent:%s/%s", got.Kind, got.State, tc.agent, tc.wantState)
			}
		})
	}
}

// AD-12: an agent suspended with Ctrl-Z leaves the pane's input with the
// shell, although its idle screen is still visible above the prompt. The pane
// is a shell again, and the agent returns on "fg".
func TestSuspendedAgentIsShellInTmux(t *testing.T) {
	requireProcessTable(t)
	dir := fakeAgentDir(t, "claude", "claude_idle")
	srv := tmuxtest.New(t)
	target, pid := paneShell(t, srv, "suspended", dir)

	srv.SendKeys(target, "claude", "Enter")
	srv.WaitFor(target, "for shortcuts", 10*time.Second)
	if got := classifyPane(srv, target, "claude", pid); got.Kind != "agent:claude" || got.State != ops.PaneStateStable {
		t.Fatalf("running agent: got %+v", got)
	}
	// Under the shell, the pane's foreground command is the agent script.
	eventually(t, "agent under the shell", func() (bool, string) {
		got := classifyPane(srv, target, "bash", pid)
		return got.Kind == "agent:claude" && got.State == ops.PaneStateStable, got.Kind + "/" + got.State
	})

	srv.SendKeys(target, "C-z")
	srv.WaitFor(target, "Stopped", 10*time.Second)
	eventually(t, "suspended agent", func() (bool, string) {
		got := classifyPane(srv, target, "bash", pid)
		return got.Kind == ops.PaneKindShell && got.State == ops.PaneStateStable, got.Kind + "/" + got.State
	})
	if !strings.Contains(srv.Capture(target), "for shortcuts") {
		t.Fatal("the agent's idle screen is expected to stay visible above the prompt")
	}

	srv.SendKeys(target, "fg", "Enter")
	eventually(t, "resumed agent", func() (bool, string) {
		got := classifyPane(srv, target, "bash", pid)
		return got.Kind == "agent:claude", got.Kind + "/" + got.State
	})
}

// A plain shell pane with no agent classifies as shell, stable, then busy
// while a foreground job runs; a background job leaves it stable.
func TestShellPaneInTmux(t *testing.T) {
	requireProcessTable(t)
	srv := tmuxtest.New(t)
	target, pid := paneShell(t, srv, "shellpane", os.Getenv("PATH"))
	in := func() Result { return classifyPane(srv, target, "bash", pid) }
	if got := in(); got.Kind != "shell" || got.State != ops.PaneStateStable {
		t.Fatalf("idle shell: got %+v", got)
	}

	// A background job: the shell is still waiting for input.
	srv.SendKeys(target, "sleep 300 &", "Enter")
	srv.WaitFor(target, "[1]", 10*time.Second)
	for i := 0; i < 5; i++ {
		if got := in(); got.Kind != "shell" || got.State != ops.PaneStateStable {
			t.Fatalf("shell with a background job: got %+v, want shell/stable", got)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A foreground job: busy.
	srv.SendKeys(target, "sleep 301", "Enter")
	eventually(t, "shell with a foreground job", func() (bool, string) {
		got := in()
		return got.Kind == "shell" && got.State == ops.PaneStateBusy, got.Kind + "/" + got.State
	})
}

// AD-12: after the agent of "bash -c 'claude; bash -i; true'" exits, the pane
// is a shell that accepts input, although the wrapper (the pane's root
// process) keeps the argv that starts with the agent.
func TestExitedAgentUnderRootWrapperInTmux(t *testing.T) {
	requireProcessTable(t)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	dir := fakeExitingAgentDir(t, "claude", "claude_idle")
	pidFile := filepath.Join(t.TempDir(), "pid")
	srv := tmuxtest.New(t)
	script := "echo $$ > " + pidFile + ".tmp && mv " + pidFile + ".tmp " + pidFile + "; claude; PS1='$ ' bash --norc --noprofile -i; true"
	target := srv.NewSession("wrapper", "env", "PATH="+dir+":"+os.Getenv("PATH"), "bash", "-c", script)
	srv.WaitFor(target, "for shortcuts", 10*time.Second)
	var pid int
	eventually(t, "pane pid", func() (bool, string) {
		b, err := os.ReadFile(pidFile)
		n, aerr := strconv.Atoi(strings.TrimSpace(string(b)))
		pid = n
		return err == nil && aerr == nil, string(b)
	})

	eventually(t, "running agent", func() (bool, string) {
		got := classifyPane(srv, target, "bash", pid)
		return got.Kind == "agent:claude" && got.State == ops.PaneStateStable, got.Kind + "/" + got.State
	})

	srv.SendKeys(target, "Enter") // the agent reads a line and exits
	srv.WaitFor(target, "$ ", 10*time.Second)
	for i := 0; i < 10; i++ {
		got := classifyPane(srv, target, "bash", pid)
		if got.IsAgent() {
			t.Fatalf("after the agent exited: got %s/%s, want no agent", got.Kind, got.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// AD-12: an agent that is foreground on another pty (a background "script -q
// -c claude") does not receive the pane's input; the outer shell does. The
// same script in the foreground relays the input and is the agent.
func TestNestedPtyAgentInTmux(t *testing.T) {
	requireProcessTable(t)
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("script(1) is not installed")
	}
	dir := fakeAgentDir(t, "claude", "claude_idle")
	srv := tmuxtest.New(t)
	target, pid := paneShell(t, srv, "nested", dir)

	srv.SendKeys(target, "script -q -c claude /dev/null </dev/null &", "Enter")
	srv.WaitFor(target, "for shortcuts", 10*time.Second)
	for i := 0; i < 10; i++ {
		got := classifyPane(srv, target, "bash", pid)
		if got.Kind != ops.PaneKindShell || got.State != ops.PaneStateStable {
			t.Fatalf("background nested pty: got %s/%s, want shell/stable", got.Kind, got.State)
		}
		time.Sleep(50 * time.Millisecond)
	}

	srv.SendKeys(target, "clear", "Enter")
	srv.SendKeys(target, "script -q -c claude /dev/null", "Enter")
	eventually(t, "foreground script", func() (bool, string) {
		got := classifyPane(srv, target, "bash", pid)
		return got.Kind == "agent:claude", got.Kind + "/" + got.State
	})
}
