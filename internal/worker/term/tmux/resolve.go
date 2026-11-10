package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// ResolvePane resolves a target to one pane: the active pane of the active
// window of the session, or the explicit window.pane of target.Pane. The
// session is matched by its exact name (no prefix or pattern matching), so a
// session that was killed and created again under the same name resolves to
// the new one (R88). It returns term.ErrNotFound when the session, window or
// pane does not exist; every later command addresses the returned pane id.
func (h *Host) ResolvePane(ctx context.Context, target ops.Target) (term.PaneRef, error) {
	if err := target.Validate(); err != nil {
		return term.PaneRef{}, err
	}
	sessions, err := h.listPanes(ctx)
	if err != nil {
		return term.PaneRef{}, err
	}
	return pickPane(sessions, target.SessionName, target.Pane)
}

// pickPane selects the pane of a session by exact name. pane is "" for the
// active pane or "window.pane".
func pickPane(sessions []term.Session, name, pane string) (term.PaneRef, error) {
	var s *term.Session
	for i := range sessions {
		if sessions[i].Name == name {
			s = &sessions[i]
			break
		}
	}
	if s == nil {
		return term.PaneRef{}, fmt.Errorf("session %q: %w", name, term.ErrNotFound)
	}
	if pane == "" {
		p, ok := s.ActivePane()
		if !ok {
			return term.PaneRef{}, fmt.Errorf("session %q has no active pane: %w", name, term.ErrNotFound)
		}
		return p.Ref, nil
	}
	win, idx, err := splitPane(pane)
	if err != nil {
		return term.PaneRef{}, err
	}
	for _, p := range s.Panes {
		if p.Window == win && p.Index == idx {
			return p.Ref, nil
		}
	}
	return term.PaneRef{}, fmt.Errorf("session %q has no pane %d.%d: %w", name, win, idx, term.ErrNotFound)
}

func splitPane(pane string) (win, idx int, err error) {
	w, p, ok := strings.Cut(pane, ".")
	if ok {
		win, err = strconv.Atoi(w)
		if err == nil {
			idx, err = strconv.Atoi(p)
		}
	}
	if !ok || err != nil {
		return 0, 0, fmt.Errorf("pane %q: must have the form window.pane: %w", pane, ops.ErrInvalidArgument)
	}
	return win, idx, nil
}

// TargetFromSpec parses the tmux-style spec "session" or "session:window.pane"
// into an ops.Target (without a worker id). The session name is everything
// before the first colon; session names never contain ':' (tmux and ops both
// refuse it). An invalid spec returns an error that matches
// ops.ErrInvalidArgument.
func TargetFromSpec(spec string) (ops.Target, error) {
	name, pane, _ := strings.Cut(spec, ":")
	t := ops.Target{SessionName: name, Pane: pane}
	if err := t.Validate(); err != nil {
		return ops.Target{}, err
	}
	return t, nil
}
