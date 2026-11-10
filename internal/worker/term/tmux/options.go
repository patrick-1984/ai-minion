package tmux

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// OptionTask is the session option that holds the task text (set_task). The
// pane list reads it back through "#{@minion_task}".
const OptionTask = "@minion_task"

// OptionProbe is the session option of the CLI's server check: a client writes
// a random nonce there and reads it back through the Worker's session list to
// prove that both talk to the same tmux server. It is separate from
// OptionTask so that the check never touches the user's task text.
const OptionProbe = "@minion_probe"

// MaxOptionValue is the largest option value SetOption accepts, in bytes.
const MaxOptionValue = 4096

// userOptionRE matches the names of tmux user options. SetOption only writes
// user options, so it cannot change tmux's own behaviour.
var userOptionRE = regexp.MustCompile(`^@[A-Za-z0-9_-]{1,64}$`)

// SetOption sets the tmux user option key (for example OptionTask) on a
// session. key must be a user option ("@name"). An empty value clears the
// option. The session is addressed by its exact name; a missing session
// returns term.ErrNotFound. The value is a task-sized string, not a secret: it
// is visible to every tmux client of the socket anyway.
func (h *Host) SetOption(ctx context.Context, session, key, value string) error {
	if err := (ops.Target{SessionName: session}).Validate(); err != nil {
		return err
	}
	if !userOptionRE.MatchString(key) {
		return fmt.Errorf("option %q: only user options of the form @name can be set: %w", truncate(key, 40), ops.ErrInvalidArgument)
	}
	if len(value) > MaxOptionValue || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("option value: must be valid UTF-8 without NUL, at most %d bytes: %w", MaxOptionValue, ops.ErrInvalidArgument)
	}
	_, err := h.run(ctx, "set-option", "-t", exactSession(session), "--", key, escapeArg(value))
	return mapNoSuchSession(err, session)
}

// exactSession is the target of the session named name: "=" requests an exact
// name match (no prefix or pattern), and the trailing colon makes tmux treat
// the target as a session for every command.
func exactSession(name string) string { return "=" + name + ":" }

// mapNoSuchSession maps the message of tmux's set-option for an absent session
// ("no such session") to term.ErrNotFound, like the other commands do.
func mapNoSuchSession(err error, session string) error {
	var te *tmuxError
	if errors.As(err, &te) && strings.Contains(te.stderr, "no such session") {
		return fmt.Errorf("session %q: %w", session, term.ErrNotFound)
	}
	return err
}
