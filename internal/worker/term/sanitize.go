package term

import (
	"strconv"
	"unicode/utf8"
)

// Output sanitiser (AD-14, NFR3).
//
// The Worker sanitises terminal output once, here, before any data leaves it.
// The sanitiser is a byte-level state machine in the style of a terminal
// parser, so it agrees with real terminals about where a sequence ends:
//
//   - OSC (ESC ]), DCS (ESC P), SOS (ESC X), PM (ESC ^) and APC (ESC _)
//     strings are dropped entirely, payload included. They end at BEL or at
//     ST (ESC \); CAN and SUB abort them. An unterminated string swallows the
//     rest of the input, exactly as a terminal would. This covers OSC 52
//     (clipboard) and OSC 8 (hyperlinks).
//   - A CSI sequence passes only if it is an allow-listed SGR or cursor
//     sequence. It is not copied: it is parsed and written again in a
//     canonical form (ESC [ digits and semicolons, one final byte), so a
//     crafted parameter string cannot reach the consumer.
//   - Every other escape sequence (charset selection, RIS, DECSC and so on),
//     every C0 control except LF, CR and TAB, DEL and every C1 control is
//     dropped.
//   - Text is forced to valid UTF-8: an invalid byte becomes U+FFFD. Bidi
//     override and isolate controls (U+202A to U+202E, U+2066 to U+2069) and
//     the invisible Unicode tag characters (U+E0000 to U+E007F) are dropped,
//     because they can hide or reorder text for a human reader and a model.
//
// Allowed SGR: reset (0), bold (1), dim (2), italic (3), underline (4,
// including the 4:n sub-parameter form), inverse (7), strike (9), their
// "off" codes (22, 23, 24, 27, 29), the 8 and 16 color foregrounds and
// backgrounds (30 to 37, 40 to 47, 90 to 97, 100 to 107), default colors (39,
// 49) and the 256-color and truecolor forms of 38 and 48 (semicolon or colon
// separated). Blink, conceal, fonts, overline and underline color are dropped
// parameter by parameter, so "1;5m" still bolds. Conceal is dropped on
// purpose: hidden text is an injection channel against models.
//
// Allowed cursor sequences: CUU, CUD, CUF, CUB (A to D), CNL, CPL (E, F), CHA
// (G), CUP (H, f), VPA (d) and the parameterless save and restore (s, u).
// Queries such as DSR (CSI 6 n) are dropped: their answer would be typed into
// the pane.

const (
	chESC = 0x1b
	chBEL = 0x07

	maxCSIBytes  = 64    // longer parameter strings are dropped
	maxSGRParams = 32    // parameters of one SGR sequence, extra ones are dropped
	maxParamVal  = 99999 // parameter values saturate here
	maxCursorVal = 9999  // largest cursor movement or position that is passed on
)

type sstate uint8

const (
	sGround    sstate = iota
	sEsc              // after ESC
	sEscInter         // ESC plus intermediate bytes, waiting for the final byte
	sCSI              // inside CSI parameters
	sString           // inside OSC, DCS, SOS, PM or APC
	sStringEsc        // ESC seen inside a string
)

// Sanitizer filters a stream of terminal output and keeps its state between
// calls, so a sequence or a UTF-8 character split across two reads is handled
// like an unsplit one. The zero value is ready to use. A Sanitizer is not safe
// for concurrent use. Use Sanitize for a complete, self-contained capture.
type Sanitizer struct {
	st     sstate
	csi    []byte // parameter and intermediate bytes of the open CSI
	csiBad bool   // the open CSI is too long and will be dropped
	pend   [utf8.UTFMax]byte
	npend  int // bytes of an incomplete UTF-8 character at the end of the last chunk
}

// Sanitize returns the sanitised form of one complete capture of terminal
// output. A sequence left open at the end is dropped and an incomplete UTF-8
// character becomes U+FFFD.
func Sanitize(raw []byte) []byte {
	var s Sanitizer
	out := s.Push(raw)
	return append(out, s.Flush()...)
}

// SanitizeString is Sanitize for text held in a string.
func SanitizeString(raw string) string {
	return string(Sanitize([]byte(raw)))
}

// Reset forgets any open sequence and pending bytes. Use it when the stream
// restarts, for example after a pane was cleared, so that an unterminated
// string does not swallow the new output.
func (s *Sanitizer) Reset() {
	s.st = sGround
	s.csi = s.csi[:0]
	s.csiBad = false
	s.npend = 0
}

// Flush ends the stream: a pending incomplete UTF-8 character becomes U+FFFD,
// an open sequence is dropped, and the state is reset.
func (s *Sanitizer) Flush() []byte {
	var out []byte
	if s.npend > 0 && s.st == sGround {
		out = append(out, "�"...)
	}
	s.Reset()
	return out
}

// Push filters the next chunk and returns the sanitised bytes it completes.
func (s *Sanitizer) Push(chunk []byte) []byte {
	out := make([]byte, 0, len(chunk)+8)
	if s.npend > 0 {
		joined := make([]byte, 0, s.npend+len(chunk))
		joined = append(joined, s.pend[:s.npend]...)
		chunk = append(joined, chunk...)
		s.npend = 0
	}
	for i := 0; i < len(chunk); {
		b := chunk[i]
		switch s.st {
		case sGround:
			switch {
			case b == chESC:
				s.st = sEsc
				i++
			case b == '\n' || b == '\r' || b == '\t':
				out = append(out, b)
				i++
			case b < 0x20 || b == 0x7f:
				i++ // other C0 controls and DEL
			case b < 0x80:
				out = append(out, b)
				i++
			default:
				rest := chunk[i:]
				if !utf8.FullRune(rest) {
					s.npend = copy(s.pend[:], rest)
					return out
				}
				r, n := utf8.DecodeRune(rest)
				out = appendSafeRune(out, r, n)
				i += n
			}
		case sEsc:
			switch {
			case b == '[':
				s.st = sCSI
				s.csi = s.csi[:0]
				s.csiBad = false
				i++
			case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
				s.st = sString
				i++
			case b >= 0x20 && b <= 0x2f:
				s.st = sEscInter
				i++
			case b >= 0x30 && b <= 0x7e:
				s.st = sGround // two-byte escape: dropped
				i++
			case b == chESC:
				i++ // ESC ESC: the first one is void, stay
			default:
				s.st = sGround // control byte aborts the escape; reprocess it
			}
		case sEscInter:
			switch {
			case b >= 0x20 && b <= 0x2f:
				i++
			case b >= 0x30 && b <= 0x7e:
				s.st = sGround
				i++
			case b == chESC:
				s.st = sEsc
				i++
			default:
				s.st = sGround
			}
		case sCSI:
			switch {
			case b >= 0x20 && b <= 0x3f:
				if len(s.csi) < maxCSIBytes {
					s.csi = append(s.csi, b)
				} else {
					s.csiBad = true
				}
				i++
			case b >= 0x40 && b <= 0x7e:
				if !s.csiBad {
					out = appendCSI(out, s.csi, b)
				}
				s.st = sGround
				i++
			case b == chESC:
				s.st = sEsc
				i++
			default:
				s.st = sGround // control or non-ASCII byte aborts the CSI; reprocess it
			}
		case sString:
			switch b {
			case chBEL, 0x18, 0x1a: // BEL terminates, CAN and SUB abort
				s.st = sGround
			case chESC:
				s.st = sStringEsc
			}
			i++
		case sStringEsc:
			switch b {
			case '\\': // ST
				s.st = sGround
				i++
			case chESC:
				i++
			default:
				s.st = sEsc // the string was aborted, b starts a new sequence
			}
		}
	}
	return out
}

// appendSafeRune appends r, which was decoded with size n, unless it is a
// character the sanitiser removes.
func appendSafeRune(out []byte, r rune, n int) []byte {
	if r == utf8.RuneError && n == 1 {
		return append(out, "�"...)
	}
	if dropRune(r) {
		return out
	}
	return utf8.AppendRune(out, r)
}

// dropRune reports a non-ASCII character that is removed: C1 controls, bidi
// overrides and isolates, and the Unicode tag block.
func dropRune(r rune) bool {
	switch {
	case r >= 0x80 && r <= 0x9f:
		return true
	case r >= 0x202a && r <= 0x202e:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	case r >= 0xe0000 && r <= 0xe007f:
		return true
	}
	return false
}

// appendCSI writes the canonical form of one CSI sequence when it is
// allow-listed, and nothing otherwise. params holds the bytes between "ESC ["
// and the final byte.
func appendCSI(out, params []byte, final byte) []byte {
	if final == 'm' {
		return appendSGR(out, params)
	}
	if !isCursorFinal(final) {
		return out
	}
	list, ok := parseParams(params)
	if !ok {
		return out
	}
	maxParams := 1
	switch final {
	case 'H', 'f':
		maxParams = 2
	case 's', 'u':
		maxParams = 0
	}
	if len(list) > maxParams {
		return out
	}
	for _, p := range list {
		if len(p) != 1 { // colon sub-parameters are not valid here
			return out
		}
	}
	out = append(out, chESC, '[')
	for i, p := range list {
		if i > 0 {
			out = append(out, ';')
		}
		if p[0] >= 0 {
			out = strconv.AppendInt(out, int64(min(p[0], maxCursorVal)), 10)
		}
	}
	return append(out, final)
}

func isCursorFinal(b byte) bool {
	switch b {
	case 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'f', 'd', 's', 'u':
		return true
	}
	return false
}

// parseParams splits a CSI parameter string made only of digits, ';' and ':'
// into parameters, each a list of colon-separated values. An empty value is
// -1. Values saturate at maxParamVal. ok is false for any other byte (a
// private marker such as '?' or an intermediate byte).
func parseParams(b []byte) (list [][]int, ok bool) {
	if len(b) == 0 {
		return nil, true
	}
	cur := []int{-1}
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			v := cur[len(cur)-1]
			if v < 0 {
				v = 0
			}
			v = min(v*10+int(c-'0'), maxParamVal)
			cur[len(cur)-1] = v
		case c == ';':
			list = append(list, cur)
			cur = []int{-1}
		case c == ':':
			cur = append(cur, -1)
		default:
			return nil, false
		}
	}
	return append(list, cur), true
}

// appendSGR writes the allow-listed parameters of one SGR sequence as a
// canonical sequence, or nothing when no parameter survives.
func appendSGR(out, params []byte) []byte {
	list, ok := parseParams(params)
	if !ok {
		return out
	}
	if len(list) == 0 {
		return append(out, chESC, '[', '0', 'm')
	}
	if len(list) > maxSGRParams {
		list = list[:maxSGRParams]
	}
	// The canonical parameter string must itself pass the parser limits, so
	// that sanitising it again is a byte-for-byte no-op: at most maxCSIBytes
	// bytes and at most maxSGRParams entries. Empty parameters are written as
	// "0", which can make the output longer than the input. The output is cut
	// only between whole attributes, never inside a 38/48 color.
	var keep []byte
	entries := 0
	add := func(vals ...int) bool {
		var buf [40]byte
		g := buf[:0]
		for i, v := range vals {
			if i > 0 {
				g = append(g, ';')
			}
			g = strconv.AppendInt(g, int64(v), 10)
		}
		sep := 0
		if len(keep) > 0 {
			sep = 1
		}
		if entries+len(vals) > maxSGRParams || len(keep)+sep+len(g) > maxCSIBytes {
			return false
		}
		if sep == 1 {
			keep = append(keep, ';')
		}
		keep = append(keep, g...)
		entries += len(vals)
		return true
	}
	for i := 0; i < len(list); i++ {
		p := list[i]
		code := p[0]
		if code < 0 {
			code = 0
		}
		switch {
		case code == 38 || code == 48 || code == 58:
			colors, used, valid := parseExtColor(list, i)
			if !valid {
				// Unknown mode or out-of-range value: the length of the
				// rest is unknowable, so the remaining parameters go.
				i = len(list)
				continue
			}
			i += used - 1
			if code != 58 { // underline color is dropped
				if !add(append([]int{code}, colors...)...) {
					i = len(list)
				}
			}
		case code == 4 && len(p) > 1:
			v := 0
			switch {
			case len(p) == 2 && p[1] == 0:
				v = 24
			case len(p) == 2 && p[1] >= 1 && p[1] <= 5:
				v = 4
			}
			if v != 0 && !add(v) {
				i = len(list)
			}
		case len(p) > 1:
			// Other sub-parameter forms are not in the allow-list.
		case allowedSGR(code):
			if !add(code) {
				i = len(list)
			}
		}
	}
	if len(keep) == 0 {
		return out
	}
	out = append(out, chESC, '[')
	out = append(out, keep...)
	return append(out, 'm')
}

func allowedSGR(c int) bool {
	switch {
	case c == 0, c == 1, c == 2, c == 3, c == 4, c == 7, c == 9:
		return true
	case c == 22, c == 23, c == 24, c == 27, c == 29, c == 39, c == 49:
		return true
	case c >= 30 && c <= 37, c >= 40 && c <= 47:
		return true
	case c >= 90 && c <= 97, c >= 100 && c <= 107:
		return true
	}
	return false
}

// parseExtColor reads the argument of an extended color (38, 48 or 58) that
// starts at list[i]. It returns the arguments to write ("5;n" or "2;r;g;b"
// as numbers), how many list entries the color used, and whether it is valid.
func parseExtColor(list [][]int, i int) (args []int, used int, ok bool) {
	p := list[i]
	if len(p) > 1 { // colon form: 38:5:n, 38:2:r:g:b or 38:2:cs:r:g:b
		sub := p[1:]
		switch {
		case len(sub) == 2 && sub[0] == 5:
			if !byteVal(sub[1]) {
				return nil, 0, false
			}
			return []int{5, sub[1]}, 1, true
		case sub[0] == 2 && (len(sub) == 4 || len(sub) == 5):
			rgb := sub[len(sub)-3:]
			if !byteVal(rgb[0]) || !byteVal(rgb[1]) || !byteVal(rgb[2]) {
				return nil, 0, false
			}
			return []int{2, rgb[0], rgb[1], rgb[2]}, 1, true
		}
		return nil, 0, false
	}
	// Semicolon form: the following parameters are the arguments.
	next := func(k int) (int, bool) {
		if i+k >= len(list) || len(list[i+k]) != 1 {
			return 0, false
		}
		v := list[i+k][0]
		if v < 0 {
			v = 0
		}
		return v, true
	}
	mode, has := next(1)
	if !has {
		return nil, 0, false
	}
	switch mode {
	case 5:
		n, has := next(2)
		if !has || !byteVal(n) {
			return nil, 0, false
		}
		return []int{5, n}, 3, true
	case 2:
		r, h1 := next(2)
		g, h2 := next(3)
		b, h3 := next(4)
		if !h1 || !h2 || !h3 || !byteVal(r) || !byteVal(g) || !byteVal(b) {
			return nil, 0, false
		}
		return []int{2, r, g, b}, 5, true
	}
	return nil, 0, false
}

func byteVal(v int) bool { return v >= 0 && v <= 255 }
