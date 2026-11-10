package term

import "context"

// PaneEvidence is what the multiplexer reports about one pane at one moment,
// read in the same invocation as the rows it is delivered with (spine AM-5).
// The turn extractor decides from it, and only from it, whether a row index
// still names the row it named at delivery.
//
// Collected and Generation are tmux 3.8 counters and are -1 on a multiplexer
// that does not have them. Without them nothing shows that an index still
// names the same row, so no turn can be extracted.
type PaneEvidence struct {
	PaneID  string
	Command string // pane_current_command
	PID     int    // pane_pid; 0 for a dead pane

	Dead      bool // the program ended and the pane is kept
	InputOff  bool // input is disabled: the multiplexer drops it
	Alternate bool // the alternate screen is on: no scrollback is written

	Width        int
	Height       int
	HistoryLimit int
	HistorySize  int // rows of scrollback that are held now
	CursorX      int
	CursorY      int
	RegionUpper  int // top row of the scroll region; 0 when the region is the whole screen

	// Collected counts every row that ever entered the scrollback, trimmed
	// rows included. The absolute index of screen row y is
	// Collected + HistorySize + y, and it survives scrolling, trims, height
	// changes and live history-limit changes.
	Collected int64
	// Generation rises on every clear of the scrollback and on every width
	// change (a reflow): an index read before it may name another row.
	Generation int64
}

// HasCounters reports whether the pane's multiplexer gave the counters that
// make absolute row indexes meaningful (tmux 3.8 or later).
func (e PaneEvidence) HasCounters() bool { return e.Collected >= 0 && e.Generation >= 0 }

// ScreenTop is the absolute index of the first visible row.
func (e PaneEvidence) ScreenTop() int64 { return e.Collected + int64(e.HistorySize) }

// LastRow is the absolute index of the last visible row.
func (e PaneEvidence) LastRow() int64 { return e.ScreenTop() + int64(e.Height) - 1 }

// DeliveryMark is the state of the pane read in the invocation that delivers
// a message, before the message reaches the program. Rows are the sanitised
// rows from absolute index From to the last screen row; they include a few
// history rows above the screen when there are any. Rows is empty when the
// multiplexer has no counters.
type DeliveryMark struct {
	PaneEvidence
	From int64
	Rows []string
}

// Frame is one read of the pane: the evidence and the sanitised rows read in
// the same invocation. From is the absolute index the caller asked for. The
// rows start at Start(): the multiplexer clamps a start that was trimmed away
// (From < Collected, which the extractor reads as trimmed) and the host starts
// no later than the first visible row, so the visible rows are always the last
// Height rows. Rows is empty when the multiplexer has no counters.
type Frame struct {
	PaneEvidence
	From int64
	Rows []string
}

// Start returns the absolute index of Rows[0].
func (f Frame) Start() int64 {
	s := f.From
	if s < f.Collected {
		s = f.Collected
	}
	if top := f.ScreenTop(); s > top {
		s = top
	}
	return s
}

// Anchorer is the optional port that gives a turn its delivery evidence. The
// pipe delivery type-asserts the SessionHost for it; a host without it still
// delivers the message, and no turn is extracted (reason no_evidence). The
// tmux host implements it. The ConPTY host does not yet.
type Anchorer interface {
	// PasteMarked pastes text into the pane verbatim, as one invocation that
	// reads the evidence and the rows first, and then pastes only when the
	// pane still is expect (KEEP): same pane id, pid and current command, the
	// program running, input enabled.
	//
	// Nothing is added to the text. It must not be empty, must be valid UTF-8
	// of at most MaxPasteBytes and must hold no control character other than
	// TAB and LF; otherwise PasteMarked returns an error matching
	// ops.ErrInvalidArgument and sends nothing. A refusal by the KEEP check
	// returns ErrPaneChanged and sends nothing. An invocation that does not
	// report its outcome is an error that does not match ErrPaneChanged: the
	// caller reports unknown.
	PasteMarked(ctx context.Context, pane PaneRef, text string, expect PaneRef) (DeliveryMark, error)

	// ReadFrame reads the evidence and the rows from absolute index from to
	// the last screen row in one invocation.
	ReadFrame(ctx context.Context, pane PaneRef, from int64) (Frame, error)
}
