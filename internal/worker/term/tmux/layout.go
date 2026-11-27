package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

var _ term.LayoutKeeper = (*Host)(nil)

// layoutFormatFields is one pane row of SessionLayout, framed like the rows of
// ListSessions (see paneFormatFields): a window name, a folder or a start
// command can hold any byte.
const layoutFormatFields = "#{window_index}%[1]s#{window_name}%[1]s#{window_layout}%[1]s#{window_active}%[1]s" +
	"#{automatic-rename}%[1]s#{pane_index}%[1]s#{pane_active}%[1]s#{pane_pid}%[1]s#{pane_current_command}%[1]s" +
	"#{pane_current_path}%[1]s#{pane_start_command}%[2]s"

const layoutFields = 11

// buildWidth and buildHeight are the size a rebuilt window is made at: a new
// detached window is 80x24, where the fifth split finds no room. The final
// select-layout of the saved layout sets the saved splits and sizes, and the
// first client that attaches resizes the window as always.
const (
	buildWidth  = 200
	buildHeight = 100
)

// idFormat prints the window and pane id of a new window.
const idFormat = "#{window_id} #{pane_id}"

// maxLayoutPanes bounds the panes of one rebuilt session.
const maxLayoutPanes = 64

// SessionLayout implements [term.LayoutKeeper].
func (h *Host) SessionLayout(ctx context.Context, name string) (term.SessionLayout, error) {
	if err := (ops.Target{SessionName: name}).Validate(); err != nil {
		return term.SessionLayout{}, err
	}
	c, err := newPaneCodec()
	if err != nil {
		return term.SessionLayout{}, err
	}
	out, err := h.run(ctx, "list-panes", "-s", "-t", exactSession(name), "-F", fmt.Sprintf(layoutFormatFields, c.fs, c.rs))
	if err != nil {
		var te *tmuxError
		if errors.As(err, &te) && (isNotFound(te.stderr) || strings.Contains(te.stderr, "can't find session")) || h.serverGone(err) {
			return term.SessionLayout{}, fmt.Errorf("session %q: %w", name, term.ErrNotFound)
		}
		return term.SessionLayout{}, mapNoSuchSession(err, name)
	}
	return c.parseLayout(string(out))
}

// parseLayout parses the rows of SessionLayout, in tmux's order (windows by
// index, panes by index).
func (c paneCodec) parseLayout(out string) (term.SessionLayout, error) {
	rows := strings.Split(out, c.rs+"\n")
	if last := rows[len(rows)-1]; last != "" {
		return term.SessionLayout{}, errors.New("tmux: list-panes output ends in an unterminated row")
	}
	var l term.SessionLayout
	for n, row := range rows[:len(rows)-1] {
		f := strings.Split(row, c.fs)
		if len(f) != layoutFields {
			return term.SessionLayout{}, fmt.Errorf("tmux: list-panes row %d has %d fields, want %d", n+1, len(f), layoutFields)
		}
		win, e1 := strconv.Atoi(f[0])
		idx, e2 := strconv.Atoi(f[5])
		pid, e3 := parsePID(f[7])
		if err := errors.Join(e1, e2, e3); err != nil {
			return term.SessionLayout{}, fmt.Errorf("tmux: list-panes row %d: %w", n+1, err)
		}
		if len(l.Windows) == 0 || l.Windows[len(l.Windows)-1].Index != win {
			l.Windows = append(l.Windows, term.WindowLayout{
				Index: win, Name: f[1], Layout: f[2], Active: f[3] == "1", AutoRename: f[4] == "1",
			})
		}
		w := &l.Windows[len(l.Windows)-1]
		w.Panes = append(w.Panes, term.PaneLayout{
			Index: idx, Active: f[6] == "1", PID: pid, Command: f[8], Cwd: f[9], StartCommand: f[10],
		})
	}
	if len(l.Windows) == 0 {
		return term.SessionLayout{}, term.ErrNotFound
	}
	return l, nil
}

// ServerID implements [term.LayoutKeeper]: an identity of the running server
// that changes when the server is replaced. It is the identity of the server's
// socket file (device, inode and change time): every new server creates a new
// socket. This needs no session, so it works on every supported tmux version
// (tmux 3.0a answers "no current target" to display-message without a
// session). A server that is not running gives an error.
func (h *Host) ServerID(ctx context.Context) (string, error) {
	if _, err := h.run(ctx, "show-options", "-g", "-v", "exit-empty"); err != nil {
		return "", err
	}
	id, err := socketIdentity(h.socket)
	if err != nil {
		return "", fmt.Errorf("tmux: server identity: %w", err)
	}
	return id, nil
}

// RebuildSession implements [term.LayoutKeeper]. The session is created with
// its first pane, then each window and split in the saved order; the saved
// #{window_layout} is applied to every window at the end (it takes effect
// when the pane count matches, which it does unless a pane failed to start).
// A missing folder starts in homeDir. Nothing is killed or renamed: a name
// that is taken fails at new-session with ErrSessionExists.
func (h *Host) RebuildSession(ctx context.Context, name string, l term.SessionLayout, homeDir string, argv func(p term.PaneLayout, shell string) []string) error {
	if h.psmux {
		return fmt.Errorf("tmux: psmux cannot rebuild a session layout: %w", ops.ErrInvalidArgument)
	}
	if err := (ops.Target{SessionName: name}).Validate(); err != nil {
		return err
	}
	panes := 0
	for _, w := range l.Windows {
		panes += len(w.Panes)
	}
	if panes == 0 {
		return fmt.Errorf("session %q: the saved layout has no pane: %w", name, ops.ErrInvalidArgument)
	}
	if panes > maxLayoutPanes {
		return fmt.Errorf("session %q: the saved layout has %d panes, more than %d: %w", name, panes, maxLayoutPanes, ops.ErrInvalidArgument)
	}
	shell := h.userShell(ctx)
	program := func(p term.PaneLayout) []string {
		var a []string
		if argv != nil {
			a = argv(p, shell)
		}
		if len(a) == 0 {
			a = []string{shell}
		}
		if len(a) == 1 {
			a = []string{"env", "--", a[0]} // see newSessionArgs
		}
		out := []string{"--"}
		for _, s := range a {
			out = append(out, escapeArg(s))
		}
		return out
	}
	folder := func(p term.PaneLayout) string {
		if p.Cwd != "" && filepath.IsAbs(p.Cwd) && !strings.ContainsRune(p.Cwd, 0) {
			if fi, err := os.Stat(p.Cwd); err == nil && fi.IsDir() {
				return escapeFormat(p.Cwd)
			}
		}
		if homeDir != "" {
			return escapeFormat(homeDir)
		}
		return ""
	}
	cwdArgs := func(p term.PaneLayout) []string {
		if c := folder(p); c != "" {
			return []string{"-c", escapeArg(c)}
		}
		return nil
	}
	size := []string{"-x", strconv.Itoa(buildWidth), "-y", strconv.Itoa(buildHeight)}
	built, activeWin := 0, ""
	for wi, w := range l.Windows {
		if len(w.Panes) == 0 {
			continue
		}
		wname := escapeArg(escapeFormat(w.Name))
		first := w.Panes[0]
		var ids string
		if built == 0 {
			args := append([]string{"new-session", "-d", "-P", "-F", idFormat, "-s", escapeArg(escapeFormat(name))}, size...)
			if w.Name != "" {
				args = append(args, "-n", wname)
			}
			args = append(args, cwdArgs(first)...)
			out, err := h.run(ctx, append(args, program(first)...)...)
			h.tightenSocket()
			if err != nil {
				if h.managed && h.serverGone(err) {
					return fmt.Errorf("tmux: the managed server is not running: %w", term.ErrHostDown)
				}
				var te *tmuxError
				if errors.As(err, &te) && strings.Contains(te.stderr, "duplicate session") {
					return fmt.Errorf("session %q: %w", name, ErrSessionExists)
				}
				return err
			}
			ids = string(out)
		} else {
			args := []string{"new-window", "-d", "-P", "-F", idFormat, "-t", exactSession(name)}
			if w.Name != "" {
				args = append(args, "-n", wname)
			}
			args = append(args, cwdArgs(first)...)
			out, err := h.run(ctx, append(args, program(first)...)...)
			if err != nil {
				return fmt.Errorf("session %q: window %d: %w", name, wi, err)
			}
			ids = string(out)
		}
		winID, lastPane, ok := strings.Cut(strings.TrimSpace(ids), " ")
		if !ok || !strings.HasPrefix(winID, "@") || checkPaneID(lastPane) != nil {
			return fmt.Errorf("session %q: tmux returned %q instead of a window and pane id", name, truncate(ids, 40))
		}
		if built > 0 {
			_, _ = h.run(ctx, append([]string{"resize-window", "-t", winID}, size...)...)
			_, _ = h.run(ctx, "set-option", "-wu", "-t", winID, "window-size") // resize-window made it manual
		}
		built++
		for _, p := range w.Panes[1:] {
			// One tiled layout before every split, so that the next split
			// finds room; the split goes after the last pane (saved order).
			_, _ = h.run(ctx, "select-layout", "-t", winID, "tiled")
			args := []string{"split-window", "-d", "-P", "-F", "#{pane_id}", "-t", lastPane}
			args = append(args, cwdArgs(p)...)
			out, err := h.run(ctx, append(args, program(p)...)...)
			if err != nil {
				return fmt.Errorf("session %q: window %d: split: %w", name, wi, err)
			}
			if id := strings.TrimSpace(string(out)); checkPaneID(id) == nil {
				lastPane = id
			}
		}
		if w.AutoRename {
			_, _ = h.run(ctx, "set-option", "-w", "-t", winID, "automatic-rename", "on") // -n turned it off
		}
		if w.Layout != "" && !strings.ContainsAny(w.Layout, "\x00\n") {
			_, _ = h.run(ctx, "select-layout", "-t", winID, escapeArg(w.Layout))
		}
		for i, p := range w.Panes {
			if p.Active {
				_, _ = h.run(ctx, "select-pane", "-t", winID+"."+strconv.Itoa(i+h.paneBase(ctx)))
				break
			}
		}
		if w.Active && activeWin == "" {
			activeWin = winID
		}
	}
	if activeWin != "" {
		_, _ = h.run(ctx, "select-window", "-t", activeWin)
	}
	return nil
}

// paneBase is the server's pane-base-index (0 when it cannot be read).
func (h *Host) paneBase(ctx context.Context) int {
	out, err := h.run(ctx, "show-options", "-gv", "pane-base-index")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
