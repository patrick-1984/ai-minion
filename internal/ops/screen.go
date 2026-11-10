package ops

import (
	"encoding/json"
	"unicode/utf8"
)

// Screen is the one screen format of every surface: REST, wire, MCP, pipe and
// the web UI (AD-14). It is UTF-8 text lines plus SGR style spans plus the
// cursor. The Worker sanitises once, before building a Screen, so no line
// holds an escape byte; Validate checks that invariant.
type Screen struct {
	Width  int         `json:"width"`  // columns of the pane, 0 when unknown
	Height int         `json:"height"` // rows of the pane, 0 when unknown
	Lines  []string    `json:"lines"`  // plain UTF-8 text, one string per row, no control characters
	Spans  []StyleSpan `json:"spans"`  // style runs, ordered by line then start, never overlapping
	Cursor *Cursor     `json:"cursor"` // null when the cursor is hidden or unknown
}

// StyleSpan applies one SGR style to the rune range [Start, End) of one line.
// Offsets count runes of the line string, not terminal columns, so that a web
// renderer can slice the escaped text directly. Runs with the default style
// are not listed.
type StyleSpan struct {
	Line      int    `json:"line"`                // index into Lines
	Start     int    `json:"start"`               // first rune, inclusive
	End       int    `json:"end"`                 // last rune, exclusive, greater than Start
	FG        string `json:"fg,omitempty"`        // foreground color, see ValidColor
	BG        string `json:"bg,omitempty"`        // background color, see ValidColor
	Bold      bool   `json:"bold,omitempty"`      // SGR 1
	Dim       bool   `json:"dim,omitempty"`       // SGR 2
	Italic    bool   `json:"italic,omitempty"`    // SGR 3
	Underline bool   `json:"underline,omitempty"` // SGR 4
	Inverse   bool   `json:"inverse,omitempty"`   // SGR 7
	Strike    bool   `json:"strike,omitempty"`    // SGR 9
}

// IsDefault reports whether the span carries no style at all.
func (s StyleSpan) IsDefault() bool {
	return s.FG == "" && s.BG == "" && !s.Bold && !s.Dim && !s.Italic && !s.Underline && !s.Inverse && !s.Strike
}

// Cursor is the cursor position, zero-based, in runes of the line.
type Cursor struct {
	Row int `json:"row"`
	Col int `json:"col"`
}

// ValidColor reports whether c is a color in the screen model: "p0" to "p255"
// for a palette index (0 to 7 standard, 8 to 15 bright, 16 to 255 extended) or
// "#rrggbb" in lowercase hex for a true color.
func ValidColor(c string) bool {
	if len(c) == 7 && c[0] == '#' {
		for i := 1; i < 7; i++ {
			ch := c[i]
			if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
				return false
			}
		}
		return true
	}
	if len(c) < 2 || len(c) > 4 || c[0] != 'p' {
		return false
	}
	n := 0
	for i := 1; i < len(c); i++ {
		if c[i] < '0' || c[i] > '9' {
			return false
		}
		n = n*10 + int(c[i]-'0')
	}
	if len(c) > 2 && c[1] == '0' { // no leading zero
		return false
	}
	return n <= 255
}

// Validate checks the screen invariants: every line is valid UTF-8 without
// control characters (so no raw escape byte), spans are well-formed, ordered
// and non-overlapping, colors are valid, and the cursor is non-negative.
func (s Screen) Validate() error {
	if s.Width < 0 || s.Height < 0 {
		return invalid("screen", "negative size")
	}
	runes := make([]int, len(s.Lines))
	for i, line := range s.Lines {
		if !utf8.ValidString(line) {
			return invalid("lines", "line %d is not valid UTF-8", i)
		}
		for _, r := range line {
			if isControl(r) {
				return invalid("lines", "line %d contains a control character", i)
			}
		}
		runes[i] = utf8.RuneCountInString(line)
	}
	prevLine, prevEnd := -1, 0
	for i, sp := range s.Spans {
		switch {
		case sp.Line < 0 || sp.Line >= len(s.Lines):
			return invalid("spans", "span %d refers to a missing line", i)
		case sp.Start < 0 || sp.End <= sp.Start || sp.End > runes[sp.Line]:
			return invalid("spans", "span %d has an invalid range", i)
		case sp.Line < prevLine || (sp.Line == prevLine && sp.Start < prevEnd):
			return invalid("spans", "span %d is out of order or overlaps", i)
		case sp.FG != "" && !ValidColor(sp.FG), sp.BG != "" && !ValidColor(sp.BG):
			return invalid("spans", "span %d has an invalid color", i)
		}
		prevLine, prevEnd = sp.Line, sp.End
	}
	if s.Cursor != nil && (s.Cursor.Row < 0 || s.Cursor.Col < 0) {
		return invalid("cursor", "negative position")
	}
	return nil
}

// isControl reports C0 controls, DEL and C1 controls. Tabs are not kept: the
// sanitiser expands them to spaces.
func isControl(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r <= 0x9f)
}

// String returns the plain text of the screen, lines joined by newlines.
func (s Screen) String() string {
	n := 0
	for _, l := range s.Lines {
		n += len(l) + 1
	}
	b := make([]byte, 0, n)
	for i, l := range s.Lines {
		if i > 0 {
			b = append(b, '\n')
		}
		b = append(b, l...)
	}
	return string(b)
}

// MarshalJSON writes empty lines and spans as arrays instead of null, so the
// shape is the same for every screen.
func (s Screen) MarshalJSON() ([]byte, error) {
	type plain Screen
	if s.Lines == nil {
		s.Lines = []string{}
	}
	if s.Spans == nil {
		s.Spans = []StyleSpan{}
	}
	return json.Marshal(plain(s))
}
