package tmuxtest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeT records what the harness reports. Fatalf and Skipf end the calling
// goroutine like the real testing.T does, so run them through runFake.
type fakeT struct {
	dir      string
	cleanups []func()
	fatal    string
	skipped  string
	logs     []string
}

func (f *fakeT) Helper()                      {}
func (f *fakeT) TempDir() string              { return f.dir }
func (f *fakeT) Cleanup(fn func())            { f.cleanups = append(f.cleanups, fn) }
func (f *fakeT) Logf(format string, a ...any) { f.logs = append(f.logs, fmt.Sprintf(format, a...)) }
func (f *fakeT) Fatalf(format string, a ...any) {
	f.fatal = fmt.Sprintf(format, a...)
	runtime.Goexit()
}
func (f *fakeT) Skipf(format string, a ...any) {
	f.skipped = fmt.Sprintf(format, a...)
	runtime.Goexit()
}

// runFake runs fn on its own goroutine, so Goexit from Fatalf/Skipf ends only
// that goroutine, then runs the registered cleanups like testing.T does.
func runFake(f *fakeT, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	<-done
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

func envWith(kv ...string) func() []string { return func() []string { return kv } }

func missingTmux(string) (string, error) { return "", errors.New("executable file not found in $PATH") }

func TestDefaultSocketPath(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		uid     int
		want    string
	}{
		{"no TMUX_TMPDIR", []string{"HOME=/h"}, 1000, "/tmp/tmux-1000/default"},
		{"empty TMUX_TMPDIR", []string{"TMUX_TMPDIR="}, 0, "/tmp/tmux-0/default"},
		{"TMUX_TMPDIR set", []string{"TMUX_TMPDIR=/run/x"}, 501, "/run/x/tmux-501/default"},
		{"trailing slash", []string{"TMUX_TMPDIR=/run/x/"}, 7, "/run/x/tmux-7/default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := defaultSocketPath(c.environ, c.uid); got != c.want {
				t.Fatalf("defaultSocketPath = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSameSocket(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !sameSocket(filepath.Join(real, "default"), filepath.Join(real, ".", "default")) {
		t.Error("cleaned equal paths must match")
	}
	if !sameSocket(filepath.Join(real, "default"), filepath.Join(link, "default")) {
		t.Error("a path through a symlinked directory must match")
	}
	if sameSocket(filepath.Join(real, "default"), filepath.Join(real, "tmux.sock")) {
		t.Error("different files must not match")
	}
	if sameSocket(filepath.Join(real, "default"), filepath.Join(root, "other", "default")) {
		t.Error("different directories must not match")
	}
}

func TestFilterEnv(t *testing.T) {
	in := []string{"PATH=/bin", "TMUX=/tmp/tmux-1/default,12,0", "TMUX_PANE=%3", "TMUX_TMPDIR=/x", "TMUXX=1", "A=B=C"}
	got := filterEnv(in)
	want := []string{"PATH=/bin", "TMUX_TMPDIR=/x", "TMUXX=1", "A=B=C"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("filterEnv = %q, want %q", got, want)
	}
	if len(in) != 6 {
		t.Fatal("filterEnv must not modify its input")
	}
}

func TestGetenvLastWins(t *testing.T) {
	if got := getenv([]string{"A=1", "B=2", "A=3"}, "A"); got != "3" {
		t.Fatalf("getenv = %q, want 3", got)
	}
	if got := getenv([]string{"AB=1"}, "A"); got != "" {
		t.Fatalf("getenv matched a prefix: %q", got)
	}
}

// TestRefusesDefaultSocket is the default-socket AC: a computed socket equal
// to <TMUX_TMPDIR>/tmux-<uid>/default fails the test before any tmux runs.
func TestRefusesDefaultSocket(t *testing.T) {
	tmpdir := t.TempDir()
	uid := 4242
	f := &fakeT{dir: filepath.Join(tmpdir, fmt.Sprintf("tmux-%d", uid))}
	cfg := config{
		lookPath:   func(string) (string, error) { return "/nonexistent/tmux-must-not-run", nil },
		environ:    envWith("TMUX_TMPDIR=" + tmpdir),
		uid:        uid,
		socketName: "default", // forces socket == default socket
	}
	runFake(f, func() { newServer(f, cfg) })
	if !strings.Contains(f.fatal, "default tmux socket") {
		t.Fatalf("expected a fatal about the default socket, got fatal=%q skipped=%q", f.fatal, f.skipped)
	}
	if len(f.cleanups) != 0 {
		t.Fatal("no cleanup (and so no kill-server) may be registered for a refused socket")
	}
}

func TestRefusesDefaultSocketWithoutTmuxTmpdir(t *testing.T) {
	// /tmp/tmux-<uid>/default as the computed path: TempDir is faked to /tmp/tmux-<uid>.
	uid := os.Getuid()
	f := &fakeT{dir: filepath.Join("/tmp", fmt.Sprintf("tmux-%d", uid))}
	cfg := config{
		lookPath:   func(string) (string, error) { return "/nonexistent/tmux-must-not-run", nil },
		environ:    envWith("HOME=/h"),
		uid:        uid,
		socketName: "default",
	}
	runFake(f, func() { newServer(f, cfg) })
	if !strings.Contains(f.fatal, "default tmux socket") {
		t.Fatalf("expected a fatal about the default socket, got %q", f.fatal)
	}
}

func TestRefusesTooLongSocketPath(t *testing.T) {
	f := &fakeT{dir: "/tmp/" + strings.Repeat("d", maxSocketPath)}
	cfg := config{
		lookPath:   func(string) (string, error) { return "/nonexistent/tmux-must-not-run", nil },
		environ:    envWith(),
		uid:        1,
		socketName: socketFile,
	}
	runFake(f, func() { newServer(f, cfg) })
	if !strings.Contains(f.fatal, "too long") {
		t.Fatalf("expected a too-long fatal, got %q", f.fatal)
	}
}

func TestMissingTmuxSkips(t *testing.T) {
	f := &fakeT{dir: t.TempDir()}
	cfg := config{lookPath: missingTmux, environ: envWith("HOME=/h"), uid: 1, socketName: socketFile}
	runFake(f, func() { newServer(f, cfg) })
	if f.fatal != "" {
		t.Fatalf("missing tmux must skip, got fatal %q", f.fatal)
	}
	if !strings.Contains(f.skipped, "tmux is not installed") || !strings.Contains(f.skipped, requireEnv) {
		t.Fatalf("skip message is not clear: %q", f.skipped)
	}
}

func TestMissingTmuxFailsWhenRequired(t *testing.T) {
	f := &fakeT{dir: t.TempDir()}
	cfg := config{lookPath: missingTmux, environ: envWith(requireEnv + "=1"), uid: 1, socketName: socketFile}
	runFake(f, func() { newServer(f, cfg) })
	if f.skipped != "" {
		t.Fatalf("with %s=1 a missing tmux must fail, got skip %q", requireEnv, f.skipped)
	}
	if !strings.Contains(f.fatal, "tmux is not installed") {
		t.Fatalf("fatal message is not clear: %q", f.fatal)
	}
}

func TestMissingTmuxOtherRequireValueSkips(t *testing.T) {
	f := &fakeT{dir: t.TempDir()}
	cfg := config{lookPath: missingTmux, environ: envWith(requireEnv + "=0"), uid: 1, socketName: socketFile}
	runFake(f, func() { newServer(f, cfg) })
	if f.skipped == "" || f.fatal != "" {
		t.Fatalf("%s=0 must skip: skipped=%q fatal=%q", requireEnv, f.skipped, f.fatal)
	}
}

func TestBaseArgs(t *testing.T) {
	s := &Server{socket: "/x/tmux.sock"}
	got := strings.Join(s.baseArgs(), " ")
	if got != "-S /x/tmux.sock -f /dev/null" {
		t.Fatalf("baseArgs = %q", got)
	}
}

// --- tests against a real tmux (skipped without tmux, failed with MINION_REQUIRE_TMUX=1) ---

func TestNewStartsPrivateServer(t *testing.T) {
	srv := New(t)
	sock := srv.Socket()
	if filepath.Base(sock) != "tmux.sock" {
		t.Fatalf("unexpected socket name %q", sock)
	}
	// Every t.TempDir call returns a sibling (001, 002, ...) below one per-test
	// directory, so the socket must sit in a directory next to what TempDir
	// returns now.
	if got, want := filepath.Dir(filepath.Dir(sock)), filepath.Dir(t.TempDir()); got != want {
		t.Fatalf("socket %q is not below the test temp dir %q", sock, want)
	}
	if sameSocket(sock, defaultSocketPath(os.Environ(), os.Getuid())) {
		t.Fatal("socket equals the default socket")
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket file missing: %v", err)
	}
	// The server stays up without any session (exit-empty off).
	time.Sleep(200 * time.Millisecond)
	out, err := srv.run("display-message", "-p", "#{socket_path}")
	if err != nil || !sameSocket(strings.TrimSpace(out), sock) {
		t.Fatalf("server not answering on the private socket %s: %v %q", sock, err, out)
	}
}

func TestSessionSendKeysCaptureWaitFor(t *testing.T) {
	srv := New(t)
	target := srv.NewSession("work", "sh")
	srv.WaitFor(target, "$", 5*time.Second)
	srv.SendKeys(target, "echo hello-$((20+22))", "Enter")
	out := srv.WaitFor(target, "hello-42", 5*time.Second)
	if !strings.Contains(out, "hello-42") {
		t.Fatalf("WaitFor returned %q", out)
	}
	if got := srv.Capture(target); !strings.Contains(got, "hello-42") {
		t.Fatalf("Capture = %q", got)
	}
}

func TestNewSessionRunsArgvWithoutShell(t *testing.T) {
	srv := New(t)
	// argv is executed directly: the metacharacter stays literal.
	target := srv.NewSession("argv", "sh", "-c", "echo 'a;b $HOME'; sleep 30")
	srv.WaitFor(target, "a;b $HOME", 5*time.Second)
}

// TestNewSessionSingleArgIsNotShell pins that a single argument is executed
// as a program, not as shell code: tmux alone would run it through
// "default-shell -c", which expands $HOME and splits on ';'.
func TestNewSessionSingleArgIsNotShell(t *testing.T) {
	srv := New(t)
	dir := t.TempDir()
	// A program whose path contains a space and shell metacharacters.
	prog := filepath.Join(dir, "my prog;x $HOME")
	script := "#!/bin/sh\necho single-arg-ran; sleep 30\n"
	if err := os.WriteFile(prog, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	target := srv.NewSession("single", prog)
	srv.WaitFor(target, "single-arg-ran", 5*time.Second)

	// A shell command line as the single argument is a (missing) program name,
	// not shell code: the pane process cannot start, so the session ends without
	// ever printing the shell's expansion.
	srv.NewSession("shellish", `echo "x;y $HOME"; sleep 30`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := srv.run("list-sessions", "-F", "#{session_name}")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "shellish\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session still alive; the single argument was probably run by a shell: %q", srv.Capture(sessionTarget("shellish")))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSessionTargetIsExact recreates the tmux name-resolution collision:
// session "sleep" runs cat, session "work" runs sleep, so the window of "work"
// is named "sleep", and "wor" is a prefix of "work".
func TestSessionTargetIsExact(t *testing.T) {
	srv := New(t)
	sleepTarget := srv.NewSession("sleep", "sh", "-c", "echo marker-in-sleep-session; cat")
	workTarget := srv.NewSession("work", "sleep", "30")
	srv.WaitFor(sleepTarget, "marker-in-sleep-session", 5*time.Second)
	// With a client attached tmux auto-names the window of "work" after its
	// command, "sleep"; a detached server does not, so name it the same way.
	if out, err := srv.run("rename-window", "-t", workTarget, "sleep"); err != nil {
		t.Fatalf("rename-window: %v: %s", err, out)
	}

	// Sanity: the bare name resolves to the window of "work" first, which is the
	// bug the exact target prevents.
	if out, err := srv.capture("sleep"); err != nil || strings.Contains(out, "marker-in-sleep-session") {
		t.Fatalf("expected the bare name to hit the wrong pane in this tmux (err %v): %q", err, out)
	}
	if got := srv.Capture(sleepTarget); !strings.Contains(got, "marker-in-sleep-session") {
		t.Fatalf("target %q captured the wrong pane: %q", sleepTarget, got)
	}
	if out, err := srv.run("display-message", "-p", "-t", workTarget, "#{session_name}"); err != nil || strings.TrimSpace(out) != "work" {
		t.Fatalf("target %q resolved to session %q (%v)", workTarget, out, err)
	}
	// A unique prefix must not match a target.
	if _, err := srv.capture(sessionTarget("wor")); err == nil {
		t.Fatal("a session-name prefix must not resolve")
	}
}

// TestSendKeysLiteralText covers text that tmux would parse as a flag or as a
// command separator.
func TestSendKeysLiteralText(t *testing.T) {
	srv := New(t)
	for i, text := range []string{"-n", "-hello", "--", "semi;", ";", "a\\;", "mid;dle"} {
		// A fresh pane prevents a suffix of an earlier line ("semi;") plus
		// the next typed echo (";") from satisfying ";\n;" before cat has
		// echoed that next line. Otherwise subsequent echoes can interleave.
		target := srv.NewSession("lit"+strconv.Itoa(i), "sh", "-c", "printf 'ready\\n'; exec cat")
		srv.WaitFor(target, "ready\n", 5*time.Second)
		srv.SendKeys(target, text, "Enter")
		srv.WaitFor(target, text+"\n"+text, 5*time.Second) // typed echo, then cat's output
	}
}

func TestEscapeArg(t *testing.T) {
	cases := map[string]string{
		"plain": "plain", "a;b": "a;b", "hello;": `hello\;`, ";": `\;`, `a\;`: `a\\;`, ";;": `;\;`, "": "",
	}
	for in, want := range cases {
		if got := escapeArg(in); got != want {
			t.Errorf("escapeArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDirectArgv(t *testing.T) {
	if got := strings.Join(directArgv([]string{"/p q"}), "|"); got != "env|--|/p q" {
		t.Fatalf("single element: %q", got)
	}
	if got := strings.Join(directArgv([]string{"a", "b"}), "|"); got != "a|b" {
		t.Fatalf("two elements: %q", got)
	}
	if len(directArgv(nil)) != 0 {
		t.Fatal("empty argv must stay empty")
	}
}

// TestSessionArgvSemicolons: argv elements that end in ';' reach the program
// unchanged.
func TestSessionArgvSemicolons(t *testing.T) {
	srv := New(t)
	target := srv.NewSession("semi", "sh", "-c", `printf '[%s][%s]\n' "$0" "$1"; sleep 30`, "end;", ";")
	srv.WaitFor(target, "[end;][;]", 5*time.Second)
}

func TestNewSessionDefaultShell(t *testing.T) {
	srv := New(t)
	target := srv.NewSession("dflt")
	if _, err := srv.waitFor(target, "", 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

// TestChildEnvironmentHasNoAmbientTmuxVars: the ambient TMUX and TMUX_PANE do
// not reach the server; tmux itself then sets TMUX to the private socket and
// TMUX_PANE to the pane of this server.
func TestChildEnvironmentHasNoAmbientTmuxVars(t *testing.T) {
	t.Setenv("TMUX", "/nonexistent/other-server,1,0")
	t.Setenv("TMUX_PANE", "%99")
	srv := New(t)
	target := srv.NewSession("envchk", "sh", "-c", `echo "T=[$TMUX] P=[$TMUX_PANE]"; sleep 30`)
	out := srv.WaitFor(target, "T=[", 5*time.Second)
	if strings.Contains(out, "other-server") || strings.Contains(out, "%99") {
		t.Fatalf("ambient TMUX or TMUX_PANE leaked into the pane: %q", out)
	}
	if !strings.Contains(out, "T=["+srv.Socket()+",") {
		t.Fatalf("pane TMUX does not point at the private socket %s: %q", srv.Socket(), out)
	}
}

func TestWaitForTimeout(t *testing.T) {
	srv := New(t)
	target := srv.NewSession("quiet", "sleep", "30")
	start := time.Now()
	_, err := srv.waitFor(target, "never-appears", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "never-appears") {
		t.Fatalf("expected a timeout error naming the substring, got %v", err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("returned before the timeout")
	}
	_, err = srv.waitFor(sessionTarget("no-such-session"), "x", 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "capture error") {
		t.Fatalf("expected a capture error for a missing target, got %v", err)
	}
}

func TestInvalidSessionNameFails(t *testing.T) {
	srv := New(t)
	f := &fakeT{dir: t.TempDir()}
	srv.t = f
	for _, name := range []string{"", "a:b", "a.b"} {
		f.fatal = ""
		runFake(f, func() { srv.NewSession(name) })
		if !strings.Contains(f.fatal, "invalid session name") {
			t.Errorf("name %q: expected invalid-name fatal, got %q", name, f.fatal)
		}
	}
}

// TestCleanupKillsOnlyItsServer: a second server survives the cleanup of the
// first, and after the cleanup the first server's socket answers nothing and
// its process is gone.
func TestCleanupKillsOnlyItsServer(t *testing.T) {
	other := New(t)
	other.NewSession("keep", "sleep", "60")

	var sock string
	var pid int
	var inner *Server
	t.Run("inner", func(t *testing.T) {
		inner = New(t)
		inner.NewSession("gone", "sleep", "60")
		sock = inner.Socket()
		out, err := inner.run("display-message", "-p", "#{pid}")
		if err != nil {
			t.Fatal(err)
		}
		pid, err = strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			t.Fatalf("pid %q: %v", out, err)
		}
	}) // inner cleanup has run here

	if _, err := inner.run("list-sessions"); err == nil {
		t.Fatalf("server on %s still answers after cleanup", sock)
	}
	if runtime.GOOS != "windows" {
		p, _ := os.FindProcess(pid)
		deadline := time.Now().Add(5 * time.Second)
		for p.Signal(syscall.Signal(0)) == nil && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if p.Signal(syscall.Signal(0)) == nil {
			t.Fatalf("tmux server process %d still alive after cleanup", pid)
		}
	}
	if out, err := other.run("list-sessions"); err != nil || !strings.Contains(out, "keep") {
		t.Fatalf("the other private server was affected: %v %q", err, out)
	}
}

// TestNeverUsesAmbientServer: with TMUX pointing at a socket that does not
// exist, the harness still works, so nothing depends on the ambient server.
func TestNeverUsesAmbientServer(t *testing.T) {
	t.Setenv("TMUX", filepath.Join(t.TempDir(), "ambient")+",1,0")
	srv := New(t)
	target := srv.NewSession("s", "sleep", "30")
	srv.Capture(target)
}

// TestExecSettled is the table of the exec wait on the facts of one pane. The
// first rows are the sequence seen on tmux 3.6 with a delayed child start:
// before the child has a controlling terminal pane_current_command falls back
// to the configured command ("sleep") while the process is still the forked
// tmux server image, so the command text alone must not settle the pane.
func TestExecSettled(t *testing.T) {
	const server = "tmux: server"
	transient := map[string]bool{"tmux": true, "env": true}
	cases := []struct {
		name             string
		paneCmd, comm    string
		alive            bool
		serverComm, prog string
		want             bool
	}{
		{"fallback command before start", "sleep", server, true, server, "", false},
		{"fallback command before start, program given", "sleep", server, true, server, "sleep", false},
		{"forked child reports the server", "tmux", server, true, server, "", false},
		{"exec'd, tmux lookup lags", "tmux", "sleep", true, server, "", false},
		{"env wrapper", "env", "env", true, server, "", false},
		{"exec'd", "sleep", "sleep", true, server, "", true},
		{"exec'd, program matches", "sleep", "sleep", true, server, "/bin/sleep", true},
		{"exec'd another program", "sh", "sh", true, server, "sleep", false},
		{"truncated image name", "averyveryverylongprogram", "averyveryverylo", true, server, "averyveryverylongprogram", true},
		{"process gone", "", "", false, server, "sleep", true},
		{"server image unknown", "sleep", "sleep", true, "", "", true},
	}
	for _, c := range cases {
		if got := execSettled(c.paneCmd, c.comm, c.alive, c.serverComm, c.prog, transient); got != c.want {
			t.Errorf("%s: execSettled(%q, %q, alive=%v, server=%q, program=%q) = %v, want %v",
				c.name, c.paneCmd, c.comm, c.alive, c.serverComm, c.prog, got, c.want)
		}
	}
}
