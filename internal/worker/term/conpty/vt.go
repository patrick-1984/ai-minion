package conpty

import (
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// Minimal VT screen buffer. It interprets the output of a pseudo console just
// far enough to know what the screen shows: printable text with auto-wrap,
// cursor movement and save/restore, SGR colors and attributes, erase, insert
// and delete of characters and lines, scroll regions, the alternate screen
// and a bounded scrollback. Strings (OSC, DCS, APC, PM, SOS) are skipped
// without being buffered, so hostile output cannot make the Worker allocate
// without limit; the grid is fixed at the session size.
//
// The buffer is not safe for concurrent use; the session serialises access.
// Rendering goes through term.BuildScreen, which runs the sanitiser (AD-14),
// so what leaves this package is always a sanitised ops.Screen.

const (
	vtMaxScrollback = 2000
	vtMaxParams     = 32
	vtMaxParamBytes = 128
	vtMaxParamVal   = 65535
)

// vtAttr is the SGR state of a cell. fg and bg are -1 for the default color,
// 0..255 for a palette entry and vtRGB|0xRRGGBB for a truecolor value.
type vtAttr struct {
	fg, bg int32
	flags  uint8
}

// vtRGB marks a truecolor value in vtAttr.fg and vtAttr.bg.
const vtRGB int32 = 1 << 24

// Attribute flags of vtAttr.
const (
	flBold uint8 = 1 << iota
	flDim
	flItalic
	flUnderline
	flInverse
	flStrike
)

var defaultAttr = vtAttr{fg: -1, bg: -1}

const (
	wNarrow uint8 = iota
	wHead
	wCont
)

type vtCell struct {
	r    rune
	attr vtAttr
	w    uint8
}

type parserState uint8

const (
	psGround parserState = iota
	psEsc
	psEscInter
	psCSI
	psString
	psStringEsc
)

type vt struct {
	cols, rows int
	grid       [][]vtCell
	scrollback [][]vtCell
	cx, cy     int
	wrapNext   bool // the cursor is past the last column, the next glyph wraps
	attr       vtAttr
	top, bot   int // scroll region, inclusive
	autowrap   bool
	cursorOn   bool
	bracketed  bool
	alt        bool
	mainGrid   [][]vtCell // the main screen while the alternate one is shown
	saved      vtSaved    // DECSC state
	altSaved   vtSaved    // cursor saved by DECSET 1049
	reply      func([]byte)

	state  parserState
	params []byte
	inter  []byte
	utf    []byte
}

type vtSaved struct {
	cx, cy   int
	attr     vtAttr
	wrapNext bool
}

func newVT(cols, rows int, reply func([]byte)) *vt {
	v := &vt{cols: max(cols, 1), rows: max(rows, 1), reply: reply}
	v.grid = v.blankGrid()
	v.reset()
	return v
}

func (v *vt) reset() {
	v.cx, v.cy, v.wrapNext = 0, 0, false
	v.attr = defaultAttr
	v.top, v.bot = 0, v.rows-1
	v.autowrap, v.cursorOn, v.bracketed = true, true, false
	v.saved = vtSaved{attr: defaultAttr}
	v.altSaved = v.saved
	v.state, v.params, v.inter, v.utf = psGround, nil, nil, nil
}

func (v *vt) blankRow(a vtAttr) []vtCell {
	row := make([]vtCell, v.cols)
	b := vtCell{r: ' ', attr: vtAttr{fg: -1, bg: a.bg}}
	for i := range row {
		row[i] = b
	}
	return row
}

func (v *vt) blankGrid() [][]vtCell {
	g := make([][]vtCell, v.rows)
	for i := range g {
		g[i] = v.blankRow(defaultAttr)
	}
	return g
}

// feed interprets output bytes.
func (v *vt) feed(data []byte) {
	for _, b := range data {
		v.step(b)
	}
}

func (v *vt) step(b byte) {
	// CAN and SUB abort any sequence; ESC starts a new one except inside a
	// string, where ESC only matters as the start of ST.
	switch b {
	case 0x18, 0x1a:
		v.flushUTF()
		v.state = psGround
		return
	case 0x1b:
		v.flushUTF()
		if v.state == psString {
			v.state = psStringEsc
			return
		}
		v.state, v.params, v.inter = psEsc, v.params[:0], v.inter[:0]
		return
	}
	switch v.state {
	case psGround:
		v.ground(b)
	case psEsc:
		v.escByte(b)
	case psEscInter:
		if b >= 0x20 && b <= 0x2f {
			return
		}
		v.state = psGround // final byte of an ignored sequence such as ESC ( B
	case psCSI:
		v.csiByte(b)
	case psString:
		if b == 0x07 { // BEL ends an OSC
			v.state = psGround
		}
	case psStringEsc:
		if b == '\\' { // ST
			v.state = psGround
			return
		}
		// ESC followed by anything else ends the string and starts a new
		// escape sequence.
		v.state, v.params, v.inter = psEsc, v.params[:0], v.inter[:0]
		v.escByte(b)
	}
}

func (v *vt) flushUTF() {
	if len(v.utf) > 0 {
		v.utf = v.utf[:0]
		v.put(utf8.RuneError)
	}
}

func (v *vt) ground(b byte) {
	if b >= 0x80 {
		v.utf = append(v.utf, b)
		if utf8.FullRune(v.utf) {
			r, _ := utf8.DecodeRune(v.utf)
			v.utf = v.utf[:0]
			v.put(r)
		}
		return
	}
	v.flushUTF()
	switch b {
	case 0x08: // BS
		if v.cx > 0 {
			v.cx--
		}
		v.wrapNext = false
	case 0x09: // HT
		v.cx = min(v.cx+8-v.cx%8, v.cols-1)
		v.wrapNext = false
	case 0x0a, 0x0b, 0x0c: // LF, VT, FF
		v.index()
	case 0x0d: // CR
		v.cx, v.wrapNext = 0, false
	case 0x7f:
	default:
		if b >= 0x20 {
			v.put(rune(b))
		}
	}
}

func (v *vt) escByte(b byte) {
	switch {
	case b == '[':
		v.state, v.params, v.inter = psCSI, v.params[:0], v.inter[:0]
	case b == ']' || b == 'P' || b == '_' || b == '^' || b == 'X':
		v.state = psString
	case b >= 0x20 && b <= 0x2f:
		v.state = psEscInter
	default:
		v.state = psGround
		switch b {
		case '7':
			v.saved = vtSaved{v.cx, v.cy, v.attr, v.wrapNext}
		case '8':
			v.restore(v.saved)
		case 'D':
			v.index()
		case 'E':
			v.cx, v.wrapNext = 0, false
			v.index()
		case 'M':
			v.reverseIndex()
		case 'c':
			v.fullReset()
		}
	}
}

func (v *vt) fullReset() {
	if v.alt {
		v.grid, v.mainGrid, v.alt = v.mainGrid, nil, false
	}
	v.grid = v.blankGrid()
	v.scrollback = nil
	v.reset()
}

func (v *vt) restore(s vtSaved) {
	v.cx, v.cy, v.attr, v.wrapNext = min(s.cx, v.cols-1), min(s.cy, v.rows-1), s.attr, s.wrapNext
}

func (v *vt) csiByte(b byte) {
	switch {
	case b >= 0x30 && b <= 0x3f:
		if len(v.params) < vtMaxParamBytes {
			v.params = append(v.params, b)
		}
	case b >= 0x20 && b <= 0x2f:
		if len(v.inter) < 4 {
			v.inter = append(v.inter, b)
		}
	case b >= 0x40 && b <= 0x7e:
		v.state = psGround
		v.csi(b)
	default:
		v.state = psGround
	}
}

// parseParams splits a CSI parameter string. Each parameter is a list of
// colon separated sub-parameters; a missing value is -1. A private marker
// (? > < =) is returned separately.
func parseParams(p []byte) (marker byte, list [][]int) {
	if len(p) > 0 && p[0] >= '<' && p[0] <= '?' {
		marker, p = p[0], p[1:]
	}
	if len(p) == 0 {
		return marker, nil
	}
	cur, val, have := []int{}, 0, false
	flush := func() {
		if have {
			cur = append(cur, val)
		} else {
			cur = append(cur, -1)
		}
		val, have = 0, false
	}
	for _, c := range p {
		switch {
		case c >= '0' && c <= '9':
			val = min(val*10+int(c-'0'), vtMaxParamVal)
			have = true
		case c == ':':
			flush()
		case c == ';':
			flush()
			if len(list) < vtMaxParams {
				list = append(list, cur)
			}
			cur = []int{}
		}
	}
	flush()
	if len(list) < vtMaxParams {
		list = append(list, cur)
	}
	return marker, list
}

// num returns parameter k, or def when it is missing, empty or zero.
func num(list [][]int, k, def int) int {
	if k >= len(list) || len(list[k]) == 0 || list[k][0] <= 0 {
		return def
	}
	return list[k][0]
}

// numZ is like num but keeps an explicit zero (erase modes).
func numZ(list [][]int, k int) int {
	if k >= len(list) || len(list[k]) == 0 || list[k][0] < 0 {
		return 0
	}
	return list[k][0]
}

func (v *vt) csi(final byte) {
	marker, list := parseParams(v.params)
	if len(v.inter) > 0 {
		return // DECSCUSR, DECSTR, DECRQM and similar are not needed
	}
	if marker != 0 {
		switch final {
		case 'h', 'l':
			v.privateMode(list, final == 'h')
		}
		return
	}
	switch final {
	case 'A':
		v.cy = max(v.cy-num(list, 0, 1), v.regionFloor())
		v.wrapNext = false
	case 'B', 'e':
		v.cy = min(v.cy+num(list, 0, 1), v.regionCeil())
		v.wrapNext = false
	case 'C', 'a':
		v.cx = min(v.cx+num(list, 0, 1), v.cols-1)
		v.wrapNext = false
	case 'D':
		v.cx = max(v.cx-num(list, 0, 1), 0)
		v.wrapNext = false
	case 'E':
		v.cy, v.cx, v.wrapNext = min(v.cy+num(list, 0, 1), v.regionCeil()), 0, false
	case 'F':
		v.cy, v.cx, v.wrapNext = max(v.cy-num(list, 0, 1), v.regionFloor()), 0, false
	case 'G', '`':
		v.cx, v.wrapNext = min(num(list, 0, 1)-1, v.cols-1), false
	case 'd':
		v.cy, v.wrapNext = min(num(list, 0, 1)-1, v.rows-1), false
	case 'H', 'f':
		v.cy, v.cx, v.wrapNext = min(num(list, 0, 1)-1, v.rows-1), min(num(list, 1, 1)-1, v.cols-1), false
	case 'J':
		v.eraseDisplay(numZ(list, 0))
	case 'K':
		v.eraseLine(numZ(list, 0))
	case 'L':
		v.insertLines(num(list, 0, 1))
	case 'M':
		v.deleteLines(num(list, 0, 1))
	case '@':
		v.insertChars(num(list, 0, 1))
	case 'P':
		v.deleteChars(num(list, 0, 1))
	case 'X':
		v.eraseChars(num(list, 0, 1))
	case 'S':
		for i := min(num(list, 0, 1), v.rows); i > 0; i-- {
			v.scrollUp()
		}
	case 'T':
		for i := min(num(list, 0, 1), v.rows); i > 0; i-- {
			v.scrollDown()
		}
	case 'm':
		v.sgr(list)
	case 'r':
		top, bot := num(list, 0, 1)-1, num(list, 1, v.rows)-1
		if bot > v.rows-1 {
			bot = v.rows - 1
		}
		if top < bot {
			v.top, v.bot = top, bot
			v.cx, v.cy, v.wrapNext = 0, 0, false
		}
	case 's':
		v.saved = vtSaved{v.cx, v.cy, v.attr, v.wrapNext}
	case 'u':
		v.restore(v.saved)
	case 'n':
		switch numZ(list, 0) {
		case 5:
			v.send("\x1b[0n")
		case 6:
			v.send("\x1b[" + strconv.Itoa(v.cy+1) + ";" + strconv.Itoa(v.cx+1) + "R")
		}
	case 'c':
		if numZ(list, 0) == 0 {
			v.send("\x1b[?1;0c")
		}
	}
}

// regionFloor and regionCeil bound vertical cursor movement: inside the
// scroll region the cursor stays in it, outside it moves over the whole
// screen.
func (v *vt) regionFloor() int {
	if v.cy >= v.top && v.cy <= v.bot {
		return v.top
	}
	return 0
}

func (v *vt) regionCeil() int {
	if v.cy >= v.top && v.cy <= v.bot {
		return v.bot
	}
	return v.rows - 1
}

func (v *vt) send(s string) {
	if v.reply != nil {
		v.reply([]byte(s))
	}
}

func (v *vt) privateMode(list [][]int, on bool) {
	for _, p := range list {
		if len(p) == 0 {
			continue
		}
		switch p[0] {
		case 7:
			v.autowrap = on
		case 25:
			v.cursorOn = on
		case 2004:
			v.bracketed = on
		case 47, 1047, 1049:
			v.setAlt(on, p[0] == 1049)
		}
	}
}

func (v *vt) setAlt(on, saveCursor bool) {
	if on == v.alt {
		return
	}
	if on {
		if saveCursor {
			v.altSaved = vtSaved{v.cx, v.cy, v.attr, v.wrapNext}
		}
		v.mainGrid, v.grid, v.alt = v.grid, v.blankGrid(), true
		return
	}
	v.grid, v.mainGrid, v.alt = v.mainGrid, nil, false
	if saveCursor {
		v.restore(v.altSaved)
	}
}

func (v *vt) sgr(list [][]int) {
	if len(list) == 0 {
		v.attr = defaultAttr
		return
	}
	a := v.attr
	for i := 0; i < len(list); i++ {
		p := list[i]
		c := 0
		if len(p) > 0 && p[0] > 0 {
			c = p[0]
		}
		switch {
		case c == 0:
			a = defaultAttr
		case c == 1:
			a.flags |= flBold
		case c == 2:
			a.flags |= flDim
		case c == 3:
			a.flags |= flItalic
		case c == 4:
			if len(p) > 1 && p[1] == 0 { // 4:0 is "no underline"
				a.flags &^= flUnderline
			} else {
				a.flags |= flUnderline
			}
		case c == 7:
			a.flags |= flInverse
		case c == 9:
			a.flags |= flStrike
		case c == 22:
			a.flags &^= flBold | flDim
		case c == 23:
			a.flags &^= flItalic
		case c == 24:
			a.flags &^= flUnderline
		case c == 27:
			a.flags &^= flInverse
		case c == 29:
			a.flags &^= flStrike
		case c >= 30 && c <= 37:
			a.fg = int32(c - 30)
		case c >= 40 && c <= 47:
			a.bg = int32(c - 40)
		case c >= 90 && c <= 97:
			a.fg = int32(c - 90 + 8)
		case c >= 100 && c <= 107:
			a.bg = int32(c - 100 + 8)
		case c == 39:
			a.fg = -1
		case c == 49:
			a.bg = -1
		case c == 38 || c == 48:
			col, used := extendedColor(list, i)
			i += used - 1
			if col != -2 {
				if c == 38 {
					a.fg = col
				} else {
					a.bg = col
				}
			}
		}
	}
	v.attr = a
}

// extendedColor reads "38;5;n", "38;2;r;g;b" or the colon forms starting at
// list[i]. It returns the color (-2 when malformed) and the number of list
// entries used.
func extendedColor(list [][]int, i int) (int32, int) {
	if p := list[i]; len(p) > 1 { // colon form: 38:5:n or 38:2:[cs]:r:g:b
		switch p[1] {
		case 5:
			if len(p) > 2 && p[2] >= 0 && p[2] <= 255 {
				return int32(p[2]), 1
			}
		case 2:
			if len(p) >= 6 {
				return rgb(p[len(p)-3], p[len(p)-2], p[len(p)-1])
			}
		}
		return -2, 1
	}
	get := func(k int) int {
		if i+k < len(list) && len(list[i+k]) > 0 {
			return list[i+k][0]
		}
		return -1
	}
	switch get(1) {
	case 5:
		if n := get(2); n >= 0 && n <= 255 {
			return int32(n), 3
		}
		return -2, 3
	case 2:
		c, _ := rgb(get(2), get(3), get(4))
		return c, 5
	}
	return -2, len(list) - i
}

func rgb(r, g, b int) (int32, int) {
	if r < 0 || g < 0 || b < 0 || r > 255 || g > 255 || b > 255 {
		return -2, 1
	}
	return vtRGB | int32(r<<16|g<<8|b), 1
}

// --- editing ---------------------------------------------------------------

// index moves the cursor down one row, scrolling the region at its bottom.
func (v *vt) index() {
	v.wrapNext = false
	switch {
	case v.cy == v.bot:
		v.scrollUp()
	case v.cy < v.rows-1:
		v.cy++
	}
}

func (v *vt) reverseIndex() {
	v.wrapNext = false
	switch {
	case v.cy == v.top:
		v.scrollDown()
	case v.cy > 0:
		v.cy--
	}
}

// scrollUp scrolls the region up one row. On the main screen a region that
// starts at the top feeds the scrollback.
func (v *vt) scrollUp() {
	if !v.alt && v.top == 0 {
		v.pushScrollback(v.grid[0])
	}
	copy(v.grid[v.top:v.bot], v.grid[v.top+1:v.bot+1])
	v.grid[v.bot] = v.blankRow(v.attr)
}

func (v *vt) scrollDown() {
	copy(v.grid[v.top+1:v.bot+1], v.grid[v.top:v.bot])
	v.grid[v.top] = v.blankRow(v.attr)
}

func (v *vt) pushScrollback(row []vtCell) {
	n := len(row)
	for n > 0 && isBlank(row[n-1]) {
		n--
	}
	cp := make([]vtCell, n)
	copy(cp, row[:n])
	if len(v.scrollback) >= vtMaxScrollback {
		copy(v.scrollback, v.scrollback[1:])
		v.scrollback[len(v.scrollback)-1] = cp
		return
	}
	v.scrollback = append(v.scrollback, cp)
}

func isBlank(c vtCell) bool { return c.r == ' ' && c.attr == defaultAttr && c.w == wNarrow }

func (v *vt) insertLines(n int) {
	if v.cy < v.top || v.cy > v.bot {
		return
	}
	n = min(n, v.bot-v.cy+1)
	copy(v.grid[v.cy+n:v.bot+1], v.grid[v.cy:v.bot+1-n])
	for i := 0; i < n; i++ {
		v.grid[v.cy+i] = v.blankRow(v.attr)
	}
	v.cx, v.wrapNext = 0, false
}

func (v *vt) deleteLines(n int) {
	if v.cy < v.top || v.cy > v.bot {
		return
	}
	n = min(n, v.bot-v.cy+1)
	copy(v.grid[v.cy:v.bot+1-n], v.grid[v.cy+n:v.bot+1])
	for i := 0; i < n; i++ {
		v.grid[v.bot-i] = v.blankRow(v.attr)
	}
	v.cx, v.wrapNext = 0, false
}

func (v *vt) blankCell() vtCell { return vtCell{r: ' ', attr: vtAttr{fg: -1, bg: v.attr.bg}} }

// fixWide repairs a wide character that an edit cut in half at column c.
func (v *vt) fixWide(row []vtCell, c int) {
	if c < 0 || c >= len(row) {
		return
	}
	switch row[c].w {
	case wHead:
		if c+1 < len(row) {
			row[c+1] = v.blankCell()
		}
		row[c] = v.blankCell()
	case wCont:
		if c > 0 {
			row[c-1] = v.blankCell()
		}
		row[c] = v.blankCell()
	}
}

func (v *vt) insertChars(n int) {
	row := v.grid[v.cy]
	n = min(n, v.cols-v.cx)
	// Only a wide character cut by the insertion point is repaired; one that
	// starts at the cursor moves intact.
	if row[v.cx].w == wCont {
		v.fixWide(row, v.cx)
	}
	copy(row[v.cx+n:], row[v.cx:v.cols-n])
	for i := 0; i < n; i++ {
		row[v.cx+i] = v.blankCell()
	}
	// A wide character pushed over the right edge loses its second half.
	if row[v.cols-1].w == wHead {
		row[v.cols-1] = v.blankCell()
	}
	v.wrapNext = false
}

func (v *vt) deleteChars(n int) {
	row := v.grid[v.cy]
	n = min(n, v.cols-v.cx)
	// The deleted span [cx, cx+n) may cut a wide character at either edge; the
	// halves outside the span are blanked, wide characters that merely move
	// stay intact.
	if row[v.cx].w == wCont && v.cx > 0 {
		row[v.cx-1] = v.blankCell()
	}
	if v.cx+n < v.cols && row[v.cx+n].w == wCont {
		row[v.cx+n] = v.blankCell()
	}
	copy(row[v.cx:], row[v.cx+n:])
	for i := v.cols - n; i < v.cols; i++ {
		row[i] = v.blankCell()
	}
	v.wrapNext = false
}

func (v *vt) eraseChars(n int) {
	v.eraseSpan(v.cy, v.cx, min(v.cx+n, v.cols))
	v.wrapNext = false
}

// eraseSpan blanks cells [from, to) of row y, repairing wide characters at
// both edges.
func (v *vt) eraseSpan(y, from, to int) {
	row := v.grid[y]
	if from >= to {
		return
	}
	v.fixWide(row, from)
	v.fixWide(row, to-1)
	for i := from; i < to; i++ {
		row[i] = v.blankCell()
	}
}

func (v *vt) eraseLine(mode int) {
	switch mode {
	case 0:
		v.eraseSpan(v.cy, v.cx, v.cols)
	case 1:
		v.eraseSpan(v.cy, 0, v.cx+1)
	case 2:
		v.eraseSpan(v.cy, 0, v.cols)
	}
	v.wrapNext = false
}

func (v *vt) eraseDisplay(mode int) {
	switch mode {
	case 0:
		v.eraseSpan(v.cy, v.cx, v.cols)
		for y := v.cy + 1; y < v.rows; y++ {
			v.eraseSpan(y, 0, v.cols)
		}
	case 1:
		for y := 0; y < v.cy; y++ {
			v.eraseSpan(y, 0, v.cols)
		}
		v.eraseSpan(v.cy, 0, v.cx+1)
	case 2:
		for y := 0; y < v.rows; y++ {
			v.eraseSpan(y, 0, v.cols)
		}
	case 3:
		v.scrollback = nil
	}
	v.wrapNext = false
}

// put writes one character at the cursor.
func (v *vt) put(r rune) {
	if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
		return
	}
	w := runeCells(r)
	if w == 0 {
		return
	}
	if v.wrapNext || v.cx+w > v.cols {
		if !v.autowrap {
			v.cx = v.cols - w
			v.wrapNext = false
		} else {
			v.cx = 0
			v.index()
		}
	}
	row := v.grid[v.cy]
	v.fixWide(row, v.cx)
	if w == 2 {
		v.fixWide(row, v.cx+1)
		row[v.cx] = vtCell{r: r, attr: v.attr, w: wHead}
		row[v.cx+1] = vtCell{attr: v.attr, w: wCont}
	} else {
		row[v.cx] = vtCell{r: r, attr: v.attr}
	}
	v.cx += w
	if v.cx >= v.cols {
		v.cx, v.wrapNext = v.cols-1, true
	}
}

// runeCells returns the display width of r: 0 for combining and format
// characters, 2 for East Asian wide characters and emoji, 1 otherwise. It is
// the same compact approximation of wcwidth that term.BuildScreen uses, so the
// column of a cursor sequence means the same thing in both grids.
func runeCells(r rune) int {
	switch {
	case r < 0x300:
		return 1
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case r >= 0x1160 && r <= 0x11ff:
		return 0
	}
	for _, rg := range vtWide {
		if r < rg[0] {
			break
		}
		if r <= rg[1] {
			return 2
		}
	}
	return 1
}

var vtWide = [...][2]rune{
	{0x1100, 0x115f}, {0x2e80, 0x303e}, {0x3041, 0x33ff}, {0x3400, 0x4dbf},
	{0x4e00, 0x9fff}, {0xa000, 0xa4cf}, {0xa960, 0xa97f}, {0xac00, 0xd7a3},
	{0xf900, 0xfaff}, {0xfe10, 0xfe19}, {0xfe30, 0xfe6f}, {0xff00, 0xff60},
	{0xffe0, 0xffe6}, {0x1f300, 0x1f64f}, {0x1f680, 0x1f6ff}, {0x1f900, 0x1f9ff},
	{0x1fa70, 0x1faff}, {0x20000, 0x3fffd},
}

// --- rendering -------------------------------------------------------------

// renderRows appends the canonical text of rows to dst: one line per row,
// separated by LF, SGR runs in the form the sanitiser allows. Trailing blank
// cells of a row are left out.
func renderRows(dst []byte, rows [][]vtCell) []byte {
	for y, row := range rows {
		if y > 0 {
			dst = append(dst, '\n')
		}
		n := len(row)
		for n > 0 && isBlank(row[n-1]) {
			n--
		}
		cur := defaultAttr
		for _, c := range row[:n] {
			if c.w == wCont {
				continue
			}
			if c.attr != cur {
				dst = appendSGR(dst, c.attr)
				cur = c.attr
			}
			dst = utf8.AppendRune(dst, c.r)
		}
		if cur != defaultAttr {
			dst = append(dst, "\x1b[0m"...)
		}
	}
	return dst
}

// appendSGR appends one SGR sequence that sets exactly a, starting from reset.
func appendSGR(dst []byte, a vtAttr) []byte {
	dst = append(dst, "\x1b[0"...)
	for _, f := range [...]struct {
		bit  uint8
		code string
	}{{flBold, ";1"}, {flDim, ";2"}, {flItalic, ";3"}, {flUnderline, ";4"}, {flInverse, ";7"}, {flStrike, ";9"}} {
		if a.flags&f.bit != 0 {
			dst = append(dst, f.code...)
		}
	}
	dst = appendColor(dst, a.fg, 38)
	dst = appendColor(dst, a.bg, 48)
	return append(dst, 'm')
}

func appendColor(dst []byte, c int32, base int) []byte {
	switch {
	case c < 0:
		return dst
	case c&vtRGB != 0:
		dst = append(dst, ';')
		dst = strconv.AppendInt(dst, int64(base), 10)
		dst = append(dst, ";2"...)
		for _, sh := range [...]uint{16, 8, 0} {
			dst = append(dst, ';')
			dst = strconv.AppendInt(dst, int64(c>>sh&0xff), 10)
		}
		return dst
	default:
		dst = append(dst, ';')
		dst = strconv.AppendInt(dst, int64(base), 10)
		dst = append(dst, ";5;"...)
		return strconv.AppendInt(dst, int64(c), 10)
	}
}

// visibleRows returns the visible rows with trailing blank rows dropped.
func (v *vt) visibleRows() [][]vtCell {
	n := len(v.grid)
	for n > 0 && rowBlank(v.grid[n-1]) {
		n--
	}
	return v.grid[:n]
}

func rowBlank(row []vtCell) bool {
	for _, c := range row {
		if !isBlank(c) {
			return false
		}
	}
	return true
}

// snapshot renders the visible screen as a sanitised ops.Screen.
func (v *vt) snapshot() ops.Screen {
	scr, _ := v.snapshotChecked()
	return scr
}

// snapshotChecked is snapshot that also says whether the screen builder kept
// every character (see term.BuildScreenChecked).
func (v *vt) snapshotChecked() (ops.Screen, bool) {
	text := renderRows(nil, v.visibleRows())
	row, col := -1, -1
	if v.cursorOn {
		row, col = v.cy, v.cx
		if v.wrapNext {
			col = v.cols // just past the last column, as a terminal shows it
		}
	}
	scr, complete := term.BuildScreenChecked(text, term.WithSize(v.cols, v.rows), term.WithCursor(row, col))
	if scr.Lines == nil {
		scr.Lines = []string{}
	}
	return scr, complete
}

// capture renders the last n lines including the scrollback. The cursor is
// left unknown because the lines are not positioned on the screen.
func (v *vt) capture(n int) ops.Screen {
	scr, _ := v.captureChecked(n)
	return scr
}

// captureChecked is capture that also says whether the screen builder kept
// every character.
func (v *vt) captureChecked(n int) (ops.Screen, bool) {
	var rows [][]vtCell
	if !v.alt {
		rows = append(rows, v.scrollback...)
	}
	rows = append(rows, v.visibleRows()...)
	if n >= 0 && len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	scr, complete := term.BuildScreenChecked(renderRows(nil, rows), term.WithSize(v.cols, v.rows))
	if scr.Lines == nil {
		scr.Lines = []string{}
	}
	return scr, complete
}
