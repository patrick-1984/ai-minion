package tmuxtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	// requireEnv makes a missing tmux binary a failure instead of a skip.
	requireEnv = "MINION_REQUIRE_TMUX"

	socketFile = "tmux.sock"

	// Detached sessions get a fixed geometry so captures are deterministic.
	sessionWidth  = "120"
	sessionHeight = "40"

	commandTimeout = 30 * time.Second
	pollInterval   = 50 * time.Millisecond
	shutdownWait   = 5 * time.Second

	// maxSocketPath is a conservative bound under the sockaddr_un path limit
	// (104 bytes on macOS and BSD, 108 on Linux).
	maxSocketPath = 100
)

// testingT is the part of testing.TB the harness uses. It is an interface so
// the harness can be tested with a fake that records Fatalf and Skipf.
type testingT interface {
	Helper()
	TempDir() string
	Cleanup(func())
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// config holds the seams of New: everything that touches the real
// environment of the test process.
type config struct {
	lookPath   func(file string) (string, error)
	environ    func() []string
	uid        int
	socketName string
}

func realConfig() config {
	return config{
		lookPath:   exec.LookPath,
		environ:    os.Environ,
		uid:        os.Getuid(),
		socketName: socketFile,
	}
}

// Server is a tmux server running on a private socket that belongs to one test.
type Server struct {
	t      testingT
	bin    string
	socket string
	env    []string
	// serverImage caches the image name of the server process (serverComm).
	serverImage string
	// proc looks the image name of a pane process up.
	proc procInspector
}

// New starts a tmux server on a private socket below t.TempDir() and kills
// that server (that socket only) in t.Cleanup.
//
// It skips the test when tmux is not installed, except when MINION_REQUIRE_TMUX=1,
// where it fails. It fails the test when the socket path equals the default
// tmux socket of the user.
func New(t testing.TB) *Server {
	t.Helper()
	return newServer(t, realConfig())
}

func newServer(t testingT, cfg config) *Server {
	t.Helper()
	environ := cfg.environ()
	bin, err := cfg.lookPath("tmux")
	if err != nil {
		if getenv(environ, requireEnv) == "1" {
			t.Fatalf("tmuxtest: tmux is not installed (%v) and %s=1 requires it", err, requireEnv)
		} else {
			t.Skipf("tmuxtest: tmux is not installed (%v); install tmux to run this test (set %s=1 to fail instead of skip)", err, requireEnv)
		}
		return nil
	}
	socket := filepath.Join(t.TempDir(), cfg.socketName)
	def := defaultSocketPath(environ, cfg.uid)
	if sameSocket(socket, def) {
		t.Fatalf("tmuxtest: refusing to use %s: it is the default tmux socket of the user", socket)
		return nil
	}
	if len(socket) > maxSocketPath {
		t.Fatalf("tmuxtest: socket path %s is too long for a unix socket (%d > %d bytes); use a shorter test name or a shorter TMPDIR", socket, len(socket), maxSocketPath)
		return nil
	}
	s := &Server{t: t, bin: bin, socket: socket, env: filterEnv(environ), proc: defaultInspector()}
	t.Cleanup(s.shutdown)
	// start-server alone exits at once without a session; exit-empty off keeps
	// the server alive until the cleanup kills it.
	if out, err := s.run("start-server", ";", "set-option", "-g", "exit-empty", "off"); err != nil {
		t.Fatalf("tmuxtest: cannot start tmux on %s: %v: %s", socket, err, out)
		return nil
	}
	return s
}

// Socket returns the path of the private tmux socket.
func (s *Server) Socket() string { return s.socket }

// NewSession creates a detached session named name and returns its target: the
// string "=" + name + ":", which tmux resolves as the exact session name (no
// window-name lookup and no prefix matching) and then that session's current
// window and active pane. Pass the returned target, never the bare name, to
// SendKeys, Capture and WaitFor.
//
// argv is executed directly, without a shell, for every non-empty argv: tmux
// itself execs argv directly only for two or more arguments and runs a single
// argument through the default shell, so a single element is wrapped as
// "env -- argv0". With an empty argv the session runs the default shell.
func (s *Server) NewSession(name string, argv ...string) string {
	s.t.Helper()
	if name == "" || strings.ContainsAny(name, ":.") {
		s.t.Fatalf("tmuxtest: invalid session name %q (empty, or contains ':' or '.')", name)
		return ""
	}
	args := []string{"new-session", "-d", "-s", escapeArg(name), "-x", sessionWidth, "-y", sessionHeight}
	if cmd := directArgv(argv); len(cmd) > 0 {
		args = append(args, "--")
		for _, a := range cmd {
			args = append(args, escapeArg(a))
		}
	}
	if out, err := s.run(args...); err != nil {
		s.t.Fatalf("tmuxtest: new-session %q: %v: %s", name, err, out)
		return ""
	}
	s.waitExec(name, transientCommands(argv))
	return sessionTarget(name)
}

// transientCommands returns the pane_current_command values that only exist
// until the program of a new session has exec'd: "tmux" (tmux forks the pane
// process from the server and reports the server's name until the exec) and,
// for a single-element argv, the "env" wrapper. A name the caller asked for is
// not transient.
func transientCommands(argv []string) map[string]bool {
	t := map[string]bool{"tmux": true}
	if len(argv) == 1 {
		t["env"] = true
	}
	for _, a := range argv {
		delete(t, filepath.Base(a))
	}
	return t
}

// waitExec waits until the pane of a new session has really exec'd its
// program, so that the pane runs the program and not the process that is about
// to exec it when the caller first reads the pane (the identity of the pane
// would change under the caller's hands). It gives up after a bound, without
// failing the test: a program that never execs is the caller's case. It logs
// why when it could not inspect the pane process, because that is never
// treated as a settled pane.
func (s *Server) waitExec(name string, transient map[string]bool) {
	deadline := time.Now().Add(shutdownWait)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var lastErr error
	for time.Now().Before(deadline) {
		settled, err := s.checkPanes(ctx, deadline, []string{"display-message", "-p", "-t", sessionTarget(name), "#{pane_pid} #{pane_current_command}"}, "", transient)
		if settled {
			return
		}
		if err != nil {
			lastErr = err
		}
		time.Sleep(pollInterval / 5)
	}
	if lastErr != nil {
		s.t.Logf("tmuxtest: could not confirm that the pane of %q has exec'd: %v", name, lastErr)
	}
}

// WaitAllExec waits until every pane of this server runs its program: the
// process of the pane (pane_pid) has exec'd out of the forked tmux server
// image, and, when program is not empty, its image is program (compared by base
// name). It fails the test after timeout.
//
// It is a fixture synchronisation for tests that create sessions or panes
// through tmux directly (names that NewSession refuses, or arguments the code
// under test builds itself) and then read the same pane twice. tmux forks the
// pane process from the server and then execs the program, and for that window
// pane_current_command is not a reliable signal in either direction: it says
// "tmux" while the forked child has no controlling terminal and a process
// lookup yields the server, but before that lookup works at all tmux falls back
// to the configured command, so a pane that has not started yet already
// reports "sleep" ("sleep", "tmux", "sleep" was observed on tmux 3.6). The
// process image does not have that window: while the process is the forked
// child it has the image (name) of the server. Waiting for the pane process to
// differ from the server image is therefore valid as synchronisation only; no
// assertion of the code under test depends on it.
//
// The deadline is one for everything: every tmux call and every process
// inspection is bounded by it, and a result that arrives after it does not
// count (the contract of WaitFor). A process that cannot be inspected is never
// a settled pane; the wait retries until the deadline and reports the last
// inspection error.
func (s *Server) WaitAllExec(program string, timeout time.Duration) {
	s.t.Helper()
	if err := s.waitAllExec(program, timeout); err != nil {
		s.t.Fatalf("tmuxtest: %v", err)
	}
}

func (s *Server) waitAllExec(program string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	transient := map[string]bool{"tmux": true}
	var lastErr error
	for {
		settled, err := s.checkPanes(ctx, deadline, []string{"list-panes", "-a", "-F", "#{pane_pid} #{pane_current_command}"}, program, transient)
		if settled {
			return nil
		}
		// An error that only says the deadline cut a call short does not
		// replace the inspection error that explains why the wait was stuck.
		if err != nil && (lastErr == nil || ctx.Err() == nil) {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(pollInterval / 5)
	}
	if lastErr != nil {
		return fmt.Errorf("timeout after %s: a pane has not exec'd %s yet; last error: %v", timeout, programOrAny(program), lastErr)
	}
	return fmt.Errorf("timeout after %s: a pane has not exec'd %s yet", timeout, programOrAny(program))
}

func programOrAny(program string) string {
	if program == "" {
		return "its program"
	}
	return fmt.Sprintf("%q", program)
}

// checkPanes runs the tmux command tmuxArgs, which prints one "pane_pid
// command" line per pane, and reports whether every pane is settled. Every
// step is bounded by ctx, and a verdict reached after deadline is "not
// settled" even when it would be a success, like waitFor: the caller asked for
// an answer by then. An error means the panes could not be inspected, never a
// settled pane.
func (s *Server) checkPanes(ctx context.Context, deadline time.Time, tmuxArgs []string, program string, transient map[string]bool) (bool, error) {
	settled, err := s.panesSettled(ctx, tmuxArgs, program, transient)
	if !time.Now().Before(deadline) {
		return false, err
	}
	return settled, err
}

func (s *Server) panesSettled(ctx context.Context, tmuxArgs []string, program string, transient map[string]bool) (bool, error) {
	out, err := s.runCtx(ctx, tmuxArgs...)
	if err != nil {
		return false, fmt.Errorf("%s: %v: %s", tmuxArgs[0], err, strings.TrimSpace(out))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return false, nil
	}
	serverComm, err := s.serverComm(ctx)
	if err != nil {
		return false, err
	}
	for _, line := range lines {
		pidStr, cmd, _ := strings.Cut(line, " ")
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			return false, fmt.Errorf("unexpected pane line %q", line)
		}
		comm, state, err := s.proc.comm(ctx, pid)
		if err != nil {
			return false, fmt.Errorf("inspecting pane process %d: %w", pid, err)
		}
		if !execSettled(cmd, comm, state == procPresent, serverComm, program, transient) {
			return false, nil
		}
	}
	return true, nil
}

// execSettled is the predicate of the exec wait, on the facts of one pane:
// the pane_current_command text, and the image name of the pane process
// (comm, alive) next to that of the tmux server. alive is false only for a
// process whose absence was confirmed, never for one that could not be
// inspected.
//
//   - A pane process that is gone is settled: the program ran and exited, and
//     a process that exists is the only thing that can be pre-exec.
//   - A pane process with the image name of the server is the forked child that
//     has not exec'd yet, whatever pane_current_command says.
//   - A transient pane_current_command is not settled either (the "env" wrapper
//     of a single-element argv, and the tmux server name before the process
//     lookup works).
//   - With a program, the image name must be that program.
func execSettled(paneCmd, comm string, alive bool, serverComm, program string, transient map[string]bool) bool {
	if !alive {
		return true
	}
	if serverComm != "" && comm == serverComm {
		return false
	}
	if transient[paneCmd] {
		return false
	}
	if program != "" {
		want := filepath.Base(program)
		// Linux truncates the image name to 15 bytes.
		return comm == want || (len(comm) == 15 && strings.HasPrefix(want, comm))
	}
	return true
}

// serverComm returns the image name of the private tmux server process: the
// name a freshly forked pane process still has before it execs the program. A
// server whose image cannot be determined is an error, not a weaker check.
func (s *Server) serverComm(ctx context.Context) (string, error) {
	if s.serverImage != "" {
		return s.serverImage, nil
	}
	out, err := s.runCtx(ctx, "display-message", "-p", "#{pid}")
	if err != nil {
		return "", fmt.Errorf("display-message #{pid}: %v: %s", err, strings.TrimSpace(out))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return "", fmt.Errorf("server pid %q: %v", strings.TrimSpace(out), err)
	}
	comm, state, err := s.proc.comm(ctx, pid)
	if err != nil {
		return "", fmt.Errorf("inspecting the server process %d: %w", pid, err)
	}
	if state != procPresent || comm == "" {
		return "", fmt.Errorf("the server process %d is gone", pid)
	}
	s.serverImage = comm
	return comm, nil
}

// procState is the outcome of looking a process up, next to the error: a
// lookup that failed says nothing about the process.
type procState int

const (
	procPresent procState = iota // the process exists; comm is its image name
	procGone                     // the process is confirmed not to exist
)

// procInspector looks up the image name of a process. It is an interface so
// tests can inject the process view (a delayed child start) and the failures
// of the lookup.
type procInspector interface {
	comm(ctx context.Context, pid int) (string, procState, error)
}

// osInspector reads /proc (Linux) and falls back to ps (macOS, BSD).
type osInspector struct {
	procRoot string // "/proc"
	ps       string // "ps"
}

func defaultInspector() osInspector { return osInspector{procRoot: "/proc", ps: "ps"} }

// comm returns the image name (base name of the executable, as ps shows it) of
// process pid. It reads only the one pid the private server reported for its
// own pane or for itself.
//
// Absence is confirmed by /proc, when it is mounted (a missing /proc/<pid> next
// to a present /proc/self), or by ps exiting 1 with no output for the pid. Any
// other failure, such as ps missing, not launchable, failing, printing nothing
// with a zero status, or being cut off by ctx, is an error and never means
// that the process is gone.
func (o osInspector) comm(ctx context.Context, pid int) (string, procState, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	b, err := os.ReadFile(filepath.Join(o.procRoot, strconv.Itoa(pid), "comm"))
	if err == nil {
		return strings.TrimSpace(string(b)), procPresent, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := os.Stat(filepath.Join(o.procRoot, "self")); statErr == nil {
			return "", procGone, nil
		}
	}
	// No usable /proc (or an unexpected read error): ask ps.
	out, err := exec.CommandContext(ctx, o.ps, "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	name := strings.TrimSpace(string(out))
	if err == nil {
		if name == "" {
			return "", 0, fmt.Errorf("%s printed nothing for pid %d", o.ps, pid)
		}
		return filepath.Base(name), procPresent, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", 0, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && name == "" {
		return "", procGone, nil
	}
	return "", 0, fmt.Errorf("%s -p %d: %w", o.ps, pid, err)
}

// sessionTarget is the exact-match target of the active pane of a session.
func sessionTarget(name string) string { return "=" + name + ":" }

// directArgv returns the arguments for new-session so that tmux execs argv
// without a shell. tmux runs a single argument through "default-shell -c", so a
// single element is prefixed with env, which makes two arguments.
func directArgv(argv []string) []string {
	if len(argv) == 1 {
		return []string{"env", "--", argv[0]}
	}
	return argv
}

// escapeArg protects a caller-supplied tmux argument from the command parser:
// an argument that ends in ';' is a command separator for tmux, unless the
// semicolon is written as "\;", which tmux turns back into ';'.
func escapeArg(a string) string {
	if strings.HasSuffix(a, ";") {
		return strings.TrimSuffix(a, ";") + `\;`
	}
	return a
}

// SendKeys sends tmux key names to target (as returned by NewSession), for
// example s.SendKeys(target, "echo hi", "Enter"). Arguments that are not key
// names are typed as text, including text that starts with '-' or ends with
// ';'.
func (s *Server) SendKeys(target string, keys ...string) {
	s.t.Helper()
	args := []string{"send-keys", "-t", target, "--"}
	for _, k := range keys {
		args = append(args, escapeArg(k))
	}
	if out, err := s.run(args...); err != nil {
		s.t.Fatalf("tmuxtest: send-keys to %q: %v: %s", target, err, out)
	}
}

// Tmux runs one tmux command on the private socket, for example
// s.Tmux("resize-window", "-t", target, "-x", "80"), and returns its combined
// output. It exists for tests that change the pane under test (resize, clear,
// respawn, options); it can only reach this server, like every other method.
func (s *Server) Tmux(args ...string) (string, error) {
	s.t.Helper()
	return s.run(args...)
}

// Capture returns the visible text of the target pane (a target returned by
// NewSession), with wrapped lines joined.
func (s *Server) Capture(target string) string {
	s.t.Helper()
	out, err := s.capture(target)
	if err != nil {
		s.t.Fatalf("tmuxtest: capture-pane %q: %v", target, err)
		return ""
	}
	return out
}

// WaitFor polls the target pane (a target returned by NewSession) until it
// contains substring and returns the captured text. It fails the test after
// timeout; the deadline also bounds every tmux call and a capture received
// after it does not count.
func (s *Server) WaitFor(target, substring string, timeout time.Duration) string {
	s.t.Helper()
	out, err := s.waitFor(target, substring, timeout)
	if err != nil {
		s.t.Fatalf("tmuxtest: %v", err)
		return ""
	}
	return out
}

func (s *Server) waitFor(target, substring string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var last string
	var lastErr error
	for {
		out, err := s.captureCtx(ctx, target)
		if !time.Now().Before(deadline) {
			// A capture that returns after the deadline does not count.
			if err != nil {
				lastErr = err
			} else {
				last, lastErr = out, nil
			}
			break
		}
		if err == nil && strings.Contains(out, substring) {
			return out, nil
		}
		last, lastErr = out, err
		time.Sleep(pollInterval)
	}
	if lastErr != nil {
		return "", fmt.Errorf("timeout after %s waiting for %q in %q: last capture error: %v", timeout, substring, target, lastErr)
	}
	return "", fmt.Errorf("timeout after %s waiting for %q in %q; last capture:\n%s", timeout, substring, target, last)
}

func (s *Server) capture(target string) (string, error) {
	return s.captureCtx(context.Background(), target)
}

func (s *Server) captureCtx(ctx context.Context, target string) (string, error) {
	out, err := s.runCtx(ctx, "capture-pane", "-p", "-J", "-t", target)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return out, nil
}

// baseArgs is the fixed prefix of every tmux invocation of the harness: the
// private socket and no configuration file.
func (s *Server) baseArgs() []string {
	return []string{"-S", s.socket, "-f", "/dev/null"}
}

// run execs tmux on the private socket. Every tmux exec of the harness goes
// through here, so no command can reach another server.
func (s *Server) run(args ...string) (string, error) {
	return s.runCtx(context.Background(), args...)
}

// runCtx is run bounded by ctx as well as by commandTimeout.
func (s *Server) runCtx(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.bin, append(s.baseArgs(), args...)...)
	cmd.Env = s.env
	cmd.WaitDelay = time.Second
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// shutdown is the t.Cleanup: kill-server for this socket only, then wait until
// the server no longer accepts connections.
func (s *Server) shutdown() {
	// The socket was checked in newServer; a failing kill means no server is
	// running, which is the goal.
	_, _ = s.run("kill-server")
	deadline := time.Now().Add(shutdownWait)
	for time.Now().Before(deadline) {
		if _, err := s.run("list-sessions"); err != nil {
			return
		}
		time.Sleep(pollInterval)
	}
	s.t.Logf("tmuxtest: tmux server on %s still answers %s after kill-server", s.socket, shutdownWait)
}

// defaultSocketPath returns the socket a tmux client without -S and -L uses
// when it is not inside tmux: <TMUX_TMPDIR or /tmp>/tmux-<uid>/default.
func defaultSocketPath(environ []string, uid int) string {
	dir := getenv(environ, "TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, fmt.Sprintf("tmux-%d", uid), "default")
}

// sameSocket reports whether two socket paths name the same file, comparing
// cleaned paths, then the symlink-resolved directory (for example /tmp being
// /private/tmp), then the files themselves when both exist.
func sameSocket(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	if filepath.Base(a) == filepath.Base(b) {
		da, errA := filepath.EvalSymlinks(filepath.Dir(a))
		db, errB := filepath.EvalSymlinks(filepath.Dir(b))
		if errA == nil && errB == nil && da == db {
			return true
		}
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// filterEnv returns environ without TMUX and TMUX_PANE, so the child neither
// believes it is inside tmux nor talks to the server of the surrounding pane.
func filterEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		if key == "TMUX" || key == "TMUX_PANE" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// getenv looks key up in an environ slice; the last entry wins, like os.Getenv.
func getenv(environ []string, key string) string {
	val := ""
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val = v
		}
	}
	return val
}
