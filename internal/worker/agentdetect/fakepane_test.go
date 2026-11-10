package agentdetect

import (
	"testing"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// fakePane is a pane as tmux 3.8 reports it: absolute rows, a first visible
// row, counters for trims and clears, and the evidence tmux gives with every
// read. The unit tests drive the extractor with it: a mark and a frame are
// emitted exactly as the tmux host emits them (term.Frame.Start clamps the
// same way).
//
// Row i of the pane has the absolute index collected + historySize + i, where
// historySize = top - collected. The operations keep that true: scrolling moves
// rows into history, a trim raises collected, a height change moves the first
// visible row without moving any row.
type fakePane struct {
	pid, width, height int
	limit              int
	counters           bool // false: tmux before 3.8
	alt                bool
	region             int
	command            string

	collected  int64
	generation int64
	top        int64    // absolute index of the first visible row
	rows       []string // rows from absolute index collected
}

func newFakePane(height int) *fakePane {
	return &fakePane{pid: 4242, width: 100, height: height, limit: 2000, counters: true,
		command: "agent", rows: make([]string, height)}
}

// ensure makes rows cover the absolute indexes below n.
func (p *fakePane) ensure(n int64) {
	for p.collected+int64(len(p.rows)) < n {
		p.rows = append(p.rows, "")
	}
}

func (p *fakePane) at(idx int64) *string {
	p.ensure(idx + 1)
	return &p.rows[idx-p.collected]
}

func (p *fakePane) size() int { return int(p.top - p.collected) }

// visible returns the visible rows.
func (p *fakePane) visible() []string {
	p.ensure(p.top + int64(p.height))
	return append([]string(nil), p.rows[p.top-p.collected:p.top-p.collected+int64(p.height)]...)
}

// setView writes the visible rows; fewer than the height leave the rest blank.
func (p *fakePane) setView(view []string) {
	if len(view) > p.height {
		panic("fakePane: view taller than the pane")
	}
	for i := 0; i < p.height; i++ {
		v := ""
		if i < len(view) {
			v = view[i]
		}
		*p.at(p.top + int64(i)) = v
	}
}

// push scrolls the screen by len(history) rows: those rows become the rows at
// the first visible indexes and leave through the top (history). Then the
// visible rows are view. A pane with a history limit trims like tmux does.
func (p *fakePane) push(history []string, view []string) {
	for i, h := range history {
		*p.at(p.top + int64(i)) = h
	}
	p.top += int64(len(history))
	p.setView(view)
	p.enforceLimit()
}

// step scrolls by n rows (the n first visible rows leave through the top) and
// shows view.
func (p *fakePane) step(n int, view []string) {
	vis := p.visible()
	if n > len(vis) {
		panic("fakePane: step beyond the screen; use push")
	}
	p.push(vis[:n], view)
}

func (p *fakePane) enforceLimit() {
	for p.size() > p.limit {
		p.trim(max(p.limit/10, 1))
	}
}

// trim drops the n oldest history rows (a history-limit trim).
func (p *fakePane) trim(n int) {
	if n > p.size() {
		n = p.size()
	}
	p.rows = p.rows[n:]
	p.collected += int64(n)
}

// resizeHeight changes the height as tmux does with the cursor below the
// content: a shorter pane first drops blank rows at the bottom and only then
// moves its top rows into history; a taller one pulls history rows back. No
// row changes its index.
func (p *fakePane) resizeHeight(h int) {
	switch {
	case h < p.height:
		drop := p.height - h
		vis := p.visible()
		blanks := 0
		for i := len(vis) - 1; i >= 0 && blanks < drop && vis[i] == ""; i-- {
			blanks++
		}
		p.top += int64(drop - blanks)
	case h > p.height:
		p.top -= int64(min(p.size(), h-p.height))
	}
	p.height = h
	p.ensure(p.top + int64(p.height))
}

// resizeWidth changes the width: a reflow, which raises the generation.
func (p *fakePane) resizeWidth(w int) { p.width = w; p.generation++ }

// clearHistory is clear-history or CSI 3 J as tmux 3.8 does it: the scrollback
// is gone and the generation rises, but history_collected is left alone
// (grid_clear_history sets the history size to 0 and does not raise the
// counter). The first visible row is then history_collected again, so its
// absolute index drops below where it was, and the visible rows are the only
// rows that remain.
func (p *fakePane) clearHistory() {
	p.rows = p.visible()
	p.top = p.collected
	p.generation++
}

// respawn is respawn-pane -k: a new process, a blank screen, the history kept,
// the generation unchanged.
func (p *fakePane) respawn() {
	p.pid++
	p.setView(nil)
}

func (p *fakePane) evidence() term.PaneEvidence {
	ev := term.PaneEvidence{
		PaneID: "%1", Command: p.command, PID: p.pid,
		Alternate: p.alt, Width: p.width, Height: p.height,
		HistoryLimit: p.limit, HistorySize: p.size(), CursorY: 0, RegionUpper: p.region,
		Collected: p.collected, Generation: p.generation,
	}
	if !p.counters {
		ev.Collected, ev.Generation = -1, -1
	}
	return ev
}

// mark is the delivery mark: the evidence and the rows from a few history rows
// above the screen to the last screen row.
func (p *fakePane) mark() term.DeliveryMark {
	ev := p.evidence()
	m := term.DeliveryMark{PaneEvidence: ev}
	if !p.counters {
		return m
	}
	back := min(4, p.size())
	m.From = p.top - int64(back)
	p.ensure(p.top + int64(p.height))
	m.Rows = append([]string(nil), p.rows[m.From-p.collected:p.top+int64(p.height)-p.collected]...)
	return m
}

// frame is a frame read from absolute row from.
func (p *fakePane) frame(from int64) term.Frame {
	f := term.Frame{PaneEvidence: p.evidence(), From: from}
	if !p.counters {
		return f
	}
	p.ensure(p.top + int64(p.height))
	start := f.Start()
	f.Rows = append([]string(nil), p.rows[start-p.collected:p.top+int64(p.height)-p.collected]...)
	return f
}

// drive couples a fake pane with an extractor the way the pipe delivery will:
// deliver reads the mark, poll reads the frame at Need() and observes it.
type drive struct {
	t      *testing.T
	p      *fakePane
	e      *TurnExtractor
	origin int64 // absolute index of transcript[0] for draw
}

// draw shows transcript[:n] followed by the claude chrome with the given
// status. Rows that no longer fit leave through the top; the first visible
// row never moves back.
func (d *drive) draw(transcript []string, n int, status string) {
	chrome := claudeView(nil, status)
	top := max(d.p.top, d.origin+int64(n+len(chrome)-d.p.height))
	row := func(idx int64) string {
		if idx < d.origin { // a row from before the transcript, still on the screen
			return *d.p.at(idx)
		}
		return transcript[idx-d.origin]
	}
	var hist, view []string
	for idx := d.p.top; idx < top; idx++ {
		hist = append(hist, row(idx))
	}
	for idx := top; idx < d.origin+int64(n); idx++ {
		view = append(view, row(idx))
	}
	d.p.push(hist, append(view, chrome...))
}

func (d *drive) deliver(msg string) { d.e.Delivered(msg, d.p.mark()) }

func (d *drive) poll() (*Turn, error) { return d.e.Observe(d.p.frame(d.e.Need())) }

// pollOK polls and fails the test on an error.
func (d *drive) pollOK() *Turn {
	d.t.Helper()
	turn, err := d.poll()
	if err != nil {
		d.t.Fatalf("poll: %v", err)
	}
	return turn
}
