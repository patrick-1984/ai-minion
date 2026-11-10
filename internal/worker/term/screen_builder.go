package term

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Screen builder (AD-14): turns sanitised terminal text into the one screen
// format of every surface, ops.Screen.
//
// The builder keeps a small grid of terminal cells. Text and LF write into
// it, CR and TAB (tab stops every 8 columns) only move the write position,
// SGR sequences set the style of the cells written after them, and the
// allow-listed cursor sequences move the write position. Columns are display
// cells, as in a terminal: a wide character (CJK, emoji) takes two cells and a
// combining character takes none and joins the cell before it, so cursor
// sequences address the same cells a terminal would. The widths come from a
// small built-in approximation (runeWidth). The span offsets of ops.StyleSpan
// are rune offsets into the rendered line and are computed when the grid is
// rendered. Input is plain text with LF line ends (tmux capture-pane output),
// so LF moves to column 0 of the next row.
//
// The grid is bounded so that hostile input cannot make the Worker allocate
// without limit: rows, columns and the total number of cells are capped, and
// input beyond the row cap is ignored.

const (
	maxRows  = 50000
	maxCols  = 4096
	maxCells = 4 << 20
	tabStop  = 8

	maxCombining      = 8       // combining characters kept on one cell
	maxCombiningTotal = 1 << 18 // combining characters kept in the whole grid
)

// BuildOption sets a property of the screen that the text itself does not
// carry.
type BuildOption func(*buildConfig)

type buildConfig struct {
	width, height int
	cursor        *ops.Cursor
}

// WithSize records the pane size, in columns and rows. Negative values mean
// unknown (0).
func WithSize(width, height int) BuildOption {
	return func(c *buildConfig) { c.width, c.height = max(width, 0), max(height, 0) }
}

// WithCursor records the cursor of the pane, zero-based row and column. A
// negative value means the cursor is hidden or unknown and leaves it null.
func WithCursor(row, col int) BuildOption {
	return func(c *buildConfig) {
		if row >= 0 && col >= 0 {
			c.cursor = &ops.Cursor{Row: row, Col: col}
		}
	}
}

// BuildScreen builds an ops.Screen from sanitised text: UTF-8 lines and one
// style span per run of equally styled cells. Invalid UTF-8 becomes U+FFFD
// and no escape byte survives in any line. The text is expected to come from
// Sanitize; as a second line of defence BuildScreen understands only the
// allow-listed sequences, so anything else in its input is dropped too.
func BuildScreen(text []byte, opts ...BuildOption) ops.Screen {
	scr, _ := BuildScreenChecked(text, opts...)
	return scr
}

// ErrIncomplete is the error of a read that could not give the whole visible
// screen: a row, a cell or a combining character was dropped by the bounds of
// the screen builder, or the host could not tell. A host returns it wrapped
// together with the screen it has (a partial screen is still worth reading),
// and a caller that binds a write to what it saw must treat it as "nothing to
// bind to": a digest of a screen with missing content would approve a change
// the caller could not see.
var ErrIncomplete = errors.New("term: the screen could not be read completely")

// BuildScreenChecked is BuildScreen that also says whether the screen holds
// every character of the text: false when the builder dropped content because
// of its bounds (rows, columns, cells, combining characters). A cursor
// movement that is clamped is not a loss of content.
func BuildScreenChecked(text []byte, opts ...BuildOption) (scr ops.Screen, complete bool) {
	var cfg buildConfig
	for _, o := range opts {
		o(&cfg)
	}
	b := newGridBuilder()
	clean := Sanitize(text) // idempotent on sanitised input; guarantees the invariant on any input
	consumed := b.feed(clean)
	scr = b.screen()
	scr.Width, scr.Height, scr.Cursor = cfg.width, cfg.height, cfg.cursor
	return scr, !b.lost && consumed == len(clean)
}

// styleKey is the SGR state of a cell. FG and BG use the ops color syntax.
type styleKey struct {
	fg, bg                                        string
	bold, dim, italic, underline, inverse, strike bool
}

func (k styleKey) span(line, start, end int) ops.StyleSpan {
	return ops.StyleSpan{
		Line: line, Start: start, End: end, FG: k.fg, BG: k.bg,
		Bold: k.bold, Dim: k.dim, Italic: k.italic, Underline: k.underline,
		Inverse: k.inverse, Strike: k.strike,
	}
}

// Cell kinds. A wide character is a head cell followed by a continuation cell
// that holds no text and has the style of its head.
const (
	kindNarrow uint8 = iota
	kindWide
	kindCont
)

type cell struct {
	r    rune
	st   uint16 // index into gridBuilder.styles, 0 is the default style
	kind uint8
}

type cellPos struct{ row, col int }

type gridBuilder struct {
	rows     [][]cell
	row, col int
	saved    [2]int
	cur      styleKey
	curIdx   uint16
	styles   []styleKey
	styleIdx map[styleKey]uint16
	cells    int
	done     bool // the row cap was reached, the rest of the input is ignored
	lost     bool // content was dropped by a bound: a column, a cell, a combining mark

	// combining holds the combining characters that follow the base character
	// of a cell. It is a side table because it is almost always empty.
	combining      map[cellPos][]rune
	combiningTotal int
}

func newGridBuilder() *gridBuilder {
	return &gridBuilder{
		styles:   []styleKey{{}},
		styleIdx: map[styleKey]uint16{{}: 0},
	}
}

// feed processes sanitised text and returns how much of it was consumed (less
// than all of it when the row cap ended the input).
func (b *gridBuilder) feed(text []byte) int {
	i := 0
	for i < len(text) && !b.done {
		c := text[i]
		switch {
		case c == chESC:
			i = b.escape(text, i)
		case c == '\n':
			b.newline()
			i++
		case c == '\r':
			b.col = 0
			i++
		case c == '\t':
			b.tab()
			i++
		case c < 0x20 || c == 0x7f:
			i++
		default:
			r, n := utf8.DecodeRune(text[i:])
			b.put(r)
			i += n
		}
	}
	return i
}

// escape handles the sequence that starts at text[i] (an ESC) and returns the
// index after it. Sanitised text only holds "ESC [ params final"; anything
// else is skipped as a lone ESC.
func (b *gridBuilder) escape(text []byte, i int) int {
	if i+1 >= len(text) || text[i+1] != '[' {
		return i + 1
	}
	j := i + 2
	for j < len(text) && text[j] >= 0x20 && text[j] <= 0x3f {
		j++
	}
	if j >= len(text) || text[j] < 0x40 || text[j] > 0x7e {
		return i + 1
	}
	params, final := text[i+2:j], text[j]
	list, ok := parseParams(params)
	if ok {
		if final == 'm' {
			b.sgr(list)
		} else {
			b.cursorMove(list, final)
		}
	}
	return j + 1
}

// arg returns parameter k of a cursor sequence, def when it is missing, empty
// or zero.
func arg(list [][]int, k, def int) int {
	if k >= len(list) || len(list[k]) == 0 || list[k][0] <= 0 {
		return def
	}
	return list[k][0]
}

func (b *gridBuilder) cursorMove(list [][]int, final byte) {
	switch final {
	case 'A':
		b.row = max(b.row-arg(list, 0, 1), 0)
	case 'B':
		b.row = min(b.row+arg(list, 0, 1), maxRows-1)
	case 'C':
		b.col = min(b.col+arg(list, 0, 1), maxCols-1)
	case 'D':
		b.col = max(b.col-arg(list, 0, 1), 0)
	case 'E':
		b.row, b.col = min(b.row+arg(list, 0, 1), maxRows-1), 0
	case 'F':
		b.row, b.col = max(b.row-arg(list, 0, 1), 0), 0
	case 'G':
		b.col = min(arg(list, 0, 1)-1, maxCols-1)
	case 'd':
		b.row = min(arg(list, 0, 1)-1, maxRows-1)
	case 'H', 'f':
		b.row = min(arg(list, 0, 1)-1, maxRows-1)
		b.col = min(arg(list, 1, 1)-1, maxCols-1)
	case 's':
		b.saved = [2]int{b.row, b.col}
	case 'u':
		b.row, b.col = b.saved[0], b.saved[1]
	}
}

// sgr applies the parameters of one canonical SGR sequence.
func (b *gridBuilder) sgr(list [][]int) {
	if len(list) == 0 {
		b.setStyle(styleKey{})
		return
	}
	k := b.cur
	for i := 0; i < len(list); i++ {
		c := list[i][0]
		switch {
		case c <= 0:
			k = styleKey{}
		case c == 1:
			k.bold = true
		case c == 2:
			k.dim = true
		case c == 3:
			k.italic = true
		case c == 4:
			k.underline = true
		case c == 7:
			k.inverse = true
		case c == 9:
			k.strike = true
		case c == 22:
			k.bold, k.dim = false, false
		case c == 23:
			k.italic = false
		case c == 24:
			k.underline = false
		case c == 27:
			k.inverse = false
		case c == 29:
			k.strike = false
		case c >= 30 && c <= 37:
			k.fg = fmt.Sprintf("p%d", c-30)
		case c >= 40 && c <= 47:
			k.bg = fmt.Sprintf("p%d", c-40)
		case c >= 90 && c <= 97:
			k.fg = fmt.Sprintf("p%d", c-90+8)
		case c >= 100 && c <= 107:
			k.bg = fmt.Sprintf("p%d", c-100+8)
		case c == 39:
			k.fg = ""
		case c == 49:
			k.bg = ""
		case c == 38 || c == 48:
			col, used := extColor(list, i)
			i += used - 1
			if col != "" {
				if c == 38 {
					k.fg = col
				} else {
					k.bg = col
				}
			}
		}
	}
	b.setStyle(k)
}

// extColor reads a canonical "38;5;n" or "38;2;r;g;b" starting at list[i] and
// returns the color in ops syntax and the number of entries used.
func extColor(list [][]int, i int) (string, int) {
	get := func(k int) int {
		if i+k < len(list) && len(list[i+k]) > 0 && list[i+k][0] >= 0 {
			return list[i+k][0]
		}
		return 0
	}
	switch get(1) {
	case 5:
		if n := get(2); n <= 255 {
			return fmt.Sprintf("p%d", n), 3
		}
		return "", 3
	case 2:
		r, g, bl := get(2), get(3), get(4)
		if r <= 255 && g <= 255 && bl <= 255 {
			return fmt.Sprintf("#%02x%02x%02x", r, g, bl), 5
		}
		return "", 5
	}
	return "", len(list) - i // malformed: skip the rest
}

func (b *gridBuilder) setStyle(k styleKey) {
	b.cur = k
	if idx, ok := b.styleIdx[k]; ok {
		b.curIdx = idx
		return
	}
	if len(b.styles) >= 1<<16 { // style table full: fall back to the default style
		b.curIdx = 0
		return
	}
	idx := uint16(len(b.styles))
	b.styles = append(b.styles, k)
	b.styleIdx[k] = idx
	b.curIdx = idx
}

// touch makes sure row r exists.
func (b *gridBuilder) touch(r int) {
	for len(b.rows) <= r {
		b.rows = append(b.rows, nil)
	}
}

func (b *gridBuilder) newline() {
	b.touch(b.row)
	if b.row+1 >= maxRows {
		b.done = true
		return
	}
	b.row++
	b.col = 0
}

// tab moves the cursor to the next tab stop. It does not overwrite anything:
// cells that already hold text keep it, and cells that do not exist yet up to
// the stop are filled with spaces in the current style.
func (b *gridBuilder) tab() {
	target := min(b.col+tabStop-b.col%tabStop, maxCols)
	if target > b.col {
		b.touch(b.row)
		b.grow(b.row, target)
	}
	b.col = target
}

// grow extends row r to at least n cells, filled with spaces in the current
// style. It stops at the cell budget.
func (b *gridBuilder) grow(r, n int) {
	line := b.rows[r]
	for len(line) < n {
		if b.cells >= maxCells {
			b.lost = true
			break
		}
		b.cells++
		line = append(line, cell{r: ' ', st: b.curIdx})
	}
	b.rows[r] = line
}

// clearCell turns the cell at (r, c) into a plain space and repairs its wide
// partner, so a wide character is never left half-overwritten.
func (b *gridBuilder) clearCell(r, c int) {
	line := b.rows[r]
	switch line[c].kind {
	case kindWide:
		if c+1 < len(line) && line[c+1].kind == kindCont {
			line[c+1] = cell{r: ' ', st: line[c+1].st}
		}
	case kindCont:
		if c > 0 && line[c-1].kind == kindWide {
			line[c-1] = cell{r: ' ', st: line[c-1].st}
			b.dropCombining(r, c-1)
		}
	}
	line[c] = cell{r: ' ', st: line[c].st}
	b.dropCombining(r, c)
}

func (b *gridBuilder) dropCombining(r, c int) {
	if len(b.combining) == 0 {
		return
	}
	k := cellPos{r, c}
	if rs, ok := b.combining[k]; ok {
		b.combiningTotal -= len(rs)
		delete(b.combining, k)
	}
}

// put writes one character at the cursor with the current style.
func (b *gridBuilder) put(r rune) {
	w := runeWidth(r)
	if w == 0 {
		b.putCombining(r)
		return
	}
	if b.col+w > maxCols {
		b.lost = true
		return
	}
	b.touch(b.row)
	// Gap cells before the cursor are spaces in the default style.
	if b.col > len(b.rows[b.row]) {
		saved := b.curIdx
		b.curIdx = 0
		b.grow(b.row, b.col)
		b.curIdx = saved
	}
	line := b.rows[b.row]
	if b.col+w > len(line) && b.cells+(b.col+w-len(line)) > maxCells {
		b.lost = true
		return
	}
	if b.col+w > len(line) {
		b.cells += b.col + w - len(line)
		for len(line) < b.col+w {
			line = append(line, cell{r: ' '})
		}
		b.rows[b.row] = line
	}
	for k := 0; k < w; k++ {
		b.clearCell(b.row, b.col+k)
	}
	line = b.rows[b.row]
	if w == 2 {
		line[b.col] = cell{r: r, st: b.curIdx, kind: kindWide}
		line[b.col+1] = cell{st: b.curIdx, kind: kindCont}
	} else {
		line[b.col] = cell{r: r, st: b.curIdx}
	}
	b.col += w
}

// putCombining attaches a zero-width character to the character before the
// cursor. Without one, or when the limits are reached, it is dropped.
func (b *gridBuilder) putCombining(r rune) {
	if b.col == 0 || b.row >= len(b.rows) {
		return
	}
	line := b.rows[b.row]
	c := b.col - 1
	if c >= len(line) {
		return // nothing was written there
	}
	if line[c].kind == kindCont {
		c--
	}
	k := cellPos{b.row, c}
	if len(b.combining[k]) >= maxCombining || b.combiningTotal >= maxCombiningTotal {
		b.lost = true
		return
	}
	if b.combining == nil {
		b.combining = map[cellPos][]rune{}
	}
	b.combining[k] = append(b.combining[k], r)
	b.combiningTotal++
}

// screen renders the grid as an ops.Screen. Span offsets count the runes of
// the rendered line.
func (b *gridBuilder) screen() ops.Screen {
	lines := make([]string, len(b.rows))
	spans := []ops.StyleSpan{}
	for y, line := range b.rows {
		var sb strings.Builder
		sb.Grow(len(line))
		runes, start := 0, 0
		var cur uint16
		closeRun := func() {
			if cur != 0 && runes > start {
				spans = append(spans, b.styles[cur].span(y, start, runes))
			}
		}
		for x, c := range line {
			if c.kind == kindCont {
				continue
			}
			if c.st != cur {
				closeRun()
				cur, start = c.st, runes
			}
			sb.WriteRune(c.r)
			runes++
			if len(b.combining) > 0 {
				for _, m := range b.combining[cellPos{y, x}] {
					sb.WriteRune(m)
					runes++
				}
			}
		}
		closeRun()
		lines[y] = sb.String()
	}
	return ops.Screen{Lines: lines, Spans: spans}
}

// runeWidth returns the number of terminal cells of r: 0 for combining and
// format characters, 2 for East Asian wide characters and emoji, 1 otherwise.
// It is a compact approximation of wcwidth, enough to keep cursor addressing
// aligned with a terminal for common text.
func runeWidth(r rune) int {
	switch {
	case r < 0x300:
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case r >= 0x1160 && r <= 0x11ff: // Hangul medial vowels and final consonants
		return 0
	}
	for _, rg := range wideRanges {
		if r < rg[0] {
			break
		}
		if r <= rg[1] {
			return 2
		}
	}
	return 1
}

// wideRanges lists the wide ranges in ascending order.
var wideRanges = [...][2]rune{
	{0x1100, 0x115f}, {0x2e80, 0x303e}, {0x3041, 0x33ff}, {0x3400, 0x4dbf},
	{0x4e00, 0x9fff}, {0xa000, 0xa4cf}, {0xa960, 0xa97f}, {0xac00, 0xd7a3},
	{0xf900, 0xfaff}, {0xfe10, 0xfe19}, {0xfe30, 0xfe6f}, {0xff00, 0xff60},
	{0xffe0, 0xffe6}, {0x1f300, 0x1f64f}, {0x1f680, 0x1f6ff}, {0x1f900, 0x1f9ff},
	{0x1fa70, 0x1faff}, {0x20000, 0x3fffd},
}
