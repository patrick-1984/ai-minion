package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// MaxPasteBytes is the largest text PasteText accepts. It bounds the memory of
// one paste; ops limits send_text far below it.
const MaxPasteBytes = term.MaxPasteBytes

// cleanupTimeout bounds the best-effort delete-buffer that runs after a
// failed or cancelled paste, on a context that outlives the caller's.
const cleanupTimeout = 3 * time.Second

// writer is the write half of term.SessionHost that this file implements.
type writer interface {
	PasteText(ctx context.Context, pane term.PaneRef, text string, expect term.PaneRef) error
	SendKeys(ctx context.Context, pane term.PaneRef, keys []string, expect term.PaneRef) (sent int, err error)
}

var _ writer = (*Host)(nil)

// paneStateFormat reads what the KEEP re-check compares and the flags that
// decide whether tmux delivers input at all. The pane id, the pid and the
// four 0/1 flags come first and are single tokens, so the command (the rest
// of the line, which may contain any byte but is never re-expanded by tmux)
// needs no framing.
const paneStateFormat = "#{pane_id} #{pane_pid} #{pane_dead} #{pane_input_off} #{pane_in_mode} #{pane_synchronized} #{pane_current_command}"

// paneState is the live state of one pane.
type paneState struct {
	ref      term.PaneRef
	dead     bool // the program ended and the pane is kept (remain-on-exit)
	inputOff bool // input is disabled (select-pane -d): tmux drops it
	inMode   bool // copy mode or another mode: keys go to the mode, not the program
	synced   bool // synchronize-panes is on: send-keys is broadcast to the sibling panes
}

// currentState reads the live state of a pane. It returns term.ErrNotFound
// when the pane no longer exists.
func (h *Host) currentState(ctx context.Context, id string) (paneState, error) {
	out, err := h.run(ctx, "display-message", "-p", "-t", id, paneStateFormat)
	if err != nil {
		return paneState{}, err
	}
	return parsePaneState(string(out))
}

func parsePaneState(out string) (paneState, error) {
	out = strings.TrimSuffix(out, "\n")
	f := strings.SplitN(out, " ", 7)
	if len(f) != 7 {
		return paneState{}, fmt.Errorf("tmux: display-message returned %d fields, want 7", len(f))
	}
	if f[0] == "" {
		// tmux answers a format for a pane that does not exist with empty
		// fields and a success status.
		return paneState{}, fmt.Errorf("tmux: the pane does not exist: %w", term.ErrNotFound)
	}
	var flags [4]bool
	for i, v := range f[2:6] {
		switch v {
		case "0":
		case "1":
			flags[i] = true
		default:
			return paneState{}, fmt.Errorf("tmux: display-message flag %q is not 0 or 1", truncate(v, 20))
		}
	}
	// tmux 3.8 prints an empty pane_pid for a dead pane (its process is
	// gone); earlier versions print the pid of the ended process. An empty
	// pid is only valid for a dead pane and reads as pid 0.
	if f[1] == "" && !flags[0] {
		return paneState{}, errors.New("tmux: display-message pane_pid is empty for a pane that is not dead")
	}
	pid, err := parsePID(f[1])
	if err != nil {
		return paneState{}, fmt.Errorf("tmux: display-message pane_pid: %w", err)
	}
	return paneState{
		ref:  term.PaneRef{ID: f[0], PID: pid, Command: f[6]},
		dead: flags[0], inputOff: flags[1], inMode: flags[2], synced: flags[3],
	}, nil
}

// keepCheck is the KEEP re-check of AD-13 and AD-15: it compares the live pane
// with expect and returns term.ErrPaneChanged on any difference (id, pane_pid
// or current command). A pane that vanished is a changed pane as well: the
// write must not go anywhere, and the caller reports the KEEP outcome.
//
// The identity alone is not enough, because tmux accepts input for some panes
// and then drops it. The check also refuses, with term.ErrPaneChanged and a
// message that names the state, a pane whose program has ended (pane_dead),
// whose input is disabled (pane_input_off) and, when keys is true, a pane in
// copy mode or another mode (pane_in_mode), where send-keys feeds the mode and
// not the program, and a pane of a window with synchronize-panes on
// (pane_synchronized), because tmux then broadcasts every key to the sibling
// panes even with an explicit -t, and Enter or C-c would reach programs that
// were never checked. The setting is the user's and is left as it is; the
// keys are refused. Pasting into a pane in a mode reaches the program, and
// paste-buffer is not broadcast, so PasteText refuses neither case.
func (h *Host) keepCheck(ctx context.Context, pane, expect term.PaneRef, keys bool) error {
	if expect.ID != pane.ID {
		return fmt.Errorf("expected pane %q, writing to %q: %w", expect.ID, pane.ID, term.ErrPaneChanged)
	}
	st, err := h.currentState(ctx, pane.ID)
	if errors.Is(err, term.ErrNotFound) {
		return fmt.Errorf("pane %s is gone: %w", pane.ID, term.ErrPaneChanged)
	}
	if err != nil {
		return err
	}
	if st.dead {
		// Checked before the identity: the pid of a dead pane is empty (3.8)
		// or stale, and the state is the more useful message.
		return fmt.Errorf("pane %s: its program has ended, nothing receives input: %w", pane.ID, term.ErrPaneChanged)
	}
	actual := st.ref
	if err := term.CheckExpect(actual, expect); err != nil {
		return fmt.Errorf("pane %s now runs %q (pid %d), expected %q (pid %d): %w",
			pane.ID, truncate(actual.Command, 40), actual.PID, truncate(expect.Command, 40), expect.PID, err)
	}
	switch {
	case st.inputOff:
		return fmt.Errorf("pane %s: input is disabled, tmux would drop it: %w", pane.ID, term.ErrPaneChanged)
	case keys && st.inMode:
		return fmt.Errorf("pane %s is in copy mode or another mode, keys would not reach the program: %w", pane.ID, term.ErrPaneChanged)
	case keys && st.synced:
		return fmt.Errorf("pane %s: synchronize-panes is on, tmux would send the keys to the sibling panes as well: %w", pane.ID, term.ErrPaneChanged)
	}
	return nil
}

// newBufferName returns a fresh tmux buffer name. Every paste uses its own, so
// concurrent pastes to different panes cannot paste each other's text.
func newBufferName() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("tmux: no randomness for the paste buffer name: %w", err)
	}
	return "minion-" + hex.EncodeToString(b[:]), nil
}

// PasteText pastes text into the pane as typed input, verbatim.
//
// It is the same guarded paste as PasteMarked (pasteGuarded) without the
// delivery mark: the text goes through a uniquely named tmux buffer ("load-buffer
// -b <name> -" reads it from the standard input of tmux, so the content never
// appears in an argument list), and one tmux invocation checks that the pane
// still is expect (pane id, pane_pid, current command, the program running,
// input enabled) and only then runs "paste-buffer -d -p -r -b <name> -t %id".
// On a mismatch it returns term.ErrPaneChanged and sends nothing; the buffer
// is deleted whenever the paste does not happen.
//
// The paste is bracketed (-p) when the receiving program enabled bracketed
// paste, and verbatim (-r): an LF stays LF and is not rewritten to the CR of
// the Enter key. A program that does not enable bracketed paste receives the
// bytes unwrapped, and it may treat an LF as a submission; the adapter adds
// no Enter and cannot know. The text must be valid UTF-8, non-empty and at most
// MaxPasteBytes, and may hold no control character other than TAB and LF
// (checkPasteText): CR submits, ESC can end a bracketed paste, and tmux 3.8
// rewrites the other controls. A text that does not qualify returns an error
// matching ops.ErrInvalidArgument and sends nothing. Intentional control input
// is SendKeys.
//
// A failure after the paste invocation started is reported as a plain error,
// not as "not sent": the caller maps it to the unknown outcome (AD-15).
func (h *Host) PasteText(ctx context.Context, pane term.PaneRef, text string, expect term.PaneRef) error {
	_, err := h.pasteGuarded(ctx, pane, text, expect, false)
	return err
}

var _ term.VerbatimPaster = (*Host)(nil)

// VerbatimPaste reports that PasteText is the verbatim paste (term.VerbatimPaster).
func (h *Host) VerbatimPaste() bool { return true }

// deleteBuffer removes a paste buffer that was loaded but not pasted. It is
// best effort and runs on its own short context, because the caller's context
// may be the reason the paste stopped. After a successful paste-buffer -d the
// buffer is gone and the command fails harmlessly.
func (h *Host) deleteBuffer(name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), cleanupTimeout)
	defer cancel()
	_, _ = h.run(ctx, "delete-buffer", "-b", name)
}

// SendKeys sends named keys (tmux key names such as Enter or C-c, or one
// printable character) one at a time, in order. Before every key it re-checks
// pane_pid and the current command against expect, and that the pane still
// takes keys (not dead, input enabled, not in a mode, not synchronized; see keepCheck). When the
// pane changed or stopped taking keys, the call stops: sent is the number of
// keys that went out before that, and the error is term.ErrPaneChanged. The executor reports partial when sent > 0 and
// failed_not_sent only when sent == 0 (AD-15).
//
// Any other failure also returns the count of keys sent so far. The key that
// was running when the failure happened is not counted, although a timeout may
// have let it through; the caller reports unknown for such transport errors.
//
// The keys must be key names. Free text belongs in PasteText, and keys that
// are not names (empty, longer than ops.MaxKeyLen, control characters) are
// refused before anything is sent.
func (h *Host) SendKeys(ctx context.Context, pane term.PaneRef, keys []string, expect term.PaneRef) (int, error) {
	if err := checkPaneID(pane.ID); err != nil {
		return 0, err
	}
	if len(keys) == 0 {
		return 0, fmt.Errorf("keys: must not be empty: %w", ops.ErrInvalidArgument)
	}
	for _, k := range keys {
		if err := checkKey(k); err != nil {
			return 0, err
		}
	}
	sent := 0
	for _, k := range keys {
		if err := h.keepCheck(ctx, pane, expect, true); err != nil {
			return sent, err
		}
		if _, err := h.run(ctx, "send-keys", "-t", pane.ID, "--", escapeArg(k)); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

func checkKey(k string) error {
	if k == "" || len(k) > ops.MaxKeyLen || !utf8.ValidString(k) {
		return fmt.Errorf("key %q: must be a tmux key name of at most %d bytes: %w", truncate(k, 40), ops.MaxKeyLen, ops.ErrInvalidArgument)
	}
	for _, r := range k {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return fmt.Errorf("key %q: control characters are not key names: %w", truncate(k, 40), ops.ErrInvalidArgument)
		}
	}
	return nil
}

// escapeArg protects an argument from tmux's own command parser: an argument
// that ends in ';' is a command separator, unless the semicolon is written as
// "\;", which tmux turns back into ';'.
func escapeArg(a string) string {
	if strings.HasSuffix(a, ";") {
		return strings.TrimSuffix(a, ";") + `\;`
	}
	return a
}
