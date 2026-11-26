//go:build unix && tmux

package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// RebuildSession of a name that is taken wraps term.ErrSessionExists (the
// error the keeper skips) and leaves the session alone.
func TestRebuildSessionOfATakenNameIsTermErrSessionExists(t *testing.T) {
	srv := tmuxtest.New(t)
	srv.NewSession("taken", "cat")
	h, err := New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	l := term.SessionLayout{Windows: []term.WindowLayout{{Name: "w", Panes: []term.PaneLayout{{}, {Index: 1}}}}}
	err = h.RebuildSession(context.Background(), "taken", l, t.TempDir(), func(term.PaneLayout, string) []string { return nil })
	if !errors.Is(err, term.ErrSessionExists) || !errors.Is(err, ErrSessionExists) {
		t.Fatalf("err = %v, want term.ErrSessionExists", err)
	}
	if out, err := srv.Tmux("list-panes", "-t", "=taken:", "-F", "#{pane_id}"); err != nil || strings.Count(strings.TrimSpace(out), "\n") != 0 {
		t.Fatalf("the taken session changed: %q, %v", out, err)
	}
}
