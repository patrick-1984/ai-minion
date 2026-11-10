package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Turn anchoring on tmux (spine AM-5, Story 7.2 Option B). The adapter reads
// the evidence that fixes the position of a reply and never decides what the
// evidence means: that is the turn extractor's job.
//
// Every read is one tmux invocation, because tmux runs one command queue
// without reading pane output in between, while two invocations race with an
// agent that prints. Measured on 3.4, 3.6a and 3.8: 150 of 150 consistent in
// one invocation, 74 to 88 of 150 inconsistent in two.

// MinTurnMajor and MinTurnMinor are the oldest tmux with the counters that
// make an absolute row index meaningful (history_collected and
// history_generation). Before it the message is still delivered, but no turn
// can be extracted.
const (
	MinTurnMajor = 3
	MinTurnMinor = 8
)

// markHistoryRows is the number of history rows read above the screen with a
// delivery mark. They are the rows above the echo when the echo lands at the
// top of the screen; the extractor uses them to tell a reprint of the
// transcript from a reply.
const markHistoryRows = 4

var _ term.Anchorer = (*Host)(nil)

// evidenceFormat reads the evidence in one line. The command is last: it is the
// one field that may hold any byte, and tmux does not expand it again. Before
// 3.8 the two counters expand to the empty string.
const evidenceFormat = "#{pane_id}\t#{pane_pid}\t#{pane_dead}\t#{pane_input_off}\t#{alternate_on}\t" +
	"#{pane_width}\t#{pane_height}\t#{cursor_x}\t#{cursor_y}\t#{scroll_region_upper}\t" +
	"#{history_limit}\t#{history_size}\t#{history_collected}\t#{history_generation}\t#{pane_current_command}"

const evidenceFields = 15

// TurnsSupported reports whether the tmux of this host has the counters that
// turn extraction needs (3.8 or later).
func (h *Host) TurnsSupported() bool { return h.version.AtLeast(MinTurnMajor, MinTurnMinor) }

// formatEscape escapes a literal for use as an argument of a tmux format
// comparison: # as ##, a comma as #, and a closing brace as #}. Escaping the
// colon as well would break the match.
var formatEscape = strings.NewReplacer("#", "##", ",", "#,", "}", "#}").Replace

// keepPredicate is the KEEP check of a paste as a tmux format, true only when
// the pane still is expect and takes input: pane_pid and pane_current_command
// equal it, the program has not ended and input is not disabled. A pid alone
// is not enough: an agent that exited back to the shell keeps its pane's pid.
func keepPredicate(expect term.PaneRef) string {
	return fmt.Sprintf("#{&&:#{==:#{pane_pid},%d},#{&&:#{==:#{pane_current_command},%s},#{&&:#{==:#{pane_dead},0},#{==:#{pane_input_off},0}}}}",
		expect.PID, formatEscape(expect.Command))
}

// checkPasteText is the validation of PasteMarked: the text must be pasteable
// byte for byte. The default tmux paste turns LF into CR, tmux 3.8 rewrites
// control bytes as visible text, a CR submits early and an ESC can end a
// bracketed paste. TAB and LF are the only control characters the verbatim
// paste (paste-buffer -p -r) delivers unchanged on every version.
//
// The rule is term.CheckPasteText, the one every adapter shares.
func checkPasteText(text string) error { return term.CheckPasteText(text) }

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return true
		}
	}
	return false
}

func newToken(kind string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("tmux: no randomness for a result token: %w", err)
	}
	return "minion-" + kind + "-" + hex.EncodeToString(b[:]), nil
}

// parseEvidence parses the line printed for evidenceFormat.
func parseEvidence(line string) (term.PaneEvidence, error) {
	f := strings.SplitN(line, "\t", evidenceFields)
	if len(f) != evidenceFields {
		return term.PaneEvidence{}, fmt.Errorf("tmux: the evidence line has %d fields, want %d", len(f), evidenceFields)
	}
	if f[0] == "" {
		return term.PaneEvidence{}, fmt.Errorf("tmux: the pane does not exist: %w", term.ErrNotFound)
	}
	ev := term.PaneEvidence{PaneID: f[0], Command: f[14], Collected: -1, Generation: -1}
	num := func(i int, empty int64) (int64, error) {
		if f[i] == "" {
			return empty, nil
		}
		v, err := strconv.ParseInt(f[i], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("tmux: evidence field %d %q is not a number", i+1, truncate(f[i], 20))
		}
		return v, nil
	}
	flag := func(i int) (bool, error) {
		switch f[i] {
		case "0", "":
			return false, nil
		case "1":
			return true, nil
		}
		return false, fmt.Errorf("tmux: evidence flag %d %q is not 0 or 1", i+1, truncate(f[i], 20))
	}
	var err error
	var v [14]int64
	for i := 1; i < 14; i++ {
		switch i {
		case 2, 3, 4: // flags, read below
			continue
		case 12, 13:
			v[i], err = num(i, -1)
		default:
			v[i], err = num(i, 0)
		}
		if err != nil {
			return term.PaneEvidence{}, err
		}
	}
	if ev.Dead, err = flag(2); err != nil {
		return term.PaneEvidence{}, err
	}
	if ev.InputOff, err = flag(3); err != nil {
		return term.PaneEvidence{}, err
	}
	if ev.Alternate, err = flag(4); err != nil {
		return term.PaneEvidence{}, err
	}
	ev.PID = int(v[1])
	ev.Width, ev.Height = int(v[5]), int(v[6])
	ev.CursorX, ev.CursorY = int(v[7]), int(v[8])
	ev.RegionUpper = int(v[9])
	ev.HistoryLimit, ev.HistorySize = int(v[10]), int(v[11])
	ev.Collected, ev.Generation = v[12], v[13]
	if ev.Height < 1 || ev.Width < 1 || ev.HistorySize < 0 {
		return term.PaneEvidence{}, fmt.Errorf("tmux: implausible pane size %dx%d with %d history rows", ev.Width, ev.Height, ev.HistorySize)
	}
	return ev, nil
}

// splitOutput splits tmux output into lines, dropping the one newline that
// ends the last line.
func splitOutput(out []byte) []string {
	s := strings.TrimSuffix(string(out), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// readRows takes the rows of a capture out of the lines between the evidence
// and the token. want is the number of rows the evidence implies. tmux may
// leave out blank rows at the end, which are put back; more rows than the
// evidence implies mean the output is not what was asked for.
func readRows(lines []string, want int) ([]string, error) {
	if want < 0 {
		want = 0
	}
	if len(lines) > want {
		return nil, fmt.Errorf("tmux: capture returned %d rows, the evidence implies %d", len(lines), want)
	}
	rows := make([]string, want)
	for i := range rows {
		if i < len(lines) {
			// One row at a time: a damaged escape sequence in one row must
			// not swallow the line breaks of the rows after it.
			rows[i] = strings.ReplaceAll(term.SanitizeString(lines[i]), "\n", "")
		}
	}
	return rows, nil
}

// PasteMarked pastes text into the pane verbatim and returns the delivery
// mark read in the same invocation.
//
// The text goes through a uniquely named buffer (load-buffer reads it from the
// standard input). The delivery is one invocation:
//
//	display-message -p        the evidence
//	capture-pane -p           the rows before the paste (3.8 or later)
//	if-shell -F <KEEP>        paste-buffer -d -p -r, then an ok token
//	                          else delete-buffer, then a refused token
//
// The condition is the whole KEEP predicate (see keepPredicate). A successful
// exit status proves nothing: if-shell -F with a false condition exits 0, and
// so does a paste into a pane with input disabled. The outcome is the token.
// The refused token returns term.ErrPaneChanged, and nothing was sent. No token
// is an error that does not match ErrPaneChanged: the caller reports unknown.
// The tokens are read before the exit status: a user hook (after-paste-buffer)
// that fails makes tmux exit nonzero after the paste, and the ok token still
// says it happened. Once the ok token is there the paste happened, and the result is a nil error
// even when the evidence or the rows cannot be read: the mark then carries no
// evidence and the turn is unavailable (no_evidence).
//
// Before tmux 3.8 the message is delivered the same way and the mark carries
// the evidence without counters and no rows, so that no turn is extracted
// (reason tmux_too_old).
func (h *Host) PasteMarked(ctx context.Context, pane term.PaneRef, text string, expect term.PaneRef) (term.DeliveryMark, error) {
	return h.pasteGuarded(ctx, pane, text, expect, h.TurnsSupported())
}

// pasteGuarded is the one guarded, bracketed and verbatim paste of the
// adapter (paste-buffer -p -r, the KEEP predicate in the same tmux invocation
// as the paste). PasteMarked and PasteText both are this function; they differ
// only in counters, which asks for the rows of the pane before the paste
// (tmux 3.8 or later) so that the mark can anchor a turn.
func (h *Host) pasteGuarded(ctx context.Context, pane term.PaneRef, text string, expect term.PaneRef, counters bool) (term.DeliveryMark, error) {
	if err := checkPaneID(pane.ID); err != nil {
		return term.DeliveryMark{}, err
	}
	if err := checkPasteText(text); err != nil {
		return term.DeliveryMark{}, err
	}
	if expect.ID != pane.ID {
		return term.DeliveryMark{}, fmt.Errorf("expected pane %q, writing to %q: %w", expect.ID, pane.ID, term.ErrPaneChanged)
	}
	if hasControl(expect.Command) || expect.PID <= 0 {
		return term.DeliveryMark{}, fmt.Errorf("pane %s: the expected process cannot be checked: %w", pane.ID, term.ErrPaneChanged)
	}
	buf, err := newBufferName()
	if err != nil {
		return term.DeliveryMark{}, err
	}
	okTok, err := newToken("ok")
	if err != nil {
		return term.DeliveryMark{}, err
	}
	refTok, err := newToken("refused")
	if err != nil {
		return term.DeliveryMark{}, err
	}
	if _, err := h.runStdin(ctx, []byte(text), "load-buffer", "-b", buf, "-"); err != nil {
		h.deleteBuffer(buf)
		return term.DeliveryMark{}, err
	}
	args := []string{"display-message", "-p", "-t", pane.ID, evidenceFormat}
	if counters {
		args = append(args, ";", "capture-pane", "-p", "-t", pane.ID, "-S", "-"+strconv.Itoa(markHistoryRows))
	}
	args = append(args, ";", "if-shell", "-F", "-t", pane.ID, keepPredicate(expect),
		"paste-buffer -d -p -r -b "+buf+" -t "+pane.ID+" ; display-message -p "+okTok,
		"delete-buffer -b "+buf+" ; display-message -p "+refTok)
	// The output is kept when the invocation fails: a user hook on
	// after-paste-buffer that fails makes tmux exit nonzero after the paste
	// (and may print the ok token before or not at all). The tokens decide
	// what happened; the exit status only matters when there is no token.
	out, runErr := h.runKeepOutput(ctx, nil, args...)
	lines := splitOutput(out)
	last := ""
	if len(lines) > 0 {
		last = lines[len(lines)-1]
	}
	if runErr != nil && last != okTok && last != refTok {
		h.deleteBuffer(buf)
		if errors.Is(runErr, term.ErrNotFound) && len(lines) == 0 {
			// The first command found no pane and printed nothing: nothing was
			// sent. With any output the first command ran, and a later failure
			// does not say whether the paste happened.
			return term.DeliveryMark{}, fmt.Errorf("pane %s is gone: %w", pane.ID, term.ErrPaneChanged)
		}
		if errors.Is(runErr, term.ErrNotFound) {
			// Not a missing pane: the paste may have happened. The message is
			// kept, the match with ErrNotFound is not.
			return term.DeliveryMark{}, fmt.Errorf("tmux: the paste reported no result (outcome unknown): %v", runErr)
		}
		return term.DeliveryMark{}, runErr
	}
	switch last {
	case refTok:
		// The refused token decides before anything is parsed: a refusal
		// always returns ErrPaneChanged, whatever the evidence looks like.
		if len(lines) >= 2 {
			if ev, err := parseEvidence(lines[0]); err == nil {
				return term.DeliveryMark{}, refusal(ev, expect)
			}
		}
		return term.DeliveryMark{}, fmt.Errorf("pane %s: the pane changed before the paste: %w", pane.ID, term.ErrPaneChanged)
	case okTok:
		return h.pastedMark(lines[:len(lines)-1], counters)
	}
	// Neither token: the invocation ended before the if-shell command
	// reported. It may or may not have pasted.
	h.deleteBuffer(buf)
	return term.DeliveryMark{}, fmt.Errorf("tmux: the paste reported no result (outcome unknown)")
}

// pastedMark builds the mark of a paste that the ok token reported. The paste
// happened, so no failure to read the evidence or the rows is an error: the
// caller would report the outcome as unknown and send no Enter, and the message
// would stay in the input box. A mark that cannot be read is returned with
// empty evidence, which the extractor retires as no_evidence.
func (h *Host) pastedMark(lines []string, counters bool) (term.DeliveryMark, error) {
	if len(lines) == 0 {
		return term.DeliveryMark{}, nil
	}
	ev, err := parseEvidence(lines[0])
	if err != nil {
		return term.DeliveryMark{}, nil
	}
	mark := term.DeliveryMark{PaneEvidence: ev}
	if !counters || !ev.HasCounters() {
		// A host built for 3.8 reads the counters; an older one never sees
		// them, and a tmux older than the one that was announced does not
		// either.
		mark.Collected, mark.Generation = -1, -1
		return mark, nil
	}
	back := min(markHistoryRows, ev.HistorySize)
	rows, err := readRows(lines[1:], back+ev.Height)
	if err != nil {
		return term.DeliveryMark{}, nil
	}
	mark.From = ev.ScreenTop() - int64(back)
	mark.Rows = rows
	return mark, nil
}

// refusal describes why the KEEP predicate was false, from the evidence read in
// the same invocation. The error matches term.ErrPaneChanged.
func refusal(ev term.PaneEvidence, expect term.PaneRef) error {
	switch {
	case ev.Dead:
		return fmt.Errorf("pane %s: its program has ended, nothing receives input: %w", ev.PaneID, term.ErrPaneChanged)
	case ev.PID != expect.PID || ev.Command != expect.Command:
		return fmt.Errorf("pane %s now runs %q (pid %d), expected %q (pid %d): %w",
			ev.PaneID, truncate(ev.Command, 40), ev.PID, truncate(expect.Command, 40), expect.PID, term.ErrPaneChanged)
	case ev.InputOff:
		return fmt.Errorf("pane %s: input is disabled, tmux would drop it: %w", ev.PaneID, term.ErrPaneChanged)
	}
	return fmt.Errorf("pane %s: the pane changed before the paste: %w", ev.PaneID, term.ErrPaneChanged)
}

// ReadFrame reads the evidence and the rows from absolute index from to the
// last screen row in one invocation. The start of the capture is a format that
// tmux expands at capture time, so that it is relative to the same history the
// evidence was read from:
//
//	-S '#{?#{e|>:X,0},0,X}'   X = #{e|-:FROM,#{e|+:#{history_collected},#{history_size}}}
//
// X is the requested row relative to the first visible row; a start below the
// visible rows is cut to the first visible row, so that the visible rows are
// always the last Height rows of the frame. There is no -E: the default end is
// the last screen row. A start older than the oldest
// history row is clamped by tmux without notice; the evidence shows it
// (from < Collected) and the extractor reads that as trimmed.
//
// A from beyond the last row (the history shrank under the caller) is not an
// error: the frame carries the evidence and the visible rows, and the extractor
// reads the reason from the evidence.
//
// Before tmux 3.8 only the evidence is read (Collected and Generation are -1).
func (h *Host) ReadFrame(ctx context.Context, pane term.PaneRef, from int64) (term.Frame, error) {
	if err := checkPaneID(pane.ID); err != nil {
		return term.Frame{}, err
	}
	if from < 0 {
		return term.Frame{}, fmt.Errorf("from %d: must not be negative: %w", from, ops.ErrInvalidArgument)
	}
	counters := h.TurnsSupported()
	args := []string{"display-message", "-p", "-t", pane.ID, evidenceFormat}
	if counters {
		x := fmt.Sprintf("#{e|-:%d,#{e|+:#{history_collected},#{history_size}}}", from)
		args = append(args, ";", "capture-pane", "-p", "-t", pane.ID, "-S", "#{?#{e|>:"+x+",0},0,"+x+"}")
	}
	out, err := h.run(ctx, args...)
	if err != nil {
		return term.Frame{}, err
	}
	lines := splitOutput(out)
	if len(lines) == 0 {
		return term.Frame{}, errors.New("tmux: the frame read returned nothing")
	}
	ev, err := parseEvidence(lines[0])
	if err != nil {
		return term.Frame{}, err
	}
	fr := term.Frame{PaneEvidence: ev, From: from}
	if !counters || !ev.HasCounters() {
		fr.Collected, fr.Generation = -1, -1
		return fr, nil
	}
	// from may lie beyond the last row: after clear-history (tmux 3.8 sets the
	// history size to 0 and does not raise history_collected) or after a reflow
	// that joined wrapped history rows. That is not an error: the evidence
	// shows it (the generation or the width changed) and the capture starts at
	// the first visible row, so the frame holds the visible rows.
	want := int(ev.LastRow() - fr.Start() + 1)
	rows, err := readRows(lines[1:], want)
	if err != nil {
		return term.Frame{}, err
	}
	fr.Rows = rows
	return fr, nil
}
