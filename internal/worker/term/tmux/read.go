package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// MaxCaptureLines is the largest scrollback depth Capture accepts.
const MaxCaptureLines = 100000

// geometryFormat reads the size and cursor of a pane.
const geometryFormat = "#{pane_width}\t#{pane_height}\t#{cursor_x}\t#{cursor_y}\t#{cursor_flag}"

// reader is the read half of term.SessionHost that this file set implements.
type reader interface {
	ListSessions(ctx context.Context) ([]term.Session, error)
	ResolvePane(ctx context.Context, target ops.Target) (term.PaneRef, error)
	Snapshot(ctx context.Context, pane term.PaneRef) (ops.Screen, error)
	Capture(ctx context.Context, pane term.PaneRef, lines int) (ops.Screen, error)
}

var _ reader = (*Host)(nil)

var paneIDRE = regexp.MustCompile(`^%[0-9]{1,9}$`)

// Snapshot returns the visible screen of the pane, with the pane size and the
// cursor. The text comes from "capture-pane -p -e -t %id" and passes through
// term.Sanitize and term.BuildScreen, so no escape sequence other than the
// allow-listed SGR survives (AD-14). A pane that does not exist returns
// term.ErrNotFound.
func (h *Host) Snapshot(ctx context.Context, pane term.PaneRef) (ops.Screen, error) {
	if err := checkPaneID(pane.ID); err != nil {
		return ops.Screen{}, err
	}
	raw, err := h.run(ctx, "capture-pane", "-p", "-e", "-t", pane.ID)
	if err != nil {
		return ops.Screen{}, err
	}
	g, err := h.geometry(ctx, pane.ID)
	if err != nil {
		return ops.Screen{}, err
	}
	opts := []term.BuildOption{term.WithSize(g.width, g.height)}
	if g.cursorVisible {
		opts = append(opts, term.WithCursor(g.cursorY, g.cursorX))
	}
	scr, complete := buildScreen(raw, opts...)
	// capture-pane drops trailing blank rows in some versions; keep the screen
	// exactly as high as the pane so that the cursor row always has a line.
	for g.height > 0 && len(scr.Lines) < g.height {
		scr.Lines = append(scr.Lines, "")
	}
	if !complete {
		// The screen builder is bounded (rows, columns of 4096, cells): content
		// of this pane was dropped. The screen is returned, because a partial
		// screen is still worth reading, with the error that tells a caller there
		// is nothing complete to bind a write to.
		return scr, fmt.Errorf("pane %s: the visible screen exceeds the limits of the screen builder: %w", pane.ID, term.ErrIncomplete)
	}
	return scr, nil
}

// Capture returns the last lines rows of the pane including scrollback
// ("capture-pane -p -e -t %id -S -<lines>"), sanitised like Snapshot. Blank
// rows at the bottom of the visible area do not count: the result ends with the
// last non-blank row. It carries the pane width and no cursor, because the
// cursor row of the visible screen is not a row of the capture. lines must be
// between 1 and MaxCaptureLines; otherwise the error matches
// ops.ErrInvalidArgument.
func (h *Host) Capture(ctx context.Context, pane term.PaneRef, lines int) (ops.Screen, error) {
	if err := checkPaneID(pane.ID); err != nil {
		return ops.Screen{}, err
	}
	if lines < 1 || lines > MaxCaptureLines {
		return ops.Screen{}, fmt.Errorf("lines %d: must be between 1 and %d: %w", lines, MaxCaptureLines, ops.ErrInvalidArgument)
	}
	raw, err := h.run(ctx, "capture-pane", "-p", "-e", "-t", pane.ID, "-S", "-"+strconv.Itoa(lines))
	if err != nil {
		return ops.Screen{}, err
	}
	g, err := h.geometry(ctx, pane.ID)
	if err != nil {
		return ops.Screen{}, err
	}
	text := tailRows(bytes.TrimSuffix(term.Sanitize(raw), []byte("\n")), lines)
	scr, complete := term.BuildScreenChecked(text, term.WithSize(g.width, 0))
	if !complete {
		return ops.Screen{}, fmt.Errorf("%w: %w", ErrCaptureTooLarge, term.ErrIncomplete)
	}
	if err := checkComplete(text, scr); err != nil {
		return ops.Screen{}, err
	}
	return tailScreen(scr, lines), nil
}

// tailRows keeps the last n rows of capture text, ignoring blank rows at the
// bottom. The screen builder is bounded and keeps the oldest rows when its
// budget runs out, so the rows that were asked for (the newest) are cut out
// before it runs. capture-pane -e ends every row in a reset of the colours, so
// a row never depends on the one before it.
func tailRows(text []byte, n int) []byte {
	end := len(text)
	for end > 0 && text[end-1] == '\n' {
		end--
	}
	text = text[:end]
	count := 0
	for i := len(text) - 1; i >= 0; i-- {
		if text[i] == '\n' {
			count++
			if count == n {
				return text[i+1:]
			}
		}
	}
	return text
}

// ErrCaptureTooLarge reports that a capture does not fit the limits of the
// screen builder; the caller should ask for fewer lines.
var ErrCaptureTooLarge = errors.New("tmux: capture exceeds the screen builder limits; request fewer lines")

var sgrRE = regexp.MustCompile("\x1b\\[[0-9;:]*m")

// checkComplete makes sure the screen builder did not silently drop rows or
// cells of the text (it is bounded in rows, columns and total cells). Its
// budget is spent in reading order, so if the last row survived whole, all rows
// did.
func checkComplete(text []byte, scr ops.Screen) error {
	if len(text) == 0 {
		return nil
	}
	rows := strings.Split(string(text), "\n")
	if len(scr.Lines) < len(rows) {
		return ErrCaptureTooLarge
	}
	last := strings.TrimRight(sgrRE.ReplaceAllString(rows[len(rows)-1], ""), " ")
	got := strings.TrimRight(scr.Lines[len(rows)-1], " ")
	if utf8.RuneCountInString(got) < utf8.RuneCountInString(last) {
		return ErrCaptureTooLarge
	}
	return nil
}

func checkPaneID(id string) error {
	if !paneIDRE.MatchString(id) {
		return fmt.Errorf("pane id %q: must look like %%3: %w", truncate(id, 40), ops.ErrInvalidArgument)
	}
	return nil
}

// buildScreen sanitises raw capture-pane output and builds the screen. The one
// newline that ends the output is not a row of its own.
func buildScreen(raw []byte, opts ...term.BuildOption) (ops.Screen, bool) {
	raw = bytes.TrimSuffix(term.Sanitize(raw), []byte("\n"))
	return term.BuildScreenChecked(raw, opts...)
}

type geometry struct {
	width, height int
	cursorX       int
	cursorY       int
	cursorVisible bool
}

func (h *Host) geometry(ctx context.Context, id string) (geometry, error) {
	out, err := h.run(ctx, "display-message", "-p", "-t", id, geometryFormat)
	if err != nil {
		return geometry{}, err
	}
	return parseGeometry(string(out))
}

func parseGeometry(out string) (geometry, error) {
	f := strings.Split(strings.TrimRight(out, "\n"), "\t")
	if len(f) != 5 {
		return geometry{}, fmt.Errorf("tmux: display-message returned %d fields, want 5", len(f))
	}
	var n [5]int
	for i := range n {
		v, err := strconv.Atoi(f[i])
		if err != nil {
			return geometry{}, fmt.Errorf("tmux: display-message field %d: %w", i+1, err)
		}
		n[i] = v
	}
	return geometry{width: n[0], height: n[1], cursorX: n[2], cursorY: n[3], cursorVisible: n[4] == 1}, nil
}

// tailScreen drops trailing blank rows and keeps the last n rows, moving the
// style spans with their lines.
func tailScreen(s ops.Screen, n int) ops.Screen {
	end := len(s.Lines)
	for end > 0 && s.Lines[end-1] == "" {
		end--
	}
	start := max(end-n, 0)
	spans := make([]ops.StyleSpan, 0, len(s.Spans))
	for _, sp := range s.Spans {
		if sp.Line >= start && sp.Line < end {
			sp.Line -= start
			spans = append(spans, sp)
		}
	}
	s.Lines = append(make([]string, 0, end-start), s.Lines[start:end]...)
	s.Spans = spans
	return s
}
