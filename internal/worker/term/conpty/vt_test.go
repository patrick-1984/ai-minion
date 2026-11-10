package conpty

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func newTestVT(cols, rows int) (*vt, *[]string) {
	var replies []string
	return newVT(cols, rows, func(b []byte) { replies = append(replies, string(b)) }), &replies
}

func lines(v *vt) []string { return v.snapshot().Lines }

func wantLines(t *testing.T, v *vt, want ...string) {
	t.Helper()
	got := lines(v)
	if strings.Join(got, "\n") != strings.Join(want, "\n") || len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestVTTextAndNewlines(t *testing.T) {
	v, _ := newTestVT(20, 5)
	v.feed([]byte("hello\r\nworld"))
	wantLines(t, v, "hello", "world")
	// LF alone moves down without returning to column 0, as a terminal does.
	v2, _ := newTestVT(20, 5)
	v2.feed([]byte("ab\ncd"))
	wantLines(t, v2, "ab", "  cd")
}

func TestVTAutoWrapAndScroll(t *testing.T) {
	v, _ := newTestVT(5, 3)
	v.feed([]byte("abcdefghij"))
	wantLines(t, v, "abcde", "fghij")
	v.feed([]byte("klmno"))
	wantLines(t, v, "abcde", "fghij", "klmno")
	v.feed([]byte("p")) // scrolls: the first row goes to the scrollback
	wantLines(t, v, "fghij", "klmno", "p")
	if len(v.scrollback) != 1 {
		t.Fatalf("scrollback = %d", len(v.scrollback))
	}
	got := v.capture(10).Lines
	if len(got) != 4 || got[0] != "abcde" {
		t.Fatalf("capture = %q", got)
	}
}

func TestVTNoAutoWrap(t *testing.T) {
	v, _ := newTestVT(5, 3)
	v.feed([]byte("\x1b[?7labcdefg"))
	wantLines(t, v, "abcdg")
}

func TestVTCursorMovement(t *testing.T) {
	v, _ := newTestVT(20, 6)
	v.feed([]byte("\x1b[3;5Hx\x1b[1Ay\x1b[2Dz\x1b[B\x1b[10Gq"))
	wantLines(t, v, "", "    zy", "    x    q")
	// Out-of-range positions are clamped.
	v.feed([]byte("\x1b[99;99HZ"))
	got := lines(v)
	if len(got) != 6 || !strings.HasSuffix(got[5], "Z") || utf8.RuneCountInString(got[5]) != 20 {
		t.Fatalf("clamped write: %q", got)
	}
}

func TestVTSaveRestoreCursor(t *testing.T) {
	v, _ := newTestVT(20, 4)
	v.feed([]byte("ab\x1b7\x1b[3;1Hxx\x1b8cd"))
	wantLines(t, v, "abcd", "", "xx")
	v.feed([]byte("\x1b[s\x1b[H1\x1b[u2"))
	if got := lines(v)[0]; got != "1bcd2" {
		t.Fatalf("CSI s/u: %q", got)
	}
}

func TestVTErase(t *testing.T) {
	v, _ := newTestVT(10, 3)
	v.feed([]byte("aaaaaaaaaa\r\nbbbbbbbbbb\r\ncccccccccc\x1b[2;5H"))
	v.feed([]byte("\x1b[K"))
	wantLines(t, v, "aaaaaaaaaa", "bbbb", "cccccccccc")
	v.feed([]byte("\x1b[1K")) // erase to the cursor (inclusive)
	wantLines(t, v, "aaaaaaaaaa", "", "cccccccccc")
	v.feed([]byte("\x1b[0J"))
	wantLines(t, v, "aaaaaaaaaa")
	v.feed([]byte("\x1b[1;4H\x1b[1J"))
	wantLines(t, v, "    aaaaaa")
	v.feed([]byte("\x1b[2J"))
	wantLines(t, v)
	v.feed([]byte("\x1b[Habcdef\x1b[1;2H\x1b[3X"))
	wantLines(t, v, "a   ef")
}

func TestVTInsertDelete(t *testing.T) {
	v, _ := newTestVT(10, 4)
	v.feed([]byte("abcdef\x1b[1;3H\x1b[2@"))
	wantLines(t, v, "ab  cdef")
	v.feed([]byte("\x1b[1;3H\x1b[3P"))
	wantLines(t, v, "abdef")

	v2, _ := newTestVT(5, 4)
	v2.feed([]byte("1\r\n2\r\n3\r\n4\x1b[2;1H\x1b[L"))
	wantLines(t, v2, "1", "", "2", "3")
	v2.feed([]byte("\x1b[2;1H\x1b[2M"))
	wantLines(t, v2, "1", "3")
}

func TestVTScrollRegion(t *testing.T) {
	v, _ := newTestVT(5, 5)
	v.feed([]byte("a\r\nb\r\nc\r\nd\r\ne"))
	v.feed([]byte("\x1b[2;4r\x1b[4;1H\n"))
	// The region scrolls, the first and last rows stay, nothing enters the
	// scrollback because the region does not start at the top.
	wantLines(t, v, "a", "c", "d", "", "e")
	if len(v.scrollback) != 0 {
		t.Fatalf("scrollback = %d", len(v.scrollback))
	}
	v.feed([]byte("\x1b[2;1H\x1bM")) // reverse index at the top of the region
	wantLines(t, v, "a", "", "c", "d", "e")
	v.feed([]byte("\x1b[S"))
	wantLines(t, v, "a", "c", "d", "", "e")
	v.feed([]byte("\x1b[T"))
	wantLines(t, v, "a", "", "c", "d", "e")
	v.feed([]byte("\x1b[r")) // reset
	if v.top != 0 || v.bot != 4 {
		t.Fatalf("region = %d..%d", v.top, v.bot)
	}
}

func TestVTAlternateScreen(t *testing.T) {
	v, _ := newTestVT(10, 3)
	v.feed([]byte("main\x1b[?1049h"))
	wantLines(t, v)
	v.feed([]byte("\x1b[Halt"))
	wantLines(t, v, "alt")
	if got := v.capture(10).Lines; len(got) != 1 || got[0] != "alt" {
		t.Fatalf("capture on alt screen = %q", got)
	}
	v.feed([]byte("\x1b[?1049l"))
	wantLines(t, v, "main")
	// The cursor is back where it was before the switch.
	v.feed([]byte("!"))
	wantLines(t, v, "main!")
}

func TestVTSGR(t *testing.T) {
	v, _ := newTestVT(30, 2)
	v.feed([]byte("\x1b[1;3;4;7;9;32;44mA\x1b[22;23;24;27;29;39;49mB\x1b[38;5;200mC\x1b[48;2;1;2;3mD\x1b[38:2::10:20:30mE\x1b[0mF\x1b[91;102mG"))
	scr := v.snapshot()
	if scr.Lines[0] != "ABCDEFG" {
		t.Fatalf("lines = %q", scr.Lines)
	}
	want := []ops.StyleSpan{
		{Line: 0, Start: 0, End: 1, FG: "p2", BG: "p4", Bold: true, Italic: true, Underline: true, Inverse: true, Strike: true},
		{Line: 0, Start: 2, End: 3, FG: "p200"},
		{Line: 0, Start: 3, End: 4, FG: "p200", BG: "#010203"},
		{Line: 0, Start: 4, End: 5, FG: "#0a141e", BG: "#010203"},
		{Line: 0, Start: 6, End: 7, FG: "p9", BG: "p10"},
	}
	if len(scr.Spans) != len(want) {
		t.Fatalf("spans = %+v", scr.Spans)
	}
	for i, w := range want {
		if scr.Spans[i] != w {
			t.Errorf("span %d = %+v, want %+v", i, scr.Spans[i], w)
		}
	}
}

func TestVTMalformedSGRIsIgnored(t *testing.T) {
	v, _ := newTestVT(10, 1)
	v.feed([]byte("\x1b[38;5;999mA\x1b[38;2;1;2mB\x1b[38mC\x1b[48;2;300;0;0mD"))
	if got := v.snapshot(); got.Lines[0] != "ABCD" {
		t.Fatalf("lines = %q", got.Lines)
	}
}

func TestVTBackgroundColorErase(t *testing.T) {
	v, _ := newTestVT(6, 2)
	v.feed([]byte("\x1b[44m\x1b[2J\x1b[Hx"))
	scr := v.snapshot()
	// The erased cells carry the background, so the line is full width.
	if scr.Lines[0] != "x     " {
		t.Fatalf("line = %q", scr.Lines[0])
	}
}

func TestVTStringsAreSkipped(t *testing.T) {
	v, _ := newTestVT(40, 2)
	v.feed([]byte("a\x1b]0;title\x07b\x1b]8;;http://x\x1b\\c\x1bPdcs\x1b\\d\x1b_apc\x1b\\e\x1b^pm\x1b\\f\x1bXsos\x1b\\g"))
	wantLines(t, v, "abcdefg")
	// An unterminated string swallows the rest, like a terminal.
	v.feed([]byte("\x1b]0;never ends hello"))
	wantLines(t, v, "abcdefg")
	// CAN aborts it.
	v.feed([]byte("\x18Z"))
	wantLines(t, v, "abcdefgZ")
}

func TestVTSequenceSplitAcrossWrites(t *testing.T) {
	v, _ := newTestVT(20, 2)
	for _, chunk := range []string{"a\x1b", "[1;3", "1m", "b\x1b]0;ti", "tle\x07", "c"} {
		v.feed([]byte(chunk))
	}
	scr := v.snapshot()
	if scr.Lines[0] != "abc" || len(scr.Spans) != 1 || scr.Spans[0].Start != 1 || scr.Spans[0].End != 3 {
		t.Fatalf("lines %q spans %+v", scr.Lines, scr.Spans)
	}
}

func TestVTUTF8(t *testing.T) {
	v, _ := newTestVT(20, 2)
	smile := []byte("\u00e9\xe4\xb8\xad\U0001F600")
	// Feed byte by byte: multi-byte characters split across reads.
	for _, b := range smile {
		v.feed([]byte{b})
	}
	v.feed([]byte("\xff!"))
	if got := lines(v)[0]; got != "\u00e9\u4e2d\U0001F600\ufffd!" {
		t.Fatalf("line = %q", got)
	}
	// Wide characters take two columns.
	v.feed([]byte("\x1b[2;1H\u4e2d\x1b[2;3Hx"))
	if got := lines(v)[1]; got != "\u4e2dx" {
		t.Fatalf("wide: %q", got)
	}
	// Overwriting half of a wide character blanks the other half.
	v.feed([]byte("\x1b[2;2Hy"))
	if got := lines(v)[1]; got != " yx" {
		t.Fatalf("overwrite: %q", got)
	}
}

func TestVTCombiningCharactersAreDropped(t *testing.T) {
	v, _ := newTestVT(10, 1)
	v.feed([]byte("éx"))
	if got := lines(v)[0]; got != "ex" {
		t.Fatalf("line = %q", got)
	}
}

func TestVTControlCharacters(t *testing.T) {
	v, _ := newTestVT(30, 2)
	v.feed([]byte("abc\bX\tY\x07\x00\x7f\xc2\x85Z"))
	if got := lines(v)[0]; got != "abX     YZ" {
		t.Fatalf("line = %q", got)
	}
}

func TestVTQueriesAreAnswered(t *testing.T) {
	v, replies := newTestVT(20, 5)
	v.feed([]byte("\x1b[3;7H\x1b[6n\x1b[5n\x1b[c\x1b[0c\x1b[1c\x1b[>c\x1b[?6n"))
	want := []string{"\x1b[3;7R", "\x1b[0n", "\x1b[?1;0c", "\x1b[?1;0c"}
	if strings.Join(*replies, "|") != strings.Join(want, "|") {
		t.Fatalf("replies = %q, want %q", *replies, want)
	}
	// Queries never reach the screen.
	wantLines(t, v)
}

func TestVTCursorVisibility(t *testing.T) {
	v, _ := newTestVT(10, 2)
	v.feed([]byte("ab"))
	if c := v.snapshot().Cursor; c == nil || c.Row != 0 || c.Col != 2 {
		t.Fatalf("cursor = %+v", c)
	}
	v.feed([]byte("\x1b[?25l"))
	if c := v.snapshot().Cursor; c != nil {
		t.Fatalf("hidden cursor reported: %+v", c)
	}
}

func TestVTFullReset(t *testing.T) {
	v, _ := newTestVT(10, 3)
	v.feed([]byte("\x1b[31mred\r\n\r\n\r\n\r\nmore\x1b[?1049h\x1bc"))
	wantLines(t, v)
	if len(v.scrollback) != 0 || v.alt {
		t.Fatalf("reset left state: scrollback %d alt %v", len(v.scrollback), v.alt)
	}
	v.feed([]byte("x"))
	if s := v.snapshot(); len(s.Spans) != 0 {
		t.Fatalf("attributes survived reset: %+v", s.Spans)
	}
}

func TestVTScrollbackIsBounded(t *testing.T) {
	v, _ := newTestVT(10, 2)
	for i := 0; i < vtMaxScrollback+500; i++ {
		v.feed([]byte(fmt.Sprintf("%d\r\n", i)))
	}
	if len(v.scrollback) != vtMaxScrollback {
		t.Fatalf("scrollback = %d, want %d", len(v.scrollback), vtMaxScrollback)
	}
	first := v.capture(100000).Lines[0]
	if first != "499" {
		t.Fatalf("oldest kept line = %q", first)
	}
	v.feed([]byte("\x1b[3J"))
	if len(v.scrollback) != 0 {
		t.Fatal("ED 3 did not clear the scrollback")
	}
}

// Whatever the program prints, no snapshot line holds a control character and
// no style span is out of range: the screen leaves through the sanitiser.
func TestVTHostileOutputYieldsCleanScreen(t *testing.T) {
	v, _ := newTestVT(40, 10)
	hostile := "\x1b]52;c;ZXZpbA==\x07\x1b[8mhidden\x1b[0m\x1b[31;5;1m\x1b(0\x9b31m\u202eevil\u202c\x1bP1$r\x1b\\\x1b[?1049;1000hX\x00\x01\x02" +
		"\x1b[99999999999999999999;1H\x1b[;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;;m"
	v.feed([]byte(hostile))
	scr := v.snapshot()
	for _, l := range scr.Lines {
		for _, r := range l {
			if r < 0x20 || (r >= 0x7f && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) {
				t.Fatalf("control rune %U in %q", r, l)
			}
		}
	}
}

func FuzzVTFeed(f *testing.F) {
	f.Add([]byte("\x1b[1;31mhi\x1b[2J\x1b[3;4H\x1b]0;t\x07\xe4\xb8\xad"))
	f.Add([]byte("\x1b[?1049h\x1b[2;3r\x1b[L\x1b[M\x1b[5@\x1b[5P\x1b[9X"))
	f.Fuzz(func(t *testing.T, data []byte) {
		v, _ := newTestVT(17, 6)
		v.feed(data)
		v.feed(data)
		scr := v.snapshot()
		v.capture(5)
		if v.cx < 0 || v.cx >= v.cols || v.cy < 0 || v.cy >= v.rows {
			t.Fatalf("cursor out of range: %d,%d", v.cx, v.cy)
		}
		for _, l := range scr.Lines {
			if !utf8.ValidString(l) || strings.ContainsRune(l, 0x1b) {
				t.Fatalf("bad line %q", l)
			}
		}
	})
}

// Inserting or deleting characters repairs only wide characters that the edit
// really cuts; a wide character that merely moves stays intact.
func TestVTInsertDeleteKeepIntactWideCharacters(t *testing.T) {
	cases := []struct {
		name, in, want string
		cols           int
	}{
		{"insert before a wide head", "a\u4e2db\x1b[1;2H\x1b[@", "a \u4e2db", 6},
		{"delete before a wide head", "a\u4e2db\x1b[1;1H\x1b[P", "\u4e2db", 6},
		{"insert before a wide emoji", "a\U0001F600b\x1b[1;2H\x1b[2@", "a  \U0001F600b", 8},
		{"delete before a wide emoji", "ab\U0001F600c\x1b[1;1H\x1b[2P", "\U0001F600c", 8},
		{"insert inside a wide character", "a\u4e2db\x1b[1;3H\x1b[@", "a   b", 6},
		{"delete inside a wide character", "a\u4e2db\x1b[1;3H\x1b[P", "a b", 6},
		{"delete that ends inside a wide character", "a\u4e2db\x1b[1;1H\x1b[2P", " b", 6},
		{"delete that starts on a continuation", "a\u4e2db\x1b[1;3H\x1b[2P", "a", 6},
		{"insert pushes a wide head off the edge", "ab\u4e2d\x1b[1;1H\x1b[@", " ab", 4},
	}
	for _, c := range cases {
		v, _ := newTestVT(c.cols, 1)
		v.feed([]byte(c.in))
		if got := strings.TrimRight(lines(v)[0], " "); got != c.want {
			t.Errorf("%s: line = %q, want %q", c.name, got, c.want)
		}
	}
}

// A pseudo console wider than the screen builder's bound is reported incomplete
// (term.ErrIncomplete at the host), never silently shortened.
func TestVTReportsAnIncompleteScreen(t *testing.T) {
	v, _ := newTestVT(5000, 3)
	v.feed([]byte(strings.Repeat("x", 4999) + "Y"))
	if scr, complete := v.snapshotChecked(); complete || len(scr.Lines) == 0 {
		t.Fatalf("a 5000 column row: complete = %v, %d rows", complete, len(scr.Lines))
	}
	if _, complete := v.captureChecked(10); complete {
		t.Fatal("the capture of a 5000 column row was complete")
	}
	w, _ := newTestVT(80, 3)
	w.feed([]byte("hello"))
	if _, complete := w.snapshotChecked(); !complete {
		t.Fatal("an ordinary screen was incomplete")
	}
}
