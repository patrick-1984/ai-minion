package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// A pane wider than the screen builder's 4096 columns is the production adapter's
// case of an incomplete read: the snapshot succeeds, but cells beyond column 4096
// are gone, and two panes that differ only there would give the same screen and
// so the same binding. Snapshot returns what it has together with
// term.ErrIncomplete, and a complete pane returns none.
func TestSnapshotOfAWidePaneIsReportedIncomplete(t *testing.T) {
	wide := func(tail string) string {
		return "Bash command\n" + strings.Repeat("x", 4096) + " " + tail + "\nDo you want to proceed?\n1. Yes\n2. No\n"
	}
	f := &fake{reply: map[string]fakeReply{
		"capture-pane":    {stdout: wide("command-A")},
		"display-message": {stdout: "5000\t5\t0\t0\t0\n"},
	}}
	h := newFakeHost(t, f)
	a, err := h.Snapshot(context.Background(), term.PaneRef{ID: "%0"})
	if !errors.Is(err, term.ErrIncomplete) {
		t.Fatalf("a 5000 column pane: err = %v, want term.ErrIncomplete", err)
	}
	if len(a.Lines) == 0 || a.Width != 5000 {
		t.Fatalf("the partial screen is not returned: %+v", a)
	}
	f.reply["capture-pane"] = fakeReply{stdout: wide("command-B")}
	if _, err := h.Snapshot(context.Background(), term.PaneRef{ID: "%0"}); !errors.Is(err, term.ErrIncomplete) {
		t.Fatalf("the second read: err = %v", err)
	}

	// A pane of ordinary width is complete.
	f.reply["capture-pane"] = fakeReply{stdout: "Bash command\n  echo ok\nDo you want to proceed?\n1. Yes\n2. No\n"}
	f.reply["display-message"] = fakeReply{stdout: "80\t5\t0\t0\t0\n"}
	if scr, err := h.Snapshot(context.Background(), term.PaneRef{ID: "%0"}); err != nil || len(scr.Lines) != 5 {
		t.Fatalf("an ordinary pane: %v, %d rows", err, len(scr.Lines))
	}
}

// A capture whose content was cut by the builder's bounds is an error that says
// so, never a silently shortened one.
func TestCaptureOfAWidePaneIsAnIncompleteError(t *testing.T) {
	f := &fake{reply: map[string]fakeReply{
		"capture-pane":    {stdout: "ok\n" + strings.Repeat("y", 5000) + "\n"},
		"display-message": {stdout: "5000\t5\t0\t0\t0\n"},
	}}
	h := newFakeHost(t, f)
	_, err := h.Capture(context.Background(), term.PaneRef{ID: "%0"}, 10)
	if !errors.Is(err, ErrCaptureTooLarge) || !errors.Is(err, term.ErrIncomplete) {
		t.Fatalf("err = %v, want ErrCaptureTooLarge and term.ErrIncomplete", err)
	}
}
