package agentdetect

import (
	"fmt"
	"testing"
)

// reflow is a width change that joins wrapped history rows, as tmux does when
// the pane gets wider: n history rows disappear, the first visible row moves
// up by n, the counter of trimmed rows stays and the generation rises.
func (p *fakePane) reflow(w, n int) {
	if n > p.size() {
		n = p.size()
	}
	p.rows = p.rows[n:]
	p.top -= int64(n)
	p.width = w
	p.generation++
}

// longReply is a transcript of n reply rows after the echo of "hello".
func longReply(n int) []string {
	T := []string{"> hello", ""}
	for i := 1; i <= n; i++ {
		if i == 1 {
			T = append(T, fmt.Sprintf("● r%d", i))
		} else {
			T = append(T, fmt.Sprintf("  r%d", i))
		}
	}
	return T
}

// After a clear or a reflow the first visible row can have an absolute index
// below the last row the extractor consumed (tmux 3.8 leaves history_collected
// alone when it clears the history). The frame still carries the evidence, and
// the reason is read from it.
func TestShrunkHistoryGivesTheReasonNotAnError(t *testing.T) {
	setup := func(t *testing.T) *drive {
		d := newDrive(t, "claude", 12)
		d.p.setView(claudeView(nil, ""))
		d.deliver("hello")
		T := longReply(40)
		d.draw(T, len(T), claudeBusyLine)
		if turn := d.pollOK(); turn != nil {
			t.Fatalf("turn mid reply: %+v", turn)
		}
		if d.e.final <= int64(d.p.height) {
			t.Fatalf("the scenario needs consumed history beyond one screen, got %d", d.e.final)
		}
		return d
	}
	t.Run("clear-history or CSI 3 J after 40 rows", func(t *testing.T) {
		d := setup(t)
		final := d.e.final
		d.p.clearHistory()
		if f := d.p.frame(d.e.Need()); final <= f.LastRow() {
			t.Fatalf("the fake pane does not behave like tmux 3.8: consumed up to %d, last row %d", final, f.LastRow())
		}
		turn, err := d.poll()
		wantUnavailable(t, d, turn, err, ReasonHistoryCleared)
	})
	t.Run("widening that joins wrapped history rows", func(t *testing.T) {
		d := setup(t)
		final := d.e.final
		d.p.reflow(200, 28)
		if f := d.p.frame(d.e.Need()); final <= f.LastRow() {
			t.Fatalf("the reflow did not shrink the history: consumed up to %d, last row %d", final, f.LastRow())
		}
		turn, err := d.poll()
		wantUnavailable(t, d, turn, err, ReasonResized)
	})
	t.Run("a frame below its own last row without any other change is a mismatch", func(t *testing.T) {
		d := setup(t)
		f := d.p.frame(d.e.Need())
		f.Collected, f.HistorySize = 0, 0 // the evidence says the pane is short
		f.Rows = f.Rows[len(f.Rows)-d.p.height:]
		turn, err := d.e.Observe(f)
		wantUnavailable(t, d, turn, err, ReasonEvidenceMismatch)
	})
}

// The agent leaves for its shell in the middle of a reply: the pid of the pane
// is the same, but the process of the delivery is not running.
func TestAnotherProgramInThePaneRetiresTheDelivery(t *testing.T) {
	d := start(t, "claude", 24, claudeView(nil, ""), "hello")
	d.p.setView(claudeView([]string{"> hello", "", "● one"}, claudeBusyLine))
	d.pollOK()
	d.p.command = "bash"
	d.p.setView([]string{"> hello", "", "● one", "", "user@host:~$ "})
	turn, err := d.poll()
	wantUnavailable(t, d, turn, err, ReasonPaneRespawned)
	if turn, err := d.e.Flush(); turn != nil || err != nil {
		t.Fatalf("Flush after the retirement: turn=%+v err=%v", turn, err)
	}
}

// Flush reports what the last accepted frame showed: frames of another program
// are never accepted, so the shell rows are not the reply.
func TestFlushNeverReportsTheRowsOfAnotherProgram(t *testing.T) {
	d := start(t, "claude", 24, claudeView(nil, ""), "hello")
	d.p.setView(claudeView([]string{"> hello", "", "● one"}, claudeBusyLine))
	d.pollOK()
	d.p.command = "zsh"
	d.p.setView([]string{"> hello", "", "● one", "$ ls", "file1", "$ "})
	if _, err := d.poll(); reasonOf(err) != ReasonPaneRespawned {
		t.Fatalf("err = %v", err)
	}
	if turn, err := d.e.Flush(); turn != nil || err != nil {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
}

// A mark that retired the delivery needs no rows: Need() keeps the read to the
// visible rows.
func TestNeedOfADeliveryRetiredByItsMark(t *testing.T) {
	d := newDrive(t, "claude", 12)
	d.p.push(rowsN("h", 60), claudeView(nil, ""))
	d.p.region = 3 // scroll_region at delivery
	d.deliver("hello")
	if got, want := d.e.Need(), d.p.top; got != want {
		t.Fatalf("Need() = %d, want the screen top %d", got, want)
	}
	turn, err := d.poll()
	wantUnavailable(t, d, turn, err, ReasonScrollRegion)
	if d.e.Need() != 0 {
		t.Fatalf("Need() = %d when not armed", d.e.Need())
	}
}

// --- the echo reads literally first --------------------------------------------

func TestEchoEllipsisBelongsToTheMessage(t *testing.T) {
	t.Run("at the end of the first row", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "wait...\nthen go")
		d.p.setView(claudeView([]string{"> wait...", "  then go", "", "● ok"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "ok" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("at the end of a continuation row", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "first\nthen wait...\nand go")
		d.p.setView(claudeView([]string{"> first", "  then wait...", "  and go", "", "● ok"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "ok" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("a typographic ellipsis in the message", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "wait…\nthen go")
		d.p.setView(claudeView([]string{"> wait…", "  then go", "", "● ok"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "ok" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("a bare echo", func(t *testing.T) {
		d := start(t, "opencode", 24, ocView([]string{"  an older reply"}, false), "hmm...\nwhy")
		d.p.setView(ocView([]string{"  an older reply", "", "  hmm...", "  why", "", "  because"}, false))
		turn := d.pollOK()
		if turn == nil || turn.Text != "because" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("a cut echo of a message with dots is still a cut", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "hmm... the build system is slow")
		d.p.setView(claudeView([]string{"> hmm... the build sys…", "", "● ok"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "ok" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("a cut at dots that also occur in the message", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "first...\nsecond...\nthird")
		d.p.setView(claudeView([]string{"> first...", "  second...", "", "● ok"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "ok" {
			t.Fatalf("turn=%+v", turn)
		}
	})
}

func TestEchoRowWithAPromptGlyphThatContinuesTheMessage(t *testing.T) {
	t.Run("a markdown quote line", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "see this:\n> quoted\nok?")
		d.p.setView(claudeView([]string{"> see this:", "  > quoted", "  ok?", "", "● yes"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "yes" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("a boxed quote line", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "see this:\n> quoted\nok?")
		d.p.setView(claudeView([]string{"│ > see this:  │", "│   > quoted   │", "│   ok?        │", "", "● yes"}, ""))
		turn := d.pollOK()
		if turn == nil || turn.Text != "yes" {
			t.Fatalf("turn=%+v", turn)
		}
	})
	t.Run("a glyph row that does not continue the message still ends the echo", func(t *testing.T) {
		d := start(t, "claude", 24, claudeView(nil, ""), "see this and more")
		d.p.setView(claudeView([]string{"> see this", "> other", "", "● yes"}, ""))
		turn, err := d.poll()
		wantUnavailable(t, d, turn, err, ReasonEchoMismatch)
	})
}

func TestMatchEchoLiteralFirst(t *testing.T) {
	strip := stripSpace
	for _, c := range []struct {
		name    string
		lines   []string
		sent    string
		bare    bool
		wantEnd int
		wantOK  bool
	}{
		{"dots at the end of the first row", []string{"> wait...", "  then go", "● ok"}, strip("wait...\nthen go"), false, 2, true},
		{"dots at the end of a continuation row", []string{"> a", "  b...", "  c", "● ok"}, strip("a\nb...\nc"), false, 3, true},
		{"dots that are the whole message", []string{"> ..."}, strip("..."), false, 1, true},
		{"a cut", []string{"> Explain the build sys…", "● ok"}, strip("Explain the build system"), false, 1, true},
		{"a cut with dots", []string{"> Explain the build sys...", "● ok"}, strip("Explain the build system"), false, 1, true},
		{"the deepest cut wins", []string{"> a...", "  b...", "● ok"}, strip("a...\nb...\nc"), false, 2, true},
		{"a quote line", []string{"> see:", "  > q", "  ok?"}, strip("see:\n> q\nok?"), false, 3, true},
		{"a glyph row that does not continue", []string{"> see:", "  > other", "  ok?"}, strip("see:\nok?"), false, 0, false},
		{"bare dots", []string{"hmm...", "why", "because"}, strip("hmm...\nwhy"), true, 2, true},
		{"a partial echo", []string{"> Explain the build"}, strip("Explain the build system"), false, 0, false},
	} {
		end, ok := matchEcho(c.lines, 0, c.sent, c.bare, false)
		if end != c.wantEnd || ok != c.wantOK {
			t.Errorf("%s: end=%d ok=%v, want end=%d ok=%v", c.name, end, ok, c.wantEnd, c.wantOK)
		}
	}
}

// A pane that grows far, redraws a consumed row from the deep end of the
// history and shrinks again shows the same counters and size. The row lies
// further back than any fixed window of re-read rows; every kept row is read
// again, so the change is seen.
func TestDeepConsumedRowRewrittenByAGrowAndShrinkIsMismatch(t *testing.T) {
	d := newDrive(t, "claude", 10)
	d.p.limit = 1 << 30
	d.p.setView(claudeView(nil, ""))
	d.deliver("go")
	d.p.setView(claudeView([]string{"> go", "", "● start"}, claudeBusyLine))
	d.pollOK()
	history := d.p.visible()[:3] // the echo and its reply stay what they were
	for i := 0; len(history) < 25000; i++ {
		history = append(history, fmt.Sprintf("  row %d", i))
	}
	d.p.push(history, claudeView([]string{"  fresh line"}, claudeBusyLine))
	d.pollOK()
	d.p.setView(rowsN("  v", 10)) // no blank rows: a shrink moves the top back exactly
	top, collected := d.p.top, d.p.collected
	d.p.resizeHeight(15000)
	deep := d.p.top
	if top-deep < 12000 {
		t.Fatalf("the pane pulled back only %d rows", top-deep)
	}
	*d.p.at(deep) = "  REDRAWN"
	d.p.resizeHeight(10)
	if d.p.top != top || d.p.collected != collected {
		t.Fatalf("the round trip must leave the counters alone: top %d->%d", top, d.p.top)
	}
	turn, err := d.poll()
	wantUnavailable(t, d, turn, err, ReasonEvidenceMismatch)
}
