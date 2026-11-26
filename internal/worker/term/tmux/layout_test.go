package tmux

import (
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

func TestParseLayout(t *testing.T) {
	c, err := newPaneCodec()
	if err != nil {
		t.Fatal(err)
	}
	row := func(f ...string) string { return strings.Join(f, c.fs) + c.rs + "\n" }
	out := row("0", "main", "b25f,160x48,0,0{80x48,0,0,1,79x48,81,0,2}", "1", "1", "0", "0", "100", "bash", "/srv/a", "") +
		// A window name and folder with a tab and a newline stay intact.
		row("0", "main", "b25f,160x48,0,0{80x48,0,0,1,79x48,81,0,2}", "1", "1", "1", "1", "101", "claude", "/srv/a\tb\nc", "/bin/sh -lc \": minion-keep claude -; x\"") +
		row("2", "lo\tgs", "abcd,160x48,0,0,3", "0", "0", "0", "1", "", "bash", "/gone", "")
	l, err := c.parseLayout(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Windows) != 2 || l.Windows[0].Name != "main" || !l.Windows[0].Active || !l.Windows[0].AutoRename || len(l.Windows[0].Panes) != 2 {
		t.Fatalf("layout = %+v", l)
	}
	p := l.Windows[0].Panes[1]
	if p.Index != 1 || !p.Active || p.PID != 101 || p.Command != "claude" || p.Cwd != "/srv/a\tb\nc" || !strings.Contains(p.StartCommand, "minion-keep") {
		t.Fatalf("pane = %+v", p)
	}
	if w := l.Windows[1]; w.Index != 2 || w.Name != "lo\tgs" || w.AutoRename || w.Panes[0].PID != 0 {
		t.Fatalf("window 2 = %+v", w)
	}
	if _, err := c.parseLayout(""); !errors.Is(err, term.ErrNotFound) {
		t.Fatalf("empty output: %v", err)
	}
	if _, err := c.parseLayout(row("x")); err == nil {
		t.Fatal("a short row was accepted")
	}
	if _, err := c.parseLayout("0" + c.fs + "unterminated"); err == nil {
		t.Fatal("an unterminated row was accepted")
	}
}
