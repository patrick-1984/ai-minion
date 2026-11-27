package term

import (
	"context"
	"errors"
)

// ErrSessionExists is what a LayoutKeeper's RebuildSession wraps when the
// name is taken: nothing was created and the existing session is untouched.
var ErrSessionExists = errors.New("term: a session with that name already exists")

// SessionLayout is the shape of one session as a host can save and rebuild
// it: its windows with their pane splits, and each pane's folder. It holds no
// screen content, input, keys or environment values.
type SessionLayout struct {
	Windows []WindowLayout `json:"windows"`
}

// WindowLayout is one window of a [SessionLayout].
type WindowLayout struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	// Layout is the host's own description of the pane splits and sizes
	// (tmux: #{window_layout}); a host applies it again when the pane count
	// matches.
	Layout     string       `json:"layout"`
	Active     bool         `json:"active,omitempty"`
	AutoRename bool         `json:"auto_rename,omitempty"`
	Panes      []PaneLayout `json:"panes"`
}

// PaneLayout is one pane of a [WindowLayout].
type PaneLayout struct {
	Index  int    `json:"index"`
	Cwd    string `json:"cwd,omitempty"`
	Active bool   `json:"active,omitempty"`
	// PID and Command are the pane's process at the save, for the tool
	// detection of the caller; they are never used to rebuild anything.
	PID     int    `json:"-"`
	Command string `json:"-"`
	// StartCommand is the command the pane was started with (tmux:
	// #{pane_start_command}), for the caller's own markers; not saved.
	StartCommand string `json:"-"`
	// Tool and Conversation are filled by the caller: the agent tool running
	// in the pane ("claude", "codex") and its conversation id, "" when
	// unknown.
	Tool         string `json:"tool,omitempty"`
	Conversation string `json:"conversation,omitempty"`
}

// LayoutKeeper is an optional port of a SessionHost that can save and rebuild
// whole sessions (the keep list of session_keep_set). The Worker announces
// proto.FeatureSessionKeepV1 only for a host that implements it.
type LayoutKeeper interface {
	// SessionLayout reads the layout of the named session. It returns
	// ErrNotFound when the session does not exist.
	SessionLayout(ctx context.Context, name string) (SessionLayout, error)
	// RebuildSession creates the named session detached with the windows and
	// panes of l. Each pane starts in its folder (homeDir when the folder is
	// gone) with the program argv(pane, shell) returns, or the user's shell
	// for nil; shell is the user's shell as the host starts it.
	// It never kills, renames or changes an existing session: a name that is
	// taken is an error that wraps ErrSessionExists, and nothing
	// is created.
	RebuildSession(ctx context.Context, name string, l SessionLayout, homeDir string, argv func(p PaneLayout, shell string) []string) error
	// ServerID identifies the running terminal server ("" and an error when
	// none runs): another value after the server restarted.
	ServerID(ctx context.Context) (string, error)
}

// KeepsLayouts reports whether host implements [LayoutKeeper].
func KeepsLayouts(host SessionHost) (LayoutKeeper, bool) {
	k, ok := host.(LayoutKeeper)
	if !ok {
		return nil, false
	}
	if p, ok := host.(interface{ Psmux() bool }); ok && p.Psmux() {
		return nil, false
	}
	return k, true
}
