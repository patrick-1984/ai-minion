package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

func TestNewSessionArgs(t *testing.T) {
	tests := []struct {
		name string
		cwd  string
		argv []string
		want []string
	}{
		{"shell", "", nil,
			[]string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "minion-1"}},
		{"shell with cwd", "/srv/work", nil,
			[]string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "minion-1", "-c", "/srv/work"}},
		{"program", "/srv/work", []string{"claude", "--resume"},
			[]string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "minion-1", "-c", "/srv/work", "--", "claude", "--resume"}},
		{"single element runs without a shell", "", []string{"htop"},
			[]string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "minion-1", "--", "env", "--", "htop"}},
		{"hash in name and cwd", "/srv/#{pid}", nil,
			[]string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "minion-1", "-c", "/srv/##{pid}"}},
		{"semicolon argument", "", []string{"echo", "a;"},
			[]string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "minion-1", "--", "echo", `a\;`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := newSessionArgs("minion-1", tc.cwd, tc.argv)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("args = %q\nwant   %q", got, tc.want)
			}
		})
	}
}

// createHandler answers new-session with a pane id and list-panes with one
// pane of that session.
func createHandler(name string, calls *[][]string) func(string, []string, []byte) (string, error) {
	return func(sub string, args []string, _ []byte) (string, error) {
		*calls = append(*calls, args)
		switch sub {
		case "new-session":
			return "%7\n", nil
		case "list-panes":
			c := codecFromArgs(args)
			return c.frame(row("%7", "", "bash", name)), nil
		}
		return "", errors.New("unexpected " + sub)
	}
}

func TestCreateSessionRunsDetachedOnTheSocket(t *testing.T) {
	var calls [][]string
	s := &script{handler: createHandler("minion-3", &calls)}
	cwd := t.TempDir()
	got, err := s.host(t).CreateSession(context.Background(), "minion-3", cwd, []string{"sh", "-c", "sleep 9"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "minion-3" || len(got.Panes) != 1 || got.Panes[0].Ref.ID != "%7" {
		t.Errorf("session = %+v", got)
	}
	want := []string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "minion-3", "-c", cwd, "--", "sh", "-c", "sleep 9"}
	if fmt.Sprint(s.calls[0]) != fmt.Sprint(want) {
		t.Errorf("new-session args = %q", s.calls[0])
	}
}

func TestNewSessionArgsEscapesFormatsInName(t *testing.T) {
	got := newSessionArgs("a#{pid};", "", nil)
	if want := `a##{pid}\;`; got[len(got)-1] != want {
		t.Errorf("name arg = %q, want %q", got[len(got)-1], want)
	}
}

func TestCreateSessionEqualsInSeveralElementsIsFine(t *testing.T) {
	var calls [][]string
	s := &script{handler: createHandler("minion-1", &calls)}
	if _, err := s.host(t).CreateSession(context.Background(), "minion-1", "", []string{"/opt/a=b/tool", "x=y"}); err != nil {
		t.Fatal(err)
	}
}

// TestCreateSessionEmptyArgvNamesTheShell checks that an empty argv does not
// leave the choice to the default-command of the server.
func TestCreateSessionEmptyArgvNamesTheShell(t *testing.T) {
	for name, tc := range map[string]struct {
		showOptions string // answer to show-options, "" with fail set means an error
		fail        bool
		env         string
		want        string
	}{
		"server default-shell": {"/usr/bin/zsh\n", false, "/bin/bash", "/usr/bin/zsh"},
		"no server, SHELL":     {"", true, "/bin/bash", "/bin/bash"},
		"relative shell":       {"zsh\n", false, "", "/bin/sh"},
		"equals in shell":      {"/opt/a=b/sh\n", false, "/opt/c=d/sh", "/bin/sh"},
		"nothing usable":       {"", true, "", "/bin/sh"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SHELL", tc.env)
			var calls [][]string
			inner := createHandler("minion-1", &calls)
			s := &script{}
			s.handler = func(sub string, args []string, in []byte) (string, error) {
				if sub == "show-options" {
					if tc.fail {
						return "", errors.New("no server running")
					}
					return tc.showOptions, nil
				}
				return inner(sub, args, in)
			}
			if _, err := s.host(t).CreateSession(context.Background(), "minion-1", "", nil); err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(s.calls[0]); got != "[show-options -gqv default-shell]" {
				t.Errorf("first call = %s", got)
			}
			last := s.calls[1]
			if got := last[len(last)-3:]; fmt.Sprint(got) != fmt.Sprint([]string{"env", "--", tc.want}) {
				t.Errorf("new-session tail = %q, want env -- %s", got, tc.want)
			}
		})
	}
}

func TestCreateSessionDuplicate(t *testing.T) {
	s := &script{handler: func(string, []string, []byte) (string, error) {
		return "", errors.New("duplicate session: minion-1")
	}}
	_, err := s.host(t).CreateSession(context.Background(), "minion-1", "", nil)
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("err = %v, want ErrSessionExists", err)
	}
}

func TestCreateSessionRejectsBadInput(t *testing.T) {
	var calls [][]string
	s := &script{handler: createHandler("x", &calls)}
	h := s.host(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		name, cwd string
		argv      []string
	}{
		"empty name":               {"", "", nil},
		"colon in name":            {"a:b", "", nil},
		"dot in name":              {"a.b", "", nil},
		"relative cwd":             {"minion-1", "work", nil},
		"missing cwd":              {"minion-1", filepath.Join(t.TempDir(), "nope"), nil},
		"cwd is a file":            {"minion-1", file, nil},
		"nul in argv":              {"minion-1", "", []string{"a\x00b"}},
		"equals in single program": {"minion-1", "", []string{"/opt/a=b/tool"}},
		"empty program":            {"minion-1", "", []string{""}},
		"newline in name":          {"minion\n1", "", nil},
	}
	for name, tc := range cases {
		if _, err := h.CreateSession(context.Background(), tc.name, tc.cwd, tc.argv); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	if len(s.calls) != 0 {
		t.Errorf("tmux ran for invalid input: %v", s.subs())
	}
}

func TestCreateSessionBadPaneIDFromTmux(t *testing.T) {
	s := &script{handler: func(string, []string, []byte) (string, error) { return "oops\n", nil }}
	if _, err := s.host(t).CreateSession(context.Background(), "minion-1", "", nil); err == nil {
		t.Fatal("accepted a non-pane-id answer")
	}
}

func TestSetOptionArgs(t *testing.T) {
	s := &script{handler: func(string, []string, []byte) (string, error) { return "", nil }}
	h := s.host(t)
	ctx := context.Background()
	if err := h.SetOption(ctx, "minion-1", OptionTask, "fix the build;"); err != nil {
		t.Fatal(err)
	}
	want := []string{"set-option", "-t", "=minion-1:", "--", "@minion_task", `fix the build\;`}
	if fmt.Sprint(s.calls[0]) != fmt.Sprint(want) {
		t.Errorf("args = %q, want %q", s.calls[0], want)
	}
	if err := h.SetOption(ctx, "minion-1", OptionTask, ""); err != nil {
		t.Fatal(err)
	}
	if got := s.calls[1]; got[len(got)-1] != "" {
		t.Errorf("clearing must pass an empty value: %q", got)
	}
}

func TestSetOptionNotFound(t *testing.T) {
	s := &script{handler: func(string, []string, []byte) (string, error) {
		return "", errors.New("no such session: =gone:")
	}}
	err := s.host(t).SetOption(context.Background(), "gone", OptionTask, "x")
	if !errors.Is(err, term.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSetOptionRejectsBadInput(t *testing.T) {
	s := &script{handler: func(string, []string, []byte) (string, error) { return "", nil }}
	h := s.host(t)
	ctx := context.Background()
	cases := map[string][3]string{
		"server option":  {"minion-1", "default-shell", "x"},
		"no at":          {"minion-1", "minion_task", "x"},
		"flag as key":    {"minion-1", "-g", "x"},
		"space in key":   {"minion-1", "@a b", "x"},
		"empty key":      {"minion-1", "", "x"},
		"empty session":  {"", "@a", "x"},
		"colon session":  {"a:b", "@a", "x"},
		"nul value":      {"minion-1", "@a", "x\x00y"},
		"bad utf8":       {"minion-1", "@a", "\xff"},
		"too long value": {"minion-1", "@a", strings.Repeat("x", MaxOptionValue+1)},
	}
	for name, c := range cases {
		if err := h.SetOption(ctx, c[0], c[1], c[2]); !errors.Is(err, ops.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	if len(s.calls) != 0 {
		t.Errorf("tmux ran for invalid input: %v", s.subs())
	}
}

// --- Integration: a real tmux on a private socket (tmuxtest). ---

func TestIntegrationCreateWithArgvRunsDirectly(t *testing.T) {
	h, srv := newRealHost(t)
	ctx := context.Background()
	got, err := h.CreateSession(ctx, "minion-1", "", []string{"sleep", "300"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "minion-1" || len(got.Panes) != 1 || got.Attached {
		t.Fatalf("session = %+v", got)
	}
	if c := got.Panes[0].Ref.Command; c != "sleep" {
		t.Errorf("pane command = %q, want sleep (no shell in between)", c)
	}
	// The same session through the read path.
	ref, err := h.ResolvePane(ctx, ops.Target{SessionName: "minion-1"})
	if err != nil || ref != got.Panes[0].Ref {
		t.Errorf("resolve = %+v %v, want %+v", ref, err, got.Panes[0].Ref)
	}
	_ = srv
}

// markProgram writes an executable /bin/sh script whose path contains a space.
// It prints how many arguments it got and the first one, then stays alive. A
// shell that was asked to run the path as a command line would split it at the
// space and the session would die at once; a program that gets its argument
// from a shell would show a changed argument.
func markProgram(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "my prog")
	body := "#!/bin/sh\necho \"MARK args=$# first=$1\"\nexec sleep 300\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIntegrationCreateSingleElementArgvHasNoShell(t *testing.T) {
	h, srv := newRealHost(t)
	got, err := h.CreateSession(context.Background(), "minion-2", "", []string{markProgram(t)})
	if err != nil {
		t.Fatal(err)
	}
	// The script execs sleep after the session is created, so the pane runs
	// the wrapper for a moment (how long depends on the tmux version and the
	// load); wait for the exec.
	c := got.Panes[0].Ref.Command
	for deadline := time.Now().Add(5 * time.Second); c != "sleep" && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		ss, err := h.ListSessions(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range ss {
			if s.Name == "minion-2" {
				c = s.Panes[0].Ref.Command
			}
		}
	}
	if c != "sleep" {
		t.Errorf("pane command = %q, want sleep (the program ran and exec'd sleep)", c)
	}
	srv.WaitFor("=minion-2:", "MARK args=0 first=", 5*time.Second)
}

func TestIntegrationCreateSeveralElementsArgvIsLiteral(t *testing.T) {
	h, srv := newRealHost(t)
	if _, err := h.CreateSession(context.Background(), "minion-5", "", []string{markProgram(t), "$HOME;x y"}); err != nil {
		t.Fatal(err)
	}
	srv.WaitFor("=minion-5:", "MARK args=1 first=$HOME;x y", 5*time.Second)
}

func TestIntegrationCreateEqualsInPathOfSeveralElements(t *testing.T) {
	h, srv := newRealHost(t)
	dir := filepath.Join(t.TempDir(), "a=b")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	prog := filepath.Join(dir, "prog")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\necho \"MARK args=$#\"\nexec sleep 300\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateSession(context.Background(), "minion-6", "", []string{prog, "x"}); err != nil {
		t.Fatal(err)
	}
	srv.WaitFor("=minion-6:", "MARK args=1", 5*time.Second)
}

// TestIntegrationCreateKeepsFormatCharactersLiteral: tmux expands formats in
// the name and the directory unless '#' is doubled.
func TestIntegrationCreateKeepsFormatCharactersLiteral(t *testing.T) {
	h, _ := newRealHost(t)
	ctx := context.Background()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(base, "dir#{pid}")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	const name = "n#{pid}#h"
	got, err := h.CreateSession(ctx, name, cwd, []string{markProgram(t)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != name {
		t.Errorf("session name = %q, want %q", got.Name, name)
	}
	out, err := h.run(ctx, "display-message", "-p", "-t", got.Panes[0].Ref.ID, "#{pane_current_path}")
	if err != nil {
		t.Fatal(err)
	}
	if p := strings.TrimSpace(string(out)); p != cwd {
		t.Errorf("working directory = %q, want %q", p, cwd)
	}
	// The same name works as an option target and as a duplicate.
	if err := h.SetOption(ctx, name, OptionTask, "t#{pid}"); err != nil {
		t.Errorf("SetOption: %v", err)
	}
	if _, err := h.CreateSession(ctx, name, "", []string{"sleep", "300"}); !errors.Is(err, ErrSessionExists) {
		t.Errorf("duplicate: err = %v, want ErrSessionExists", err)
	}
}

// TestIntegrationCreateEmptyArgvIgnoresDefaultCommand: a server whose
// default-command runs something else must still give a shell.
func TestIntegrationCreateEmptyArgvIgnoresDefaultCommand(t *testing.T) {
	h, srv := newRealHost(t)
	ctx := context.Background()
	if _, err := h.run(ctx, "set-option", "-g", "default-command", "exec sleep 300"); err != nil {
		t.Fatal(err)
	}
	got, err := h.CreateSession(ctx, "minion-7", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Panes[0].Ref.Command; c == "sleep" {
		t.Fatalf("pane command = %q: default-command ran instead of the shell", c)
	}
	target := "=minion-7:"
	srv.SendKeys(target, "echo shell-ok-$((20+22))", "Enter")
	srv.WaitFor(target, "shell-ok-42", 10*time.Second)
}

func TestIntegrationCreateShellAndCwd(t *testing.T) {
	h, srv := newRealHost(t)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateSession(context.Background(), "minion-3", cwd, nil); err != nil {
		t.Fatal(err)
	}
	target := "=minion-3:"
	srv.SendKeys(target, "echo cwd=$(pwd)", "Enter")
	srv.WaitFor(target, "cwd="+cwd, 10*time.Second)
}

func TestIntegrationCreateDuplicateLeavesTheSessionAlone(t *testing.T) {
	h, srv := newRealHost(t)
	ctx := context.Background()
	srv.NewSession("minion-4", "sleep", "300")
	before, err := h.ResolvePane(ctx, ops.Target{SessionName: "minion-4"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.CreateSession(ctx, "minion-4", "", []string{"cat"})
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("err = %v, want ErrSessionExists", err)
	}
	after, err := h.ResolvePane(ctx, ops.Target{SessionName: "minion-4"})
	if err != nil || after != before {
		t.Errorf("existing session changed: %+v -> %+v (%v)", before, after, err)
	}
}

func TestIntegrationSetOptionShowsInList(t *testing.T) {
	h, srv := newRealHost(t)
	ctx := context.Background()
	srv.NewSession("adopted", "sleep", "300")
	task := "fix the build; -x \"quoted\" \u00fcn\u00ef"
	if err := h.SetOption(ctx, "adopted", OptionTask, task); err != nil {
		t.Fatal(err)
	}
	task1 := func() string {
		ss, err := h.ListSessions(ctx)
		if err != nil || len(ss) != 1 {
			t.Fatalf("sessions = %+v %v", ss, err)
		}
		return ss[0].Task
	}
	if got := task1(); got != task {
		t.Errorf("task = %q, want %q", got, task)
	}
	if err := h.SetOption(ctx, "adopted", OptionTask, ""); err != nil {
		t.Fatal(err)
	}
	if got := task1(); got != "" {
		t.Errorf("cleared task = %q", got)
	}
	if err := h.SetOption(ctx, "nosuch", OptionTask, "x"); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("missing session: err = %v, want ErrNotFound", err)
	}
	// Exact match: a prefix of an existing name is not that session.
	if err := h.SetOption(ctx, "adop", OptionTask, "x"); !errors.Is(err, term.ErrNotFound) {
		t.Errorf("prefix of a name: err = %v, want ErrNotFound", err)
	}
}

func TestTransientCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want []string
	}{
		{"shell", []string{"/bin/bash"}, []string{"env", "tmux"}},
		{"one element", []string{"sleep"}, []string{"env", "tmux"}},
		{"several elements", []string{"sh", "-c", "sleep 9"}, []string{"tmux"}},
		{"program is env", []string{"env", "A=b", "sleep"}, []string{"tmux"}},
		{"one element env", []string{"/usr/bin/env"}, []string{"tmux"}},
		{"program is tmux", []string{"tmux", "attach"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := transientCommands(tc.argv)
			if len(got) != len(tc.want) {
				t.Fatalf("transientCommands(%q) = %v, want %v", tc.argv, got, tc.want)
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Errorf("transientCommands(%q) = %v, want %v", tc.argv, got, tc.want)
				}
			}
		})
	}
}

// TestCreateSessionWaitsForTheProgramToExec checks that the session that
// CreateSession returns describes the program and not the forked tmux process
// (or the env wrapper) that has not exec'd it yet.
func TestCreateSessionWaitsForTheProgramToExec(t *testing.T) {
	var lists int
	s := &script{}
	s.handler = func(sub string, args []string, _ []byte) (string, error) {
		switch sub {
		case "new-session":
			return "%7\n", nil
		case "list-panes":
			lists++
			cmd := []string{"tmux", "env", "sleep", "sleep"}[min(lists-1, 3)]
			return codecFromArgs(args).frame(row("%7", "", cmd, "minion-1")), nil
		}
		return "", errors.New("unexpected " + sub)
	}
	got, err := s.host(t).CreateSession(context.Background(), "minion-1", "", []string{"sleep"})
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Panes[0].Ref.Command; c != "sleep" || lists != 3 {
		t.Errorf("command = %q after %d listings, want sleep after 3", c, lists)
	}
}

// TestCreateSessionGivesUpWaitingAfterTheBound checks that a pane that never
// leaves the transient command is returned as it is, so that the identity
// check of a later write (not this wait) decides.
func TestCreateSessionGivesUpWaitingAfterTheBound(t *testing.T) {
	s := &script{}
	s.handler = func(sub string, args []string, _ []byte) (string, error) {
		switch sub {
		case "new-session":
			return "%7\n", nil
		case "list-panes":
			return codecFromArgs(args).frame(row("%7", "", "tmux", "minion-1")), nil
		}
		return "", errors.New("unexpected " + sub)
	}
	start := time.Now()
	got, err := s.host(t).CreateSession(context.Background(), "minion-1", "", []string{"sleep", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Panes[0].Ref.Command; c != "tmux" {
		t.Errorf("command = %q, want the unchanged tmux", c)
	}
	if d := time.Since(start); d < settleTimeout || d > settleTimeout+2*time.Second {
		t.Errorf("waited %v, want about %v", d, settleTimeout)
	}
}

func TestCreateSessionWaitStopsWhenTheContextEnds(t *testing.T) {
	s := &script{}
	s.handler = func(sub string, args []string, _ []byte) (string, error) {
		switch sub {
		case "new-session":
			return "%7\n", nil
		case "list-panes":
			return codecFromArgs(args).frame(row("%7", "", "tmux", "minion-1")), nil
		}
		return "", errors.New("unexpected " + sub)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.host(t).CreateSession(ctx, "minion-1", "", []string{"sleep", "1"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
}
