//go:build unix && tmux

package tmux

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/testutil/tmuxtest"
)

// The positive control on a real private tmux server: a real pane that shows a
// 30-row dialog is read completely (no error), the full visible screen comes
// back row for row, and the screen is the same through Capture. Nothing is run
// in the pane but cat.
func TestRealPaneSnapshotIsCompleteAndMatchesCapture(t *testing.T) {
	srv := tmuxtest.New(t)
	target := srv.NewSession("complete", "cat")
	h, err := New(srv.Socket())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := h.ResolvePane(ctx, ops.Target{SessionName: "complete"})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"Bash command", "  echo real-pane-marker", "Do you want to proceed?", "1. Yes", "2. No"} {
		if err := h.PasteText(ctx, ref, l, ref); err != nil {
			t.Fatal(err)
		}
		if _, err := h.SendKeys(ctx, ref, []string{"Enter"}, ref); err != nil {
			t.Fatal(err)
		}
	}
	srv.WaitFor(target, "2. No", 10*time.Second)
	time.Sleep(100 * time.Millisecond)
	scr, err := h.Snapshot(ctx, ref)
	if err != nil {
		t.Fatalf("a real pane of ordinary width is complete, got %v", err)
	}
	if got := strings.Join(scr.Lines, "\n"); !strings.Contains(got, "real-pane-marker") || scr.Height != len(scr.Lines) {
		t.Fatalf("snapshot (%d rows of %d): %q", len(scr.Lines), scr.Height, got)
	}
	capt, err := h.Capture(ctx, ref, 100)
	if err != nil {
		t.Fatal(err)
	}
	// The capture ends with the visible screen's non-blank rows.
	want := append([]string(nil), scr.Lines...)
	for len(want) > 0 && strings.TrimSpace(want[len(want)-1]) == "" {
		want = want[:len(want)-1]
	}
	if len(capt.Lines) < len(want) {
		t.Fatalf("capture has %d rows, the screen %d", len(capt.Lines), len(want))
	}
	tail := capt.Lines[len(capt.Lines)-len(want):]
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("row %d: capture %q, snapshot %q", i, tail[i], want[i])
		}
	}
}
