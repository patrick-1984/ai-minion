package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// ErrSessionExists is returned by CreateSession when a session of that name
// already exists. The Worker never reuses a name, and never touches the
// session that has it.
var ErrSessionExists = errors.New("tmux: a session with that name already exists")

// creator is the session-creation half of term.SessionHost.
type creator interface {
	CreateSession(ctx context.Context, name, cwd string, argv []string) (term.Session, error)
	SetOption(ctx context.Context, session, key, value string) error
}

var (
	_ creator          = (*Host)(nil)
	_ term.SessionHost = (*Host)(nil)
)

// CreateSession creates a detached session named name on the configured
// socket. name is used exactly as given: the caller picks "minion-N" from the
// persisted counter (AD-13); the adapter neither generates nor rewrites names.
//
// cwd is the starting directory; it must be an absolute path of an existing
// directory, or empty for the working directory of the tmux client. A
// non-empty argv runs that program directly in the pane, without a shell, and
// the session ends when the program ends. A one-element argv must not contain
// '=' in its program name (see newSessionArgs). An empty argv starts the
// user's shell: the default-shell of the server, else $SHELL, else /bin/sh. It
// is named explicitly, so a default-command of the server does not run
// instead. The returned Session describes the new session with its pane.
//
// It never kills or renames anything: a name that is taken returns
// ErrSessionExists.
func (h *Host) CreateSession(ctx context.Context, name, cwd string, argv []string) (term.Session, error) {
	if err := (ops.Target{SessionName: name}).Validate(); err != nil {
		return term.Session{}, err
	}
	if err := checkCwd(cwd); err != nil {
		return term.Session{}, err
	}
	for _, a := range argv {
		if strings.ContainsRune(a, 0) {
			return term.Session{}, fmt.Errorf("argv: an argument contains a NUL byte: %w", ops.ErrInvalidArgument)
		}
	}
	if len(argv) > 0 && argv[0] == "" {
		return term.Session{}, fmt.Errorf("argv: the program name is empty: %w", ops.ErrInvalidArgument)
	}
	if len(argv) == 1 && strings.ContainsRune(argv[0], '=') {
		// env would read the program name as a NAME=VALUE assignment and
		// print its environment instead of running anything.
		return term.Session{}, fmt.Errorf("argv: a single program name must not contain '=': %w", ops.ErrInvalidArgument)
	}
	if len(argv) == 0 {
		argv = []string{h.userShell(ctx)}
	}
	transient := transientCommands(argv)
	out, err := h.run(ctx, newSessionArgs(name, cwd, argv)...)
	if err != nil {
		var te *tmuxError
		if errors.As(err, &te) && strings.Contains(te.stderr, "duplicate session") {
			return term.Session{}, fmt.Errorf("session %q: %w", name, ErrSessionExists)
		}
		return term.Session{}, err
	}
	paneID := strings.TrimSpace(string(out))
	if err := checkPaneID(paneID); err != nil {
		return term.Session{}, fmt.Errorf("tmux: new-session returned %q instead of a pane id", truncate(paneID, 40))
	}
	return h.settledSession(ctx, name, paneID, transient)
}

const (
	// settleTimeout bounds the wait for the pane's program to exec. A program
	// that never gets there (a machine that is stuck) is returned as it is
	// after this bound.
	settleTimeout = 3 * time.Second
	settleStep    = 5 * time.Millisecond
	settleMaxStep = 50 * time.Millisecond
)

// transientCommands returns the pane_current_command values that only exist
// between new-session returning and the program exec'ing. tmux forks the pane
// process from the server, so until the exec the pane reports the name of the
// tmux server ("tmux"); a one-element argv runs through the "env" wrapper (see
// newSessionArgs), which is reported until env has exec'd the program. A name
// that the caller asked for is not transient.
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

// settledSession lists the new pane and returns its session once the pane no
// longer reports a transient command, so that the identity of the pane (its
// pid and command, which every write operation re-checks) is the identity of
// the program and not that of the process that is about to exec it. The wait is
// bounded: when the bound passes, the last state is returned as it is, and a
// later change of the pane still fails the identity check of a write
// (fail-closed). A session that is gone, or a cancelled ctx, is an error.
func (h *Host) settledSession(ctx context.Context, name, paneID string, transient map[string]bool) (term.Session, error) {
	deadline := time.Now().Add(settleTimeout)
	step := settleStep
	for {
		sessions, err := h.listPanes(ctx)
		if err != nil {
			return term.Session{}, err
		}
		s, p, ok := findPane(sessions, paneID)
		if !ok {
			return term.Session{}, fmt.Errorf("session %q: created, but its pane %s is already gone: %w", name, paneID, term.ErrNotFound)
		}
		if !transient[p.Ref.Command] || !time.Now().Before(deadline) {
			return s, nil
		}
		select {
		case <-ctx.Done():
			return term.Session{}, ctx.Err()
		case <-time.After(step):
		}
		if step *= 2; step > settleMaxStep {
			step = settleMaxStep
		}
	}
}

func findPane(sessions []term.Session, paneID string) (term.Session, term.Pane, bool) {
	for _, s := range sessions {
		for _, p := range s.Panes {
			if p.Ref.ID == paneID {
				return s, p, true
			}
		}
	}
	return term.Session{}, term.Pane{}, false
}

// userShell returns the shell that an empty argv starts: the default-shell
// option of the running server, else $SHELL, else /bin/sh. Only an absolute
// path without '=' is taken. A server that is not running yet cannot be
// asked, and answers with an error that is not worth reporting.
func (h *Host) userShell(ctx context.Context) string {
	if out, err := h.run(ctx, "show-options", "-gqv", "default-shell"); err == nil {
		if sh := strings.TrimSpace(string(out)); usableShell(sh) {
			return sh
		}
	}
	if sh := os.Getenv("SHELL"); usableShell(sh) {
		return sh
	}
	return "/bin/sh"
}

func usableShell(sh string) bool {
	return filepath.IsAbs(sh) && !strings.ContainsAny(sh, "=\x00\n")
}

// newSessionArgs builds the new-session invocation. -P -F prints the id of the
// first pane, which identifies the new session even when tmux stores a changed
// form of the name.
//
// tmux expands formats in the values of -s and -c, so a literal '#' is written
// as "##" first (a name such as "#{pid}" must stay a name). Arguments of the
// program are not expanded.
//
// tmux executes a command of two or more arguments directly and runs a single
// argument through "default-shell -c". A one-element argv therefore becomes
// "env -- argv0", which makes two arguments and still runs the program
// without a shell. env takes an argv0 with '=' for an assignment even after
// "--", so CreateSession refuses that case before it gets here.
func newSessionArgs(name, cwd string, argv []string) []string {
	args := []string{"new-session", "-d", "-P", "-F", "#{pane_id}", "-s", escapeArg(escapeFormat(name))}
	if cwd != "" {
		args = append(args, "-c", escapeArg(escapeFormat(cwd)))
	}
	if len(argv) == 0 {
		return args
	}
	if len(argv) == 1 {
		argv = []string{"env", "--", argv[0]}
	}
	args = append(args, "--")
	for _, a := range argv {
		args = append(args, escapeArg(a))
	}
	return args
}

// escapeFormat protects a literal value from tmux's format expansion.
func escapeFormat(s string) string { return strings.ReplaceAll(s, "#", "##") }

func checkCwd(cwd string) error {
	if cwd == "" {
		return nil
	}
	if !filepath.IsAbs(cwd) || strings.ContainsRune(cwd, 0) {
		return fmt.Errorf("cwd %q: must be an absolute path: %w", truncate(cwd, 80), ops.ErrInvalidArgument)
	}
	fi, err := os.Stat(cwd)
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("cwd %q: not an existing directory: %w", truncate(cwd, 80), ops.ErrInvalidArgument)
	}
	return nil
}
