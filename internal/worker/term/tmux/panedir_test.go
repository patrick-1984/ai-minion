package tmux

import (
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

func TestParsePaneDir(t *testing.T) {
	pane := term.PaneRef{ID: "%3", PID: 42}
	got, err := parsePaneDir("42:/home/u/pro:ject\n", pane)
	if err != nil || got.PID != 42 || got.Path != "/home/u/pro:ject" {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := parsePaneDir("43:/x\n", pane); !errors.Is(err, term.ErrPaneChanged) {
		t.Fatalf("another process: %v", err)
	}
	for _, out := range []string{"", "42:\n", "42:relative\n", "x:/a\n", "0:/a\n"} {
		if _, err := parsePaneDir(out, pane); !errors.Is(err, term.ErrNoPaneDir) {
			t.Fatalf("%q: %v", out, err)
		}
	}
	h := &Host{psmux: true}
	if h.ReportsPaneDirs() {
		t.Fatal("psmux reports pane folders")
	}
	if _, ok := term.PaneDirOf(h); ok {
		t.Fatal("PaneDirOf accepts a psmux host")
	}
	if _, ok := term.PaneDirOf(&Host{}); !ok {
		t.Fatal("PaneDirOf refuses a tmux host")
	}
}
