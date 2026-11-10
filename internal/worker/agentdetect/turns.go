package agentdetect

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/pipe"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// MaxTurnBytes is the largest turn text the extractor emits. A longer reply is
// cut at a line boundary and ends with TruncatedMarker, so a runaway agent
// cannot make the Worker hold or ship an unbounded turn.
const MaxTurnBytes = 256 * 1024

// TruncatedMarker is the last line of a turn that was cut at MaxTurnBytes.
const TruncatedMarker = "[output truncated]"

// maxTrackedLines bounds the rows of one reply held in memory.
const maxTrackedLines = 50000

// quietFrames is the number of stable frames, beyond Settle, after which a
// verified echo with no reply and no busy frame is taken as a completed
// delivery with an empty reply.
const quietFrames = 2

// reprintContext is the number of rows above the echo that must appear again,
// together with the echo, for a reprint of the transcript.
const reprintContext = 3

// Turn is one agent reply extracted from the screen (AD-23, R29). It is plain
// text: the input rows are sanitised (AD-14) and the extractor drops any
// control character that is left. The Worker commits it to the pipe broker; the
// mapping to pipe.Turn is the peer {worker_id, Session} plus these fields.
type Turn struct {
	// TurnID is a UUIDv7 in canonical text form; it makes the commit idempotent.
	TurnID string `json:"turn_id"`
	// Session is the name of the session whose agent produced the reply.
	Session string `json:"session"`
	// Text is the reply, without prompt, status line, footer or escapes.
	Text string `json:"text"`
	// At is the time the reply was complete (the busy to stable transition).
	At time.Time `json:"at"`
}

// TurnConfig configures a TurnExtractor.
type TurnConfig struct {
	// Session is the session name carried by every turn. Required.
	Session string
	// Agent is the CLI name of the pane's agent (the part after "agent:" in
	// the kind). It selects the agent's screen markers from Agents; an unknown
	// or empty name uses the generic markers only.
	Agent string
	// Agents is the table of known agents; DefaultAgents() when nil.
	Agents []Agent
	// Settle is the number of consecutive stable frames that complete a turn.
	// It defaults to 1: the first stable frame. A higher value rides over an
	// agent that is stable for one poll between two tool calls.
	Settle int
	// Now returns the time stamped on a turn; time.Now when nil.
	Now func() time.Time
	// NewID returns a UUIDv7 for the given time; pipe.NewTurnID when nil.
	NewID func(now time.Time) (string, error)
}

// Reasons of ErrTurnUnavailable (spine AM-5). Each names the evidence that
// invalidated the delivery; none is a defect of the message.
const (
	// ReasonTmuxTooOld: the multiplexer has no row counters (tmux before 3.8).
	ReasonTmuxTooOld = "tmux_too_old"
	// ReasonNoEvidence: the host cannot give delivery evidence (no Anchorer,
	// or a mark that is not a read of the pane).
	ReasonNoEvidence = "no_evidence"
	// ReasonAlternateScreen: the alternate screen was on at delivery.
	ReasonAlternateScreen = "alternate_screen"
	// ReasonResized: the pane width changed since delivery (a reflow).
	ReasonResized = "resized"
	// ReasonHistoryCleared: the scrollback was cleared, or reflowed.
	ReasonHistoryCleared = "history_cleared"
	// ReasonHistoryTrimmed: rows of the reply that were not read yet left the
	// scrollback.
	ReasonHistoryTrimmed = "history_trimmed"
	// ReasonPaneRespawned: the pane's process is not the one of the delivery.
	ReasonPaneRespawned = "pane_respawned"
	// ReasonScrollRegion: a scroll region below row 0 is set, so rows may move
	// on screen where the index order cannot show it.
	ReasonScrollRegion = "scroll_region"
	// ReasonEvidenceMismatch: a row that was read as echo or as consumed reply
	// differs at its index.
	ReasonEvidenceMismatch = "evidence_mismatch"
	// ReasonTranscriptReprinted: the agent printed its transcript again.
	ReasonTranscriptReprinted = "transcript_reprinted"
	// ReasonEchoMismatch: the first changed row is not the echo of the message.
	ReasonEchoMismatch = "echo_mismatch"
	// ReasonEchoAmbiguous: the message also reads as an echo at an unchanged
	// row above the first changed row, so the start of the reply is not known.
	ReasonEchoAmbiguous = "echo_ambiguous"
)

// ErrTurnUnavailable is matched (errors.Is) by every error that retires a
// delivery without a turn. The message was delivered; its reply cannot be
// extracted with certainty, and the extractor emits nothing rather than guess.
var ErrTurnUnavailable = errors.New("agentdetect: no turn can be extracted for this delivery")

// UnavailableError is the error behind ErrTurnUnavailable. Callers read the
// reason with errors.As and log it with the session name and no content.
type UnavailableError struct{ Reason string }

func (e *UnavailableError) Error() string {
	return ErrTurnUnavailable.Error() + ": " + e.Reason
}

// Is makes errors.Is(err, ErrTurnUnavailable) true.
func (e *UnavailableError) Is(target error) bool { return target == ErrTurnUnavailable }

// Unavailable returns the error for reason, for callers that find out before a
// frame is read (a host without an Anchorer).
func Unavailable(reason string) error { return &UnavailableError{Reason: reason} }

// AnchorerOf returns the Anchorer of a host, or an UnavailableError with the
// reason no_evidence when the host has none. A host without it still delivers
// the message (PasteText); only the turn is lost.
func AnchorerOf(host term.SessionHost) (term.Anchorer, error) {
	if a, ok := host.(term.Anchorer); ok {
		return a, nil
	}
	return nil, Unavailable(ReasonNoEvidence)
}

// TurnExtractor extracts the reply of an agent to one delivered pipe message
// from the delivery evidence that tmux gives (spine AM-5, Story 7.2 Option B).
//
// The caller delivers with Anchorer.PasteMarked and hands the mark to
// Delivered. It then reads frames with Anchorer.ReadFrame(Need()) and gives
// each to Observe. The reply starts after the echo of the message, and the echo
// is the first row that changed since delivery: the extractor never searches
// the screen text for it, so a reply that quotes the message, an older echo of
// the same message and a row that repeats the prompt prefix cannot become the
// anchor. Whenever the evidence is invalidated (a resize, a trim past unread
// rows, a clear, a respawn, a scroll region, the alternate screen, a tmux
// without counters) the delivery is retired with ErrTurnUnavailable and no
// turn.
//
// The reply is the rows from the echo to the bottom chrome, taken by absolute
// row index: a row in history is kept when it is read, the visible rows and the
// history that was kept are read again on every frame (a pane that grows
// pulls history back for a redraw), so a redraw replaces what it redrew
// and never duplicates it, and a change to a kept row retires the delivery. The state that completes a turn is classified from the rows of
// the same frame, so a state from another read never completes it.
//
// The extractor is safe for concurrent use.
type TurnExtractor struct {
	cfg   TurnConfig
	agent *Agent

	mu    sync.Mutex
	armed bool
	sent  string // the delivered message without whitespace

	// The delivery.
	ev      term.PaneEvidence // evidence at delivery
	pre     string            // reason the mark itself gives, "" when usable
	top0    int64             // absolute index of the first visible row at delivery
	base    []string          // the visible rows at delivery; base[i] is row top0+i
	mfrom   int64             // absolute index of the first mark row
	mrows   []string          // the mark rows
	chrome0 int64             // absolute index of the first chrome row at delivery

	// Reading state.
	sawBusy bool
	stable  int // consecutive stable frames
	quiet   int // consecutive stable frames since the echo was verified
	// settled is set by the first stable frame that holds only the verified
	// echo. From then on the pane was seen at rest with nothing after the echo:
	// a busy frame or an echo of another input that follows cannot be tied to
	// this delivery.
	settled bool

	// Set once the echo is verified; never searched for again.
	anchored bool
	echoFrom int64    // absolute index of the first echo row
	echoTo   int64    // absolute index after the last echo row
	echoRows []string // the echo rows as read
	reprint  []string // the rows above the echo and the echo, for transcript_reprinted
	histFrom int64    // absolute index of hist[0]
	hist     []string // rows histFrom..final-1, all final
	final    int64    // rows below this index are history and read
	capped   bool     // hist reached maxTrackedLines; later rows are dropped
	reply    []string // the reply rows (raw) of the last accepted frame
}

// NewTurnExtractor returns an extractor for one session.
func NewTurnExtractor(cfg TurnConfig) (*TurnExtractor, error) {
	if strings.TrimSpace(cfg.Session) == "" {
		return nil, errors.New("agentdetect: turn extractor needs a session name")
	}
	if cfg.Settle < 1 {
		cfg.Settle = 1
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewID == nil {
		cfg.NewID = pipe.NewTurnID
	}
	if cfg.Agents == nil {
		cfg.Agents = DefaultAgents()
	}
	e := &TurnExtractor{cfg: cfg}
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == cfg.Agent && cfg.Agent != "" {
			e.agent = &cfg.Agents[i]
			break
		}
	}
	if e.agent == nil {
		e.agent = &Agent{}
	}
	return e, nil
}

// Delivered arms the extractor: a pipe message was pasted into the pane and m
// is the mark read in that paste's invocation, before the message reached the
// program. A delivery that was being followed is dropped; use Flush first to
// keep its reply. A mark that cannot anchor a turn (no counters, alternate
// screen, a scroll region) arms the extractor all the same: the first Observe
// returns the reason as ErrTurnUnavailable, so that every outcome reaches the
// caller on the same path.
func (e *TurnExtractor) Delivered(message string, m term.DeliveryMark) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.armed = true
	e.sent = stripSpace(message)
	e.sawBusy, e.stable, e.quiet, e.settled = false, 0, 0, false
	e.anchored, e.capped = false, false
	e.echoRows, e.reprint, e.hist, e.reply = nil, nil, nil, nil
	e.ev, e.pre = m.PaneEvidence, markReason(m)
	e.base, e.mrows = nil, nil
	if e.pre != "" {
		return
	}
	e.top0 = m.ScreenTop()
	e.mfrom = m.From
	e.mrows = append([]string(nil), m.Rows...)
	e.base = e.mrows[e.top0-m.From:]
	end, chrome := e.bottomEnd(e.base)
	if !chrome {
		end = len(e.base)
	}
	e.chrome0 = e.top0 + int64(end)
}

// Armed reports whether a delivered message is waiting for its reply.
func (e *TurnExtractor) Armed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.armed
}

// Need returns the absolute row index the next frame must start at: the caller
// passes it to Anchorer.ReadFrame. Before the echo is verified it is the first
// visible row at delivery; afterwards it is the first row that was read for
// the reply, so the visible rows and every row of history that was kept are
// read again. The read is bounded by the scrollback of the pane (a start that
// was trimmed away is clamped by the multiplexer).
//
// The caller reads frames only while Armed(). A delivery that its mark retired
// needs no rows (Observe returns the mark's reason from any frame); Need is
// then the screen top of the delivery, so that the read stays small. When the
// extractor is not armed Need is 0 and no frame is to be read.
func (e *TurnExtractor) Need() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.need()
}

func (e *TurnExtractor) need() int64 {
	if !e.armed {
		return 0
	}
	if e.pre != "" {
		// The mark retired the delivery: Observe returns its reason from any
		// frame, so the caller needs no rows. The screen top at delivery, when
		// known, keeps the read to the visible rows instead of the whole
		// scrollback.
		if e.ev.HasCounters() && e.ev.Height > 0 {
			return max(e.ev.ScreenTop(), 0)
		}
		return 0
	}
	if e.anchored {
		// The rows that became history are read again, all of them: a pane
		// that grows, redraws and shrinks between two polls shows the same
		// counters and the same size, and several such cycles reach rows
		// that lie further back than one pane height. Only the rows
		// themselves tell that a consumed one changed.
		return e.histFrom
	}
	return e.top0
}

// retire drops the delivery and returns the error that says why. The caller
// holds mu.
func (e *TurnExtractor) retire(reason string) error {
	e.disarm()
	return Unavailable(reason)
}

// disarm drops the delivery and what was read for it. The caller holds mu.
func (e *TurnExtractor) disarm() {
	e.armed = false
	e.base, e.mrows, e.echoRows, e.reprint, e.hist, e.reply = nil, nil, nil, nil, nil, nil
}

// Observe feeds one frame and returns the finished turn when the frame is
// stable and the reply is complete, otherwise nil.
//
// Everything it needs is in the frame: the rows, the evidence they were read
// with and the state, which it classifies from the frame's own visible rows. A
// frame read while the alternate screen is on is skipped, and a stable state is
// not honoured until the normal screen returns. A frame whose state is dialog
// or unknown never completes a turn.
//
// Known limit, for the caller that wires the extractor (S7.3): a reply whose
// first output (or busy indicator) shows only after a stable frame that held
// nothing but the echo is dropped silently (the extractor disarms, no turn and
// no Unavailable), because it cannot be told from a human exchange. Retiring
// it with a reason is the way to report it.
//
// An error that matches ErrTurnUnavailable retires the delivery (the extractor
// is disarmed, no turn will follow); read the reason with errors.As on
// *UnavailableError. Any other error leaves the extractor armed: a frame that
// does not match its evidence or does not start at Need, and a turn id that
// could not be generated (the next stable frame tries again).
func (e *TurnExtractor) Observe(f term.Frame) (*Turn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.armed {
		return nil, nil
	}
	if e.pre != "" {
		return nil, e.retire(e.pre)
	}
	if reason, skip := frameReason(e.ev, f.PaneEvidence); reason != "" {
		return nil, e.retire(reason)
	} else if skip {
		return nil, nil
	}
	if f.From > f.LastRow() {
		// A frame that starts below its own last row: the history shrank under
		// the reader without a generation or width change. The rows read so
		// far cannot be tied to this pane any more.
		return nil, e.retire(ReasonEvidenceMismatch)
	}
	if need := e.need(); f.From != need {
		return nil, fmt.Errorf("agentdetect: the frame starts at row %d, the extractor needs row %d", f.From, need)
	}
	if f.From < f.Collected {
		// Rows that were read and are final may leave the scrollback; only a
		// trim past unread rows loses part of the reply.
		if !e.anchored || e.final < f.Collected {
			return nil, e.retire(ReasonHistoryTrimmed)
		}
	}
	start, top, last := f.Start(), f.ScreenTop(), f.LastRow()
	if int64(len(f.Rows)) != last-start+1 {
		return nil, fmt.Errorf("agentdetect: the frame holds %d rows, its evidence implies %d", len(f.Rows), last-start+1)
	}
	rows := f.Rows
	vis := rows[top-start:]
	endRel, _ := e.bottomEnd(vis)
	endIdx := top + int64(endRel)

	state, _ := screenState(e.agent, ops.Screen{Width: f.Width, Height: f.Height, Lines: vis})
	switch state {
	case ops.PaneStateBusy:
		if e.settled {
			// The pane was at rest with nothing after the echo: the delivery
			// is an empty one (a command that only changed state). Work that
			// starts now may be a human's, and it is no evidence for this
			// delivery: the boundary stays where it was settled.
			e.disarm()
			return nil, nil
		}
		e.sawBusy, e.stable = true, 0
	case ops.PaneStateStable:
		e.stable++
	default:
		e.stable = 0
	}

	if !e.anchored {
		r := e.tryAnchor(rows, start, top, last, endIdx)
		switch {
		case r.reason != "":
			return nil, e.retire(r.reason)
		case !r.ok:
			if r.changed && state == ops.PaneStateStable && e.stable >= e.cfg.Settle {
				return nil, e.retire(ReasonEchoMismatch)
			}
			return nil, nil
		}
	}
	if reason := e.follow(rows, start, top, last, endIdx); reason != "" {
		return nil, e.retire(reason)
	}

	if state == ops.PaneStateStable {
		e.quiet++
	} else {
		e.quiet = 0
	}

	// A stable frame that holds only the verified echo settles the boundary,
	// whatever the Settle count: from then on a busy frame or an input that
	// follows cannot be tied to this delivery. wasSettled is what an earlier
	// frame saw.
	wasSettled := e.settled
	if state == ops.PaneStateStable && !e.hasReply() {
		e.settled = true
	}
	if state != ops.PaneStateStable || e.stable < e.cfg.Settle {
		return nil, nil
	}
	if wasSettled && e.hasReply() && (bareEcho[e.agent.Name] || e.userEcho()) {
		// The pane was at rest with only the echo, and now rows follow it that
		// are the echo of another input (a glyph shows it), or, for an agent
		// whose echo has no glyph, rows that nothing tells from another
		// exchange: a conversation that is not the reply to this message, or
		// one that cannot be shown to be. Fail closed. A reply that is read for
		// the first time with a row that looks like an input (a quoted line, an
		// "ask> " glyph) is a reply: nothing shows another input.
		e.disarm()
		return nil, nil
	}
	t, err := e.finish()
	if t == nil && err == nil && (e.sawBusy || e.quiet >= e.cfg.Settle+quietFrames) {
		// The agent is back at its prompt without having printed anything
		// after the echo (a command that only changed state, an interrupt),
		// seen working or, for a command that finished between two polls,
		// quiet for quietFrames more stable frames: the delivery is complete
		// and empty. Staying armed would turn the next unrelated conversation
		// into its reply.
		e.disarm()
	}
	return t, err
}

// hasReply reports whether the reply read so far holds a row of text.
func (e *TurnExtractor) hasReply() bool {
	for _, r := range e.reply {
		if !isBlankOrBorder(r) {
			return true
		}
	}
	return false
}

// userEcho reports whether the reply read so far starts with the echo of a
// prompt: a row in the first column (or inside a box) that starts with the
// prompt glyph of the agent.
func (e *TurnExtractor) userEcho() bool {
	if bareEcho[e.agent.Name] {
		return false // no glyph tells an echo from a reply
	}
	for _, r := range e.reply {
		if isBlankOrBorder(r) {
			continue
		}
		if boxedRowRe.MatchString(r) {
			r = strings.TrimSpace(boxInner(r))
		} else if r[0] == ' ' || r[0] == '\t' {
			return false
		}
		_, marked := echoFirst(r, modePrompts[e.agent.Name], false)
		return marked
	}
	return false
}

// Flush returns the reply read so far as a turn and disarms, or nil when
// nothing was read. The Worker calls it when it learns that the reply was cut
// short without a stable frame (the pane closed or the agent changed). It
// reports only what the last accepted frame showed with valid evidence: a
// delivery that was retired has nothing left to report.
func (e *TurnExtractor) Flush() (*Turn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.armed {
		return nil, nil
	}
	t, err := e.finish()
	if t == nil && err == nil {
		e.disarm()
	}
	return t, err
}

// finish emits the turn if there is any text and disarms. The caller holds mu.
func (e *TurnExtractor) finish() (*Turn, error) {
	text := e.text()
	if text == "" {
		// Nothing to report yet (the agent has not printed anything): the
		// caller keeps waiting (Observe) or drops the state (Flush).
		return nil, nil
	}
	now := e.cfg.Now()
	id, err := e.cfg.NewID(now)
	if err != nil {
		return nil, err
	}
	e.disarm()
	return &Turn{TurnID: id, Session: e.cfg.Session, Text: text, At: now}, nil
}

// text renders the reply of the last accepted frame. The caller holds mu.
func (e *TurnExtractor) text() string {
	if !e.anchored {
		return ""
	}
	lines := make([]string, len(e.reply))
	for i, l := range e.reply {
		lines[i] = e.cleanLine(l)
	}
	text := renderReply(lines)
	if e.capped && text != "" && !strings.HasSuffix(text, "\n"+TruncatedMarker) {
		text += "\n" + TruncatedMarker
	}
	return text
}
