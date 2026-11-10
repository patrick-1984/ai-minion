package term

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Errors of the SessionHost port. Adapters return them wrapped with %w, and
// callers test with errors.Is.
var (
	// ErrPaneChanged is the KEEP-class error: the pane's process id or current
	// command no longer matches the expected PaneRef. The write stops. For
	// SendKeys the count of keys already sent is returned alongside, so that
	// the executor reports partial instead of failed_not_sent (AD-15).
	ErrPaneChanged = errors.New("term: pane changed")
	// ErrNotFound means the session, window or pane does not exist.
	ErrNotFound = errors.New("term: not found")
)

// PaneRef identifies one pane at one moment. ID is the stable pane id (a tmux
// "%3", or the ConPTY pane id); PID and Command are what the KEEP re-check
// compares before every write (AD-13), because the id alone survives a program
// being replaced inside the pane.
type PaneRef struct {
	ID      string
	PID     int
	Command string
}

// Same reports whether two refs denote the same pane running the same process
// and command.
func (p PaneRef) Same(o PaneRef) bool {
	return p.ID == o.ID && p.PID == o.PID && p.Command == o.Command
}

// CheckExpect is the KEEP re-check every adapter runs before a write: it
// returns ErrPaneChanged when the actual pane differs from the expected one.
func CheckExpect(actual, expect PaneRef) error {
	if !actual.Same(expect) {
		return ErrPaneChanged
	}
	return nil
}

// Pane describes one pane of a session.
type Pane struct {
	Ref    PaneRef
	Window int  // window index
	Index  int  // pane index within the window
	Active bool // the active pane of the active window
}

// Session describes one session as the host reports it. Kind and state of the
// panes come from agentdetect and are added by the executor, not by the host.
type Session struct {
	Name     string
	Created  time.Time
	Windows  int
	Attached bool
	Task     string // value of the @minion_task option, empty when unset
	// Probe is the value of the @minion_probe option, empty when unset. It
	// is a dedicated identity marker for the server check of the CLI (see
	// ops.SessionInfo.Probe) and never holds user text.
	Probe string
	Panes []Pane
}

// ActivePane returns the active pane of the session.
func (s Session) ActivePane() (Pane, bool) {
	for _, p := range s.Panes {
		if p.Active {
			return p, true
		}
	}
	return Pane{}, false
}

// SessionHost is the port between the Worker and a terminal multiplexer: tmux
// on Unix, ConPTY on Windows (AD-13). The executor, the Hub path and the local
// socket path are built against this interface only.
//
// Every method addresses exactly one pane id resolved by ResolvePane. Every
// write takes the expected PaneRef and re-checks it first (KEEP); on a
// mismatch it sends nothing more and returns ErrPaneChanged. Text never goes
// on a command line. Output returned as ops.Screen is already sanitised
// (AD-14).
type SessionHost interface {
	// ListSessions returns every session the host sees, adopted ones
	// included, with their panes.
	ListSessions(ctx context.Context) ([]Session, error)

	// ResolvePane resolves a target to one pane: the active pane of the
	// session, or the explicit window.pane. It returns ErrNotFound when the
	// session or pane does not exist. The target is bound to the session
	// name, so a recreated session with the same name resolves again (R88).
	ResolvePane(ctx context.Context, target ops.Target) (PaneRef, error)

	// Snapshot returns the visible screen of the pane.
	Snapshot(ctx context.Context, pane PaneRef) (ops.Screen, error)

	// Capture returns the last lines lines of the pane including scrollback.
	Capture(ctx context.Context, pane PaneRef, lines int) (ops.Screen, error)

	// PasteText pastes text into the pane as typed input. It first compares
	// the pane with expect and returns ErrPaneChanged without sending
	// anything on a mismatch.
	PasteText(ctx context.Context, pane PaneRef, text string, expect PaneRef) error

	// SendKeys sends named keys in order. It re-checks expect before every
	// key and returns the number of keys sent, together with ErrPaneChanged
	// when the pane changed after that many keys, so that the caller can
	// report partial rather than failed_not_sent.
	SendKeys(ctx context.Context, pane PaneRef, keys []string, expect PaneRef) (sent int, err error)

	// CreateSession creates a detached session. A non-empty argv runs that
	// program directly in the pane without a shell, otherwise the user's
	// shell starts. The host never renames or kills a session.
	CreateSession(ctx context.Context, name, cwd string, argv []string) (Session, error)

	// SetOption sets a session option such as "@minion_task". It returns
	// ErrNotFound when the session does not exist.
	SetOption(ctx context.Context, session, key, value string) error
}

// VerbatimPaster is an optional port of a SessionHost. A host that implements
// it and returns true guarantees what PasteText documents for the verbatim
// paste: the text reaches the program byte for byte (an LF stays an LF, TAB
// stays TAB), the host adds no Enter, it wraps the text in bracketed-paste
// markers only when the program enabled them, and it refuses with
// ops.ErrInvalidArgument, sending nothing, every control character that a
// paste would turn into input (CR, ESC, NUL, DEL, C1). The Worker announces the
// feature proto.FeatureTextVerbatim only for such a host, so the Hub does not
// route a multi-line text to a Worker whose host would rewrite it.
type VerbatimPaster interface {
	VerbatimPaste() bool
}

// IsVerbatim reports whether host guarantees the verbatim paste.
func IsVerbatim(host SessionHost) bool {
	v, ok := host.(VerbatimPaster)
	return ok && v.VerbatimPaste()
}

// MaxPasteBytes is the largest text a host's PasteText accepts. It bounds the
// memory of one paste; ops limits send_text far below it.
const MaxPasteBytes = 1 << 20

// CheckPasteText is the validation every adapter's PasteText runs before it
// sends a byte, so the rule of what a paste may carry is one rule on every
// platform: non-empty valid UTF-8 of at most MaxPasteBytes without any control
// character except TAB and LF. A CR would submit, an ESC could end a bracketed
// paste early or start a control sequence, and NUL, DEL and the C1 range are
// input a paste must never deliver; intentional control input is SendKeys with
// a key name. The error matches ops.ErrInvalidArgument, and nothing was sent.
func CheckPasteText(text string) error {
	if len(text) > MaxPasteBytes {
		return fmt.Errorf("text: %d bytes exceed the limit of %d: %w", len(text), MaxPasteBytes, ops.ErrInvalidArgument)
	}
	return ops.CheckPasteText(text)
}
