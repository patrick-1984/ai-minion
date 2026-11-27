package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// paneDirFormat prints the pane's process id and its current path. The path
// is last, so a separator inside it cannot shift the process id; tmux ends the
// line with one newline, which is removed. A path that tmux prints differently
// from the real one (an escaped or a multi-line name) does not open as the
// same folder in the root proof, which then fails closed.
const paneDirFormat = "#{pane_pid}:#{pane_current_path}"

// ReportsPaneDirs implements the capability check of term.PaneDirOf: tmux
// reports the folder of a pane, psmux does not (it reads another process's
// memory for it and falls back to its own folder, BRD R7).
func (h *Host) ReportsPaneDirs() bool { return !h.psmux }

// PaneDir implements term.PaneDirOpener: the process id and the current path
// of the pane, read with display-message for the pane id. The process id must
// be the one of pane (term.ErrPaneChanged otherwise), so that the folder is
// the one of the process that was resolved.
func (h *Host) PaneDir(ctx context.Context, pane term.PaneRef) (term.PaneDir, error) {
	if h.psmux {
		return term.PaneDir{}, term.ErrNoPaneDir
	}
	target, err := h.paneTarget(pane)
	if err != nil {
		return term.PaneDir{}, err
	}
	out, err := h.run(ctx, "display-message", "-p", "-t", target, paneDirFormat)
	if err != nil {
		return term.PaneDir{}, err
	}
	return parsePaneDir(string(out), pane)
}

// parsePaneDir reads the output of paneDirFormat for pane.
func parsePaneDir(out string, pane term.PaneRef) (term.PaneDir, error) {
	out = strings.TrimSuffix(out, "\n")
	pidText, path, ok := strings.Cut(out, ":")
	if !ok || path == "" || !strings.HasPrefix(path, "/") {
		return term.PaneDir{}, fmt.Errorf("pane %s: tmux reported no folder: %w", truncate(pane.ID, 40), term.ErrNoPaneDir)
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return term.PaneDir{}, fmt.Errorf("pane %s: tmux reported no process: %w", truncate(pane.ID, 40), term.ErrNoPaneDir)
	}
	if pane.PID != 0 && pid != pane.PID {
		return term.PaneDir{}, term.ErrPaneChanged
	}
	return term.PaneDir{PID: pid, Path: path}, nil
}
