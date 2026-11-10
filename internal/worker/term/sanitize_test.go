package term

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// golden compares got with testdata/<name>, or rewrites it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file (run go test -update): %v", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from the golden file; run go test -update and review the diff\n--- got ---\n%s--- want ---\n%s", name, got, want)
	}
}

// overlongSGR is a legal SGR sequence whose canonical form (empty color
// components written as 0) is longer than its input and than the parser limit
// unless the sanitiser cuts it at a whole attribute.
var overlongSGR = "\x1b[" + strings.Repeat("38;2;;;;", 5) + "38;2;;;mX"

// corpus is the hostile and the ordinary input every test and the fuzz seeds
// share. Names become golden file names.
var corpus = []struct{ name, in string }{
	{"plain", "hello\nworld\n"},
	{"osc52_bel", "a\x1b]52;c;SGVsbG8=\x07b"},
	{"osc52_st", "a\x1b]52;c;SGVsbG8=\x1b\\b"},
	{"osc52_query", "a\x1b]52;c;?\x07b"},
	{"osc8_link", "see \x1b]8;;https://evil.example/x\x1b\\click\x1b]8;;\x1b\\ here"},
	{"osc8_params_bel", "\x1b]8;id=1;javascript:alert(1)\x07x\x1b]8;;\x07"},
	{"osc_title", "\x1b]0;owned\x07\x1b]2;owned\x1b\\ok"},
	{"osc_other", "\x1b]7;file:///etc\x07\x1b]133;A\x07\x1b]1337;File=inline=1:AAAA\x07z"},
	{"dcs", "a\x1bP1$r0m\x1b\\b\x1bP+q544e\x07c"},
	{"apc", "a\x1b_Gi=1,a=q;AAAA\x1b\\b"},
	{"pm_sos", "a\x1b^private\x1b\\b\x1bXsos\x07c"},
	{"nested_in_string", "a\x1b]52;c;\x1b]52;c;AAAA\x07 tail\x07b"},
	{"esc_inside_osc_restarts", "a\x1b]0;title\x1b[31mred\x1b[0mb"},
	{"unterminated_osc", "before\x1b]52;c;AAAA payload with no end\nmore text"},
	{"unterminated_dcs", "before\x1bP payload"},
	{"unterminated_csi", "before\x1b[31"},
	{"lone_esc", "before\x1b"},
	{"can_aborts_string", "a\x1b]52;c;AAAA\x18visible"},
	{"sub_aborts_string", "a\x1bP junk\x1avisible"},
	{"csi_aborted_by_control", "a\x1b[3\n1mb"},
	{"sgr_allowed", "\x1b[1mbold\x1b[2mdim\x1b[3mital\x1b[4mul\x1b[7minv\x1b[0m plain"},
	{"sgr_colors", "\x1b[31mred\x1b[92mbrightgreen\x1b[44mbgblue\x1b[105mbgbrightmagenta\x1b[39;49m"},
	{"sgr_256", "\x1b[38;5;208morange\x1b[48;5;17mnavybg\x1b[0m"},
	{"sgr_truecolor", "\x1b[38;2;255;128;0mfg\x1b[48;2;0;0;64mbg\x1b[0m"},
	{"sgr_colon_forms", "\x1b[38:5:99mx\x1b[38:2::1:2:3my\x1b[48:2:4:5:6mz\x1b[4:3mcurly\x1b[4:0m"},
	{"sgr_mixed_params", "\x1b[1;5;31;8;53mpartial\x1b[0m"},
	{"sgr_conceal_blink", "\x1b[8mhidden\x1b[28m\x1b[5mblink\x1b[25m"},
	{"sgr_invalid_color", "\x1b[38;5;999mx\x1b[38;2;1;2mx\x1b[38;9mx\x1b[1m"},
	{"sgr_underline_color", "\x1b[58;5;1mx\x1b[58;2;1;2;3;1my"},
	{"sgr_empty", "\x1b[mreset"},
	{"sgr_private", "\x1b[?25h\x1b[?1049h\x1b[>4;2m\x1b[=1c"},
	{"sgr_canonical_overlong", overlongSGR},
	{"sgr_overlong", "\x1b[" + strings.Repeat("1;", 100) + "m" + "\x1b[" + strings.Repeat("9", 200) + "mtail"},
	{"cursor_moves", "a\x1b[2Ab\x1b[3Bc\x1b[4Cd\x1b[5De\x1b[1Ef\x1b[2Fg\x1b[7Gh"},
	{"cursor_position", "\x1b[H\x1b[5;10H\x1b[;3H\x1b[4;f\x1b[9d\x1b[s\x1b[u"},
	{"cursor_huge", "\x1b[99999999;99999999Hx"},
	{"cursor_bad", "\x1b[1;2;3H\x1b[1;2A\x1b[5s\x1b[1:2H\x1b[?2A"},
	{"queries_dropped", "\x1b[6n\x1b[c\x1b[>c\x1b[5n\x1b[18t\x1b[?996n"},
	{"erase_and_scroll_dropped", "\x1b[2J\x1b[K\x1b[3J\x1b[5S\x1b[3T\x1b[1;24r\x1b[4h\x1b[?1000h"},
	{"two_byte_escapes", "\x1bc\x1b7\x1b8\x1bM\x1b=\x1b>\x1b(B\x1b)0\x1b#8x"},
	{"esc_esc", "\x1b\x1b[31mred"},
	{"esc_then_control", "a\x1b\nb"},
	{"c0_controls", "a\x00b\x01c\x07d\x08e\x0bf\x0cg\x0eh\x0fi\x7fj"},
	{"tabs_cr", "a\tb\r\nc\rd"},
	{"c1_controls", "a\u0080b\u009bc\u009dd\u009ce\u0090f\u009fg"},
	{"raw_8bit_csi", "a\x9b31mb\x9d52;c;AAAA\x07c"},
	{"invalid_utf8", "ok \xff\xfe bad \xc3 \xe2\x82 \xf0\x9f\x98 end"},
	{"truncated_utf8_at_end", "abc\xe2\x82"},
	{"bidi", "a\u202eb\u202ac\u2066d\u2069e\u202ef"},
	{"unicode_tags", "a\U000e0049\U000e0067\U000e006eb\U000e007f"},
	{"utf8_ok", "h\u00e9llo w\u00f6rld \u65e5\u672c\u8a9e \U0001f600 \u202f"},
}

// ---------- sanitiser ----------

var (
	// every ESC in sanitised output starts one of these.
	canonicalSeq = regexp.MustCompile(`^\x1b\[[0-9;]*[mABCDEFGHdfsu]`)
	sgrAllowRE   = regexp.MustCompile(`^(0|1|2|3|4|7|9|22|23|24|27|29|39|49|3[0-7]|4[0-7]|9[0-7]|10[0-7]|38;5;\d+|48;5;\d+|38;2;\d+;\d+;\d+|48;2;\d+;\d+;\d+)$`)
)

// checkSanitised asserts the AD-14 invariants of sanitised output: valid
// UTF-8, no control character except LF, CR and TAB, no C1 control, bidi
// override or tag character, and ESC only inside a canonical allow-listed SGR
// or cursor sequence.
func checkSanitised(t testing.TB, in, out []byte) {
	t.Helper()
	if !utf8.Valid(out) {
		t.Fatalf("output is not valid UTF-8: %q (input %q)", out, in)
	}
	for i := 0; i < len(out); {
		b := out[i]
		if b == chESC {
			m := canonicalSeq.Find(out[i:])
			if m == nil {
				t.Fatalf("ESC outside an allowed sequence at %d: %q (input %q)", i, out, in)
			}
			if m[len(m)-1] == 'm' {
				// Every SGR parameter group must be allow-listed. The
				// canonical form is a list of single codes and 38/48 colors.
				if !sgrParamsAllowed(string(m[2 : len(m)-1])) {
					t.Fatalf("SGR outside the allow-list %q (input %q)", m, in)
				}
			}
			i += len(m)
			continue
		}
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' || b == 0x7f {
			t.Fatalf("control byte %#x in output %q (input %q)", b, out, in)
		}
		r, n := utf8.DecodeRune(out[i:])
		if dropRune(r) {
			t.Fatalf("forbidden rune %U in output %q (input %q)", r, out, in)
		}
		i += n
	}
	if again := Sanitize(out); !bytes.Equal(again, out) {
		t.Fatalf("Sanitize is not idempotent: %q then %q (input %q)", out, again, in)
	}
	scr := BuildScreen(out)
	if err := scr.Validate(); err != nil {
		t.Fatalf("BuildScreen output is invalid: %v (input %q)", err, in)
	}
}

// sgrParamsAllowed checks a canonical parameter string against the allow-list.
func sgrParamsAllowed(params string) bool {
	parts := strings.Split(params, ";")
	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "38", "48":
			if i+2 < len(parts) && parts[i+1] == "5" {
				if !sgrAllowRE.MatchString(parts[i] + ";5;" + parts[i+2]) {
					return false
				}
				i += 2
				continue
			}
			if i+4 < len(parts) && parts[i+1] == "2" {
				if !sgrAllowRE.MatchString(strings.Join(parts[i:i+5], ";")) {
					return false
				}
				for _, v := range parts[i+2 : i+5] {
					if n, _ := strconv.Atoi(v); n > 255 {
						return false
					}
				}
				i += 4
				continue
			}
			return false
		default:
			if !sgrAllowRE.MatchString(parts[i]) {
				return false
			}
		}
	}
	return true
}

func TestSanitizeCorpusInvariants(t *testing.T) {
	for _, c := range corpus {
		t.Run(c.name, func(t *testing.T) {
			checkSanitised(t, []byte(c.in), Sanitize([]byte(c.in)))
		})
	}
}

func TestSanitizeCorpusGolden(t *testing.T) {
	for _, c := range corpus {
		t.Run(c.name, func(t *testing.T) {
			out := Sanitize([]byte(c.in))
			scr := BuildScreen(out)
			js, err := json.MarshalIndent(scr, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got := "in:  " + strconv.Quote(c.in) + "\nout: " + strconv.Quote(string(out)) + "\nscreen:\n" + string(js) + "\n"
			golden(t, c.name+".golden", got)
		})
	}
}

func TestSanitizeDropsStringSequences(t *testing.T) {
	tests := map[string]string{
		"osc52 bel":        "a\x1b]52;c;SGVsbG8=\x07b",
		"osc52 st":         "a\x1b]52;c;SGVsbG8=\x1b\\b",
		"osc52 query":      "a\x1b]52;c;?\x07b",
		"osc8 st":          "a\x1b]8;;https://x.example\x1b\\b",
		"osc8 bel":         "a\x1b]8;id=1;https://x.example\x07b",
		"other osc":        "a\x1b]0;title\x07b",
		"osc 1337":         "a\x1b]1337;File=name=eA==;inline=1:AAAA\x07b",
		"dcs st":           "a\x1bP1$qm\x1b\\b",
		"dcs bel":          "a\x1bPpayload\x07b",
		"apc st":           "a\x1b_Gi=1;AAAA\x1b\\b",
		"apc bel":          "a\x1b_payload\x07b",
		"pm":               "a\x1b^payload\x1b\\b",
		"sos":              "a\x1bXpayload\x1b\\b",
		"esc esc st":       "a\x1b]52;c;AAAA\x1b\x1b\\b",
		"can abort":        "a\x1b]52;c;AAAA\x18b",
		"payload with esc": "a\x1b]52;c;AA\x1b\x1b\x1b\\b",
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			got := string(Sanitize([]byte(in)))
			if got != "ab" {
				t.Errorf("Sanitize(%q) = %q, want %q", in, got, "ab")
			}
		})
	}
}

func TestSanitizeNestedAndUnterminated(t *testing.T) {
	tests := []struct{ name, in, want string }{
		// the first BEL ends the string, the rest is plain text
		{"nested osc", "a\x1b]52;c;\x1b]52;c;AAAA\x07 tail\x07b", "a tailb"},
		// ESC inside a string that is not ST starts a new sequence
		{"esc restarts", "a\x1b]0;title\x1b[31mred\x1b[0mb", "a\x1b[31mred\x1b[0mb"},
		{"unterminated osc swallows the rest", "a\x1b]52;c;AAAA\nmore", "a"},
		{"unterminated dcs", "a\x1bP payload", "a"},
		{"unterminated csi", "a\x1b[31", "a"},
		{"lone esc", "a\x1b", "a"},
		{"csi aborted by LF", "a\x1b[3\n1mb", "a\n1mb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Sanitize([]byte(tc.in))); got != tc.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeSGRAllowList(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"reset", "\x1b[0m", "\x1b[0m"},
		{"empty is reset", "\x1b[m", "\x1b[0m"},
		{"empty param is reset", "\x1b[;1m", "\x1b[0;1m"},
		{"bold dim italic underline inverse", "\x1b[1;2;3;4;7m", "\x1b[1;2;3;4;7m"},
		{"strike", "\x1b[9m\x1b[29m", "\x1b[9m\x1b[29m"},
		{"off codes", "\x1b[22;23;24;27m", "\x1b[22;23;24;27m"},
		{"16 colors", "\x1b[30;47;91;106m", "\x1b[30;47;91;106m"},
		{"default colors", "\x1b[39;49m", "\x1b[39;49m"},
		{"256", "\x1b[38;5;208;48;5;0m", "\x1b[38;5;208;48;5;0m"},
		{"truecolor", "\x1b[38;2;1;2;3;48;2;255;254;253m", "\x1b[38;2;1;2;3;48;2;255;254;253m"},
		{"colon 256", "\x1b[38:5:9m", "\x1b[38;5;9m"},
		{"colon truecolor with colorspace", "\x1b[38:2::1:2:3m", "\x1b[38;2;1;2;3m"},
		{"colon truecolor", "\x1b[48:2:1:2:3m", "\x1b[48;2;1;2;3m"},
		{"curly underline", "\x1b[4:3m", "\x1b[4m"},
		{"underline off sub", "\x1b[4:0m", "\x1b[24m"},
		{"blink dropped", "\x1b[5m\x1b[6m\x1b[25m", ""},
		{"conceal dropped", "\x1b[8m", ""},
		{"reveal dropped", "\x1b[28m", ""},
		{"overline dropped", "\x1b[53m", ""},
		{"font dropped", "\x1b[11m", ""},
		{"double underline dropped", "\x1b[21m", ""},
		{"partial keep", "\x1b[1;5;31;8m", "\x1b[1;31m"},
		{"underline color dropped", "\x1b[58;5;1;1m", "\x1b[1m"},
		{"underline color rgb dropped", "\x1b[58;2;1;2;3;3m", "\x1b[3m"},
		{"256 out of range", "\x1b[1;38;5;256;3m", "\x1b[1m"},
		{"truecolor out of range", "\x1b[38;2;1;2;300m", ""},
		{"truncated color", "\x1b[1;38;2;1;2m", "\x1b[1m"},
		{"unknown color mode", "\x1b[38;7;1m", ""},
		{"private marker", "\x1b[?1m", ""},
		{"intermediate", "\x1b[1 m", ""},
		{"saturating value", "\x1b[99999999999999999999m", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Sanitize([]byte(tc.in))); got != tc.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeCanonicalSGRStaysWithinLimits(t *testing.T) {
	tests := []struct {
		name, in string
		want     string // exact output, empty to check only the invariants
	}{
		{"six truecolor groups", overlongSGR, "\x1b[" + strings.TrimSuffix(strings.Repeat("38;2;0;0;0;", 5), ";") + "mX"},
		{"empty params then color", "\x1b[" + strings.Repeat(";", 28) + "38:2:1:2:3mX", ""},
		{"many resets", "\x1b[" + strings.Repeat("0;", 31) + "0mX", "\x1b[" + strings.Repeat("0;", 31) + "0mX"},
		{"color at both limits", "\x1b[" + strings.Repeat(";", 27) + "38;2;1;2;3mX", "\x1b[" + strings.Repeat("0;", 27) + "38;2;1;2;3mX"},
		{"color one entry over the limit", "\x1b[" + strings.Repeat(";", 28) + "38;2;1;2;3mX", "\x1b[" + strings.TrimSuffix(strings.Repeat("0;", 28), ";") + "mX"},
		{"colors within the limits", "\x1b[" + strings.Repeat("48;2;255;255;255;", 3) + "48;5;1mX", "\x1b[" + strings.Repeat("48;2;255;255;255;", 3) + "48;5;1mX"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := Sanitize([]byte(tc.in))
			if tc.want != "" && string(out) != tc.want {
				t.Fatalf("Sanitize = %q, want %q", out, tc.want)
			}
			if again := Sanitize(out); !bytes.Equal(again, out) {
				t.Fatalf("not idempotent: %q then %q", out, again)
			}
			checkSanitised(t, []byte(tc.in), out)
			if m := canonicalSeq.Find(out); m == nil || len(m)-3 > maxCSIBytes {
				t.Fatalf("canonical sequence missing or over %d parameter bytes: %q", maxCSIBytes, out)
			}
		})
	}

	// The screen keeps the style: sanitising twice must not lose it.
	scr := BuildScreen([]byte(overlongSGR))
	want := []ops.StyleSpan{{Line: 0, Start: 0, End: 1, FG: "#000000"}}
	if len(scr.Spans) != 1 || scr.Spans[0] != want[0] {
		t.Fatalf("Spans = %+v, want %+v", scr.Spans, want)
	}
	scr = BuildScreen(Sanitize([]byte(overlongSGR)))
	if len(scr.Spans) != 1 || scr.Spans[0] != want[0] {
		t.Fatalf("Spans after Sanitize = %+v, want %+v", scr.Spans, want)
	}
}

func TestSanitizeCursorAllowList(t *testing.T) {
	pass := []string{
		"\x1b[A", "\x1b[3A", "\x1b[3B", "\x1b[3C", "\x1b[3D", "\x1b[3E", "\x1b[3F", "\x1b[3G",
		"\x1b[H", "\x1b[5;10H", "\x1b[;3H", "\x1b[4;f", "\x1b[9d", "\x1b[s", "\x1b[u",
	}
	for _, s := range pass {
		if got := string(Sanitize([]byte(s))); got != s {
			t.Errorf("Sanitize(%q) = %q, want it unchanged", s, got)
		}
	}
	drop := []string{
		"\x1b[1;2;3H", "\x1b[1;2A", "\x1b[5s", "\x1b[1:2H", "\x1b[?2A", "\x1b[1 A",
		"\x1b[6n", "\x1b[c", "\x1b[>c", "\x1b[18t", // queries
		"\x1b[2J", "\x1b[K", "\x1b[S", "\x1b[T", "\x1b[1;24r", "\x1b[4h", "\x1b[?1049h", // not cursor movement
		"\x1b7", "\x1b8", "\x1bM", // two-byte cursor save and restore are not in the allow-list
	}
	for _, s := range drop {
		if got := Sanitize([]byte(s)); len(got) != 0 {
			t.Errorf("Sanitize(%q) = %q, want it dropped", s, got)
		}
	}
	if got := string(Sanitize([]byte("\x1b[99999999;99999999H"))); got != "\x1b[9999;9999H" {
		t.Errorf("huge cursor position = %q, want it clamped", got)
	}
}

func TestSanitizeControlsAndText(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"keeps LF CR TAB", "a\tb\r\nc", "a\tb\r\nc"},
		{"drops C0", "a\x00b\x01c\x07d\x08e\x0bf\x0cg\x0eh\x0fi\x7fj", "abcdefghij"},
		{"drops C1 as UTF-8", "a\u0080b\u009bc\u009dd\u009ce", "abcde"},
		{"8-bit C1 bytes are invalid UTF-8", "a\x9bb", "a�b"},
		{"invalid UTF-8", "\xff\xfe", "��"},
		{"truncated rune before ASCII", "\xe2\x82A", "\uFFFD\uFFFDA"},
		{"truncated rune at end", "a\xe2\x82", "a�"},
		{"bidi dropped", "a\u202eb\u2066c\u2069d", "abcd"},
		{"tags dropped", "a\U000e0049b", "ab"},
		{"valid text kept", "h\u00e9llo \u65e5\u672c \U0001f600", "h\u00e9llo \u65e5\u672c \U0001f600"},
		{"existing U+FFFD kept", "a�b", "a�b"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Sanitize([]byte(tc.in))); got != tc.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeString(t *testing.T) {
	if got := SanitizeString("a\x1b]52;c;AA\x07b"); got != "ab" {
		t.Errorf("SanitizeString = %q", got)
	}
}

// chunked runs the stream sanitiser over the given chunks.
func chunked(chunks ...[]byte) []byte {
	var s Sanitizer
	var out []byte
	for _, c := range chunks {
		out = append(out, s.Push(c)...)
	}
	return append(out, s.Flush()...)
}

func TestSanitizeSplitAcrossReads(t *testing.T) {
	for _, c := range corpus {
		t.Run(c.name, func(t *testing.T) {
			in := []byte(c.in)
			want := Sanitize(in)
			for i := 0; i <= len(in); i++ {
				if got := chunked(in[:i], in[i:]); !bytes.Equal(got, want) {
					t.Fatalf("split at %d: got %q, want %q", i, got, want)
				}
			}
			var single [][]byte
			for i := range in {
				single = append(single, in[i:i+1])
			}
			if got := chunked(single...); !bytes.Equal(got, want) {
				t.Fatalf("byte by byte: got %q, want %q", got, want)
			}
		})
	}
}

func TestSanitizerSplitSequenceNeverLeaks(t *testing.T) {
	var s Sanitizer
	in := "x\x1b]52;c;SGVs"
	if got := s.Push([]byte(in)); string(got) != "x" {
		t.Fatalf("first chunk = %q, want %q", got, "x")
	}
	if got := s.Push([]byte("bG8=\x07y")); string(got) != "y" {
		t.Fatalf("second chunk = %q, want %q", got, "y")
	}
	// A split UTF-8 character is held back and completed.
	if got := s.Push([]byte("\xe2\x82")); len(got) != 0 {
		t.Fatalf("partial rune emitted %q", got)
	}
	if got := s.Push([]byte("\xac!")); string(got) != "€!" {
		t.Fatalf("completed rune = %q", got)
	}
}

func TestSanitizerResetEndsUnterminatedString(t *testing.T) {
	var s Sanitizer
	if got := s.Push([]byte("a\x1b]52;c;AAAA")); string(got) != "a" {
		t.Fatalf("got %q", got)
	}
	if got := s.Push([]byte("still swallowed")); len(got) != 0 {
		t.Fatalf("string was not swallowed: %q", got)
	}
	s.Reset()
	if got := s.Push([]byte("visible")); string(got) != "visible" {
		t.Fatalf("after Reset: %q", got)
	}
}

func TestSanitizerFlushDropsOpenSequence(t *testing.T) {
	var s Sanitizer
	s.Push([]byte("a\x1b]52;c;AAAA"))
	if got := s.Flush(); len(got) != 0 {
		t.Fatalf("Flush emitted %q", got)
	}
	if got := s.Push([]byte("b")); string(got) != "b" {
		t.Fatalf("after Flush: %q", got)
	}
}

func TestSanitizeLargeInputs(t *testing.T) {
	// An unterminated long payload is dropped in full, in bounded time.
	big := "a\x1b]52;c;" + strings.Repeat("A", 4<<20)
	if got := string(Sanitize([]byte(big))); got != "a" {
		t.Fatalf("long OSC left %d bytes", len(got))
	}
	// Overlong CSI parameters are dropped.
	if got := Sanitize([]byte("\x1b[" + strings.Repeat("1;", 5000) + "mx")); string(got) != "x" {
		t.Fatalf("overlong SGR left %q", got)
	}
}

func FuzzSanitize(f *testing.F) {
	for _, c := range corpus {
		f.Add([]byte(c.in))
	}
	f.Add([]byte("\x1b[38;5;1;48;2;1;2;3m\x1b]8;;u\x1b\\\x1bP\x1b_\x1b^\x1bX"))
	f.Add([]byte(overlongSGR))
	f.Add([]byte("\x1b[" + strings.Repeat(";", 28) + "38:2:1:2:3mX"))
	f.Fuzz(func(t *testing.T, in []byte) {
		out := Sanitize(in)
		checkSanitised(t, in, out)
		// The stream form agrees with the one-shot form at every split.
		for _, i := range []int{0, len(in) / 3, len(in) / 2, len(in)} {
			if got := chunked(in[:i], in[i:]); !bytes.Equal(got, out) {
				t.Fatalf("split at %d: %q != %q", i, got, out)
			}
		}
	})
}

// ---------- screen builder ----------

func TestBuildScreenPlainText(t *testing.T) {
	scr := BuildScreen([]byte("hello\nw\u00f6rld\n"))
	if got, want := scr.Lines, []string{"hello", "w\u00f6rld"}; !equalStrings(got, want) {
		t.Fatalf("Lines = %q, want %q", got, want)
	}
	if len(scr.Spans) != 0 {
		t.Errorf("Spans = %v, want none", scr.Spans)
	}
	if scr.Cursor != nil || scr.Width != 0 || scr.Height != 0 {
		t.Errorf("size or cursor set without options: %+v", scr)
	}
	if err := scr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildScreenEmpty(t *testing.T) {
	scr := BuildScreen(nil)
	if scr.Lines == nil || scr.Spans == nil || len(scr.Lines) != 0 {
		t.Fatalf("empty screen = %+v, want non-nil empty slices", scr)
	}
	b, err := json.Marshal(scr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"lines":[]`) || !strings.Contains(string(b), `"spans":[]`) {
		t.Errorf("json = %s", b)
	}
}

func TestBuildScreenLineEnds(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"a\nb", []string{"a", "b"}},
		{"a\nb\n", []string{"a", "b"}},
		{"a\n\nb", []string{"a", "", "b"}},
		{"a\n\n", []string{"a", ""}},
		{"\n\n\n", []string{"", "", ""}},
		{"a\r\nb\r\n", []string{"a", "b"}},
		{"abc\rxy", []string{"xyc"}},
		{"a\tb", []string{"a       b"}},
		{"abcdefgh\tx", []string{"abcdefgh        x"}},
	}
	for _, tc := range tests {
		if got := BuildScreen([]byte(tc.in)).Lines; !equalStrings(got, tc.want) {
			t.Errorf("BuildScreen(%q).Lines = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildScreenSpans(t *testing.T) {
	in := "plain \x1b[1;31mred bold\x1b[0m \x1b[4munder\x1b[24m\n\x1b[38;5;208;48;2;1;2;3mzz\x1b[0m"
	scr := BuildScreen([]byte(in))
	if err := scr.Validate(); err != nil {
		t.Fatal(err)
	}
	wantLines := []string{"plain red bold under", "zz"}
	if !equalStrings(scr.Lines, wantLines) {
		t.Fatalf("Lines = %q, want %q", scr.Lines, wantLines)
	}
	want := []ops.StyleSpan{
		{Line: 0, Start: 6, End: 14, FG: "p1", Bold: true},
		{Line: 0, Start: 15, End: 20, Underline: true},
		{Line: 1, Start: 0, End: 2, FG: "p208", BG: "#010203"},
	}
	if len(scr.Spans) != len(want) {
		t.Fatalf("Spans = %+v, want %+v", scr.Spans, want)
	}
	for i := range want {
		if scr.Spans[i] != want[i] {
			t.Errorf("Spans[%d] = %+v, want %+v", i, scr.Spans[i], want[i])
		}
	}
}

func TestBuildScreenStyleAttributes(t *testing.T) {
	scr := BuildScreen([]byte("\x1b[1ma\x1b[2mb\x1b[3mc\x1b[4md\x1b[7me\x1b[9mf\x1b[22;23;24;27;29mg"))
	want := []ops.StyleSpan{
		{Line: 0, Start: 0, End: 1, Bold: true},
		{Line: 0, Start: 1, End: 2, Bold: true, Dim: true},
		{Line: 0, Start: 2, End: 3, Bold: true, Dim: true, Italic: true},
		{Line: 0, Start: 3, End: 4, Bold: true, Dim: true, Italic: true, Underline: true},
		{Line: 0, Start: 4, End: 5, Bold: true, Dim: true, Italic: true, Underline: true, Inverse: true},
		{Line: 0, Start: 5, End: 6, Bold: true, Dim: true, Italic: true, Underline: true, Inverse: true, Strike: true},
	}
	if len(scr.Spans) != len(want) {
		t.Fatalf("Spans = %+v", scr.Spans)
	}
	for i := range want {
		if scr.Spans[i] != want[i] {
			t.Errorf("Spans[%d] = %+v, want %+v", i, scr.Spans[i], want[i])
		}
	}
	if scr.Lines[0] != "abcdefg" {
		t.Errorf("line = %q", scr.Lines[0])
	}
}

func TestBuildScreenColors(t *testing.T) {
	tests := []struct {
		in       string
		fg, bg   string
		describe string
	}{
		{"\x1b[30mx", "p0", "", "standard fg"},
		{"\x1b[37mx", "p7", "", "standard fg 7"},
		{"\x1b[90mx", "p8", "", "bright fg"},
		{"\x1b[97mx", "p15", "", "bright fg 15"},
		{"\x1b[41mx", "", "p1", "standard bg"},
		{"\x1b[107mx", "", "p15", "bright bg"},
		{"\x1b[38;5;255mx", "p255", "", "256 fg"},
		{"\x1b[48;5;16mx", "", "p16", "256 bg"},
		{"\x1b[38;2;171;205;239mx", "#abcdef", "", "truecolor fg"},
		{"\x1b[48;2;0;0;0mx", "", "#000000", "truecolor bg"},
		{"\x1b[31;42mx", "p1", "p2", "both"},
		{"\x1b[31;39mx", "", "", "default fg resets"},
	}
	for _, tc := range tests {
		scr := BuildScreen([]byte(tc.in))
		if err := scr.Validate(); err != nil {
			t.Fatalf("%s: %v", tc.describe, err)
		}
		if tc.fg == "" && tc.bg == "" {
			if len(scr.Spans) != 0 {
				t.Errorf("%s: spans %+v, want none", tc.describe, scr.Spans)
			}
			continue
		}
		if len(scr.Spans) != 1 || scr.Spans[0].FG != tc.fg || scr.Spans[0].BG != tc.bg {
			t.Errorf("%s: spans %+v, want fg %q bg %q", tc.describe, scr.Spans, tc.fg, tc.bg)
		}
	}
}

func TestBuildScreenStyleCarriesAcrossLines(t *testing.T) {
	scr := BuildScreen([]byte("\x1b[1mone\ntwo\x1b[0m\nthree"))
	want := []ops.StyleSpan{
		{Line: 0, Start: 0, End: 3, Bold: true},
		{Line: 1, Start: 0, End: 3, Bold: true},
	}
	if len(scr.Spans) != 2 || scr.Spans[0] != want[0] || scr.Spans[1] != want[1] {
		t.Fatalf("Spans = %+v, want %+v", scr.Spans, want)
	}
}

func TestBuildScreenSpanOffsetsCountRunes(t *testing.T) {
	scr := BuildScreen([]byte("\u65e5\u672c\x1b[1m\u8a9e\U0001f600\x1b[0mx"))
	if len(scr.Spans) != 1 || scr.Spans[0].Start != 2 || scr.Spans[0].End != 4 {
		t.Fatalf("Spans = %+v, want one span over runes [2,4)", scr.Spans)
	}
	if err := scr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildScreenInvalidUTF8(t *testing.T) {
	scr := BuildScreen([]byte("a\xffb\xc3\x28c\xe2\x82"))
	if want := []string{"a�b�(c�"}; !equalStrings(scr.Lines, want) {
		t.Fatalf("Lines = %q, want %q", scr.Lines, want)
	}
	if err := scr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildScreenNoRawEscape(t *testing.T) {
	for _, c := range corpus {
		scr := BuildScreen([]byte(c.in)) // also unsanitised input
		for i, l := range scr.Lines {
			if strings.ContainsAny(l, "\x1b\x07\x00") || !utf8.ValidString(l) {
				t.Errorf("%s: line %d has raw control bytes: %q", c.name, i, l)
			}
		}
		if err := scr.Validate(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestBuildScreenDropsHostileSequences(t *testing.T) {
	scr := BuildScreen([]byte("a\x1b]52;c;AAAA\x07\x1b]8;;http://x\x1b\\b\x1bP1\x1b\\\x1b_x\x1b\\c"))
	if want := []string{"abc"}; !equalStrings(scr.Lines, want) {
		t.Fatalf("Lines = %q, want %q", scr.Lines, want)
	}
}

func TestBuildScreenCursorMovement(t *testing.T) {
	tests := []struct {
		name, in string
		want     []string
	}{
		{"absolute position", "\x1b[2;4Hx", []string{"", "   x"}},
		{"home then overwrite", "abcdef\x1b[Hxy", []string{"xycdef"}},
		{"column", "abc\x1b[5Gx", []string{"abc x"}},
		{"right and left", "a\x1b[3Cb\x1b[2Dc", []string{"a  cb"}},
		{"up and down", "ab\ncd\x1b[1A\x1b[1Dx\x1b[2By", []string{"ax", "cd", "  y"}},
		{"next and previous line", "ab\x1b[1Ecd\x1b[2Fx", []string{"xb", "cd"}},
		{"row", "a\x1b[3dx", []string{"a", "", " x"}},
		{"save and restore", "ab\x1b[scd\x1b[uXY", []string{"abXY"}},
		{"zero is one", "abc\x1b[0Dx\x1b[0;0Hy", []string{"ybx"}},
		{"clamped at origin", "\x1b[99A\x1b[99Dx", []string{"x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scr := BuildScreen([]byte(tc.in))
			if !equalStrings(scr.Lines, tc.want) {
				t.Errorf("Lines = %q, want %q", scr.Lines, tc.want)
			}
			if err := scr.Validate(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestBuildScreenCursorUsesDisplayColumns(t *testing.T) {
	tests := []struct {
		name, in string
		want     []string
	}{
		{"wide char takes two columns", "\u65e5\x1b[3GX", []string{"\u65e5X"}},
		{"cursor into the wide char replaces it", "\u65e5\x1b[2GX", []string{" X"}},
		{"narrow write over a wide head", "\u65e5\x1b[1GX", []string{"X "}},
		{"wide write over two narrow cells", "ab\x1b[1G\u65e5c", []string{"\u65e5c"}},
		{"wide write splits another wide char", "\u65e5\u672c\x1b[2G\u8a9e", []string{" \u8a9e "}},
		{"combining joins the previous cell", "e\u0301\x1b[2GX", []string{"e\u0301X"}},
		{"combining after a wide char", "\u65e5\u0301\x1b[3GX", []string{"\u65e5\u0301X"}},
		{"combining without a base is dropped", "\u0301a", []string{"a"}},
		{"overwriting the base drops its combining marks", "e\u0301\x1b[1GX", []string{"X"}},
		{"emoji is wide", "\U0001f600\x1b[3GX", []string{"\U0001f600X"}},
		{"column after wide char and CUB", "\u65e5\u672c\x1b[2DX", []string{"\u65e5X "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scr := BuildScreen([]byte(tc.in))
			if !equalStrings(scr.Lines, tc.want) {
				t.Errorf("Lines = %q, want %q", scr.Lines, tc.want)
			}
			if err := scr.Validate(); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestBuildScreenSpansWithWideAndCombining(t *testing.T) {
	scr := BuildScreen([]byte("\x1b[1m\u65e5e\u0301\x1b[0mx"))
	if want := []string{"\u65e5e\u0301x"}; !equalStrings(scr.Lines, want) {
		t.Fatalf("Lines = %q, want %q", scr.Lines, want)
	}
	// Rune offsets: wide char 0, e 1, combining mark 2.
	if len(scr.Spans) != 1 || scr.Spans[0].Start != 0 || scr.Spans[0].End != 3 || !scr.Spans[0].Bold {
		t.Fatalf("Spans = %+v, want one bold span over runes [0,3)", scr.Spans)
	}
	if err := scr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildScreenTabsMoveWithoutOverwriting(t *testing.T) {
	tests := []struct {
		name, in string
		want     []string
	}{
		{"tab after carriage return keeps text", "ABC\r\tX", []string{"ABC     X"}},
		{"tab after cursor move keeps text", "ABCDEFGHIJ\x1b[1G\tX", []string{"ABCDEFGHXJ"}},
		{"tab fills missing cells", "a\tb", []string{"a       b"}},
		{"trailing tab fills to the stop", "a\t", []string{"a       "}},
		{"tab on a stop moves a full stop", "12345678\tX", []string{"12345678        X"}},
		{"tab after a wide char counts two columns", "\u65e5\tX", []string{"\u65e5      X"}},
		{"tab over a wide char keeps it", "\u65e5\u672c\r\tX", []string{"\u65e5\u672c    X"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scr := BuildScreen([]byte(tc.in))
			if !equalStrings(scr.Lines, tc.want) {
				t.Errorf("Lines = %q, want %q", scr.Lines, tc.want)
			}
			if err := scr.Validate(); err != nil {
				t.Error(err)
			}
		})
	}

	// Styles of the text a tab passes over are kept as well.
	scr := BuildScreen([]byte("\x1b[1mAB\x1b[0m\r\tX"))
	if len(scr.Spans) != 1 || scr.Spans[0].Start != 0 || scr.Spans[0].End != 2 || !scr.Spans[0].Bold {
		t.Fatalf("Spans = %+v, want bold over [0,2)", scr.Spans)
	}
}

func TestBuildScreenOverwriteKeepsStyleOfNewCell(t *testing.T) {
	scr := BuildScreen([]byte("\x1b[1mabc\x1b[0m\x1b[1;2Hx"))
	if scr.Lines[0] != "axc" {
		t.Fatalf("line = %q", scr.Lines[0])
	}
	want := []ops.StyleSpan{
		{Line: 0, Start: 0, End: 1, Bold: true},
		{Line: 0, Start: 2, End: 3, Bold: true},
	}
	if len(scr.Spans) != 2 || scr.Spans[0] != want[0] || scr.Spans[1] != want[1] {
		t.Fatalf("Spans = %+v, want %+v", scr.Spans, want)
	}
}

func TestBuildScreenOptions(t *testing.T) {
	scr := BuildScreen([]byte("x"), WithSize(80, 24), WithCursor(3, 4))
	if scr.Width != 80 || scr.Height != 24 {
		t.Errorf("size = %dx%d", scr.Width, scr.Height)
	}
	if scr.Cursor == nil || *scr.Cursor != (ops.Cursor{Row: 3, Col: 4}) {
		t.Errorf("cursor = %+v", scr.Cursor)
	}
	scr = BuildScreen([]byte("x"), WithSize(-1, -5), WithCursor(-1, 0))
	if scr.Width != 0 || scr.Height != 0 || scr.Cursor != nil {
		t.Errorf("negative options not ignored: %+v", scr)
	}
}

func TestBuildScreenIsBounded(t *testing.T) {
	// Far cursor jumps do not allocate the distance in between.
	scr := BuildScreen([]byte("\x1b[9999;9999Hx"))
	if err := scr.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(scr.Lines) != 9999 || len([]rune(scr.Lines[9998])) != maxCols {
		t.Fatalf("lines = %d, last width = %d", len(scr.Lines), len([]rune(scr.Lines[len(scr.Lines)-1])))
	}

	// Many sparse writes stay within the cell budget.
	var in strings.Builder
	for i := 1; i <= 9999; i++ {
		in.WriteString("\x1b[" + strconv.Itoa(i) + ";9999Hx")
	}
	scr = BuildScreen([]byte(in.String()))
	cells := 0
	for _, l := range scr.Lines {
		cells += len([]rune(l))
	}
	if cells > maxCells {
		t.Fatalf("%d cells exceed the budget %d", cells, maxCells)
	}
	if err := scr.Validate(); err != nil {
		t.Fatal(err)
	}

	// The row cap truncates instead of growing without limit.
	scr = BuildScreen([]byte(strings.Repeat("\n", maxRows+100) + "tail"))
	if len(scr.Lines) > maxRows {
		t.Fatalf("%d lines exceed the cap", len(scr.Lines))
	}
}

func TestBuildScreenStyleTableOverflow(t *testing.T) {
	var in strings.Builder
	for i := 0; i < 70000; i++ {
		in.WriteString("\x1b[38;2;" + strconv.Itoa(i>>16&255) + ";" + strconv.Itoa(i>>8&255) + ";" + strconv.Itoa(i&255) + "mx")
	}
	scr := BuildScreen([]byte(in.String()))
	if err := scr.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildScreenJSONShape(t *testing.T) {
	scr := BuildScreen([]byte("\x1b[1mhi\x1b[0m"), WithSize(10, 2), WithCursor(0, 2))
	b, err := json.Marshal(scr)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"width":10,"height":2,"lines":["hi"],"spans":[{"line":0,"start":0,"end":2,"bold":true}],"cursor":{"row":0,"col":2}}`
	if string(b) != want {
		t.Errorf("json = %s\nwant   %s", b, want)
	}
}

func FuzzBuildScreen(f *testing.F) {
	for _, c := range corpus {
		f.Add([]byte(c.in))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		scr := BuildScreen(in)
		if err := scr.Validate(); err != nil {
			t.Fatalf("invalid screen for %q: %v", in, err)
		}
		for _, l := range scr.Lines {
			if strings.ContainsRune(l, 0x1b) {
				t.Fatalf("raw ESC in line %q (input %q)", l, in)
			}
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
