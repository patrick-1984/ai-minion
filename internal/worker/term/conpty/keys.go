package conpty

import (
	"fmt"
	"strconv"
	"unicode"
	"unicode/utf8"
)

// Key encoding: send_keys names keys the way tmux does (Enter, C-c, M-x, F5).
// encodeKey turns one name into the bytes a program reads from the input of a
// pseudo console: xterm style VT input, which ConPTY translates to console
// key events.

const (
	modShift = 1 << iota
	modMeta
	modCtrl
)

type keyKind uint8

const (
	kLiteral keyKind = iota // fixed bytes
	kCSILetter
	kCSITilde
	kSS3
	kChar // a character, as if typed
)

type keyDef struct {
	kind keyKind
	seq  string // literal bytes, final letter, tilde number or character
}

var keyTable = map[string]keyDef{
	"Enter": {kLiteral, "\r"}, "KPEnter": {kLiteral, "\r"},
	"Escape": {kLiteral, "\x1b"},
	"Tab":    {kLiteral, "\t"},
	"BTab":   {kLiteral, "\x1b[Z"}, // back-tab, the same bytes as S-Tab
	"BSpace": {kLiteral, "\x7f"},
	"Space":  {kChar, " "},
	"Up":     {kCSILetter, "A"}, "Down": {kCSILetter, "B"},
	"Right": {kCSILetter, "C"}, "Left": {kCSILetter, "D"},
	"Home": {kCSILetter, "H"}, "End": {kCSILetter, "F"},
	"IC": {kCSITilde, "2"}, "DC": {kCSITilde, "3"},
	"PageUp": {kCSITilde, "5"}, "PPage": {kCSITilde, "5"}, "PgUp": {kCSITilde, "5"},
	"PageDown": {kCSITilde, "6"}, "NPage": {kCSITilde, "6"}, "PgDn": {kCSITilde, "6"},
	"F1": {kSS3, "P"}, "F2": {kSS3, "Q"}, "F3": {kSS3, "R"}, "F4": {kSS3, "S"},
	"F5": {kCSITilde, "15"}, "F6": {kCSITilde, "17"}, "F7": {kCSITilde, "18"},
	"F8": {kCSITilde, "19"}, "F9": {kCSITilde, "20"}, "F10": {kCSITilde, "21"},
	"F11": {kCSITilde, "23"}, "F12": {kCSITilde, "24"},
	"KP0": {kChar, "0"}, "KP1": {kChar, "1"}, "KP2": {kChar, "2"}, "KP3": {kChar, "3"},
	"KP4": {kChar, "4"}, "KP5": {kChar, "5"}, "KP6": {kChar, "6"}, "KP7": {kChar, "7"},
	"KP8": {kChar, "8"}, "KP9": {kChar, "9"},
	"KP/": {kChar, "/"}, "KP*": {kChar, "*"}, "KP-": {kChar, "-"}, "KP+": {kChar, "+"}, "KP.": {kChar, "."},
}

// encodeKey returns the input bytes of one tmux key name. An unknown name is an
// error, so a bad key is found before anything is sent.
func encodeKey(name string) ([]byte, error) {
	if name == "" || !utf8.ValidString(name) {
		return nil, fmt.Errorf("conpty: invalid key %q", name)
	}
	mods, base := 0, name
	for len(base) > 2 && base[1] == '-' && (base[0] == 'C' || base[0] == 'M' || base[0] == 'S') {
		switch base[0] {
		case 'C':
			mods |= modCtrl
		case 'M':
			mods |= modMeta
		case 'S':
			mods |= modShift
		}
		base = base[2:]
	}
	if def, ok := keyTable[base]; ok {
		return encodeNamed(base, def, mods), nil
	}
	r, n := utf8.DecodeRuneInString(base)
	if n != len(base) || r == utf8.RuneError || !unicode.IsPrint(r) {
		return nil, fmt.Errorf("conpty: unknown key %q", name)
	}
	return encodeRune(r, mods), nil
}

func encodeNamed(name string, def keyDef, mods int) []byte {
	param := strconv.Itoa(1 + (mods & (modShift | modMeta | modCtrl)))
	if name == "Space" && mods&modShift != 0 {
		// S-Space has no distinct byte, and C-S-Space would lose Shift in
		// the NUL byte: both go through Win32 input mode.
		return win32Key(win32Keys[name], mods)
	}
	switch def.kind {
	case kChar:
		r, _ := utf8.DecodeRuneInString(def.seq)
		return encodeRune(r, mods)
	case kCSILetter:
		if mods == 0 {
			return []byte("\x1b[" + def.seq)
		}
		return []byte("\x1b[1;" + param + def.seq)
	case kCSITilde:
		if mods == 0 {
			return []byte("\x1b[" + def.seq + "~")
		}
		return []byte("\x1b[" + def.seq + ";" + param + "~")
	case kSS3:
		if mods == 0 {
			return []byte("\x1bO" + def.seq)
		}
		return []byte("\x1b[1;" + param + def.seq)
	}
	// Literal keys: Shift-Tab is the back-tab sequence, Ctrl-BSpace is BS and
	// Meta prefixes ESC. Every other Shift or Ctrl combination (S-Enter,
	// C-Enter, S-Escape, C-S-Tab, C-BTab, C-S-BSpace, ...) has no distinct
	// plain byte: it is sent as a Win32 input mode key event so the program
	// sees every modifier. Sending a plain byte would turn S-Enter into Enter
	// and submit pending input, and ESC [ Z or BS would silently drop Ctrl or
	// Shift.
	out := []byte(def.seq)
	key := name
	if name == "BTab" {
		key, mods = "Tab", mods|modShift // BTab is Shift-Tab
	}
	switch {
	case key == "Tab" && mods&modShift != 0 && mods&modCtrl == 0:
		out = []byte("\x1b[Z")
	case key == "BSpace" && mods&modCtrl != 0 && mods&modShift == 0:
		out = []byte{0x08}
	case mods&(modShift|modCtrl) != 0:
		if ev, ok := win32Keys[key]; ok {
			return win32Key(ev, mods)
		}
	}
	if mods&modMeta != 0 {
		out = append([]byte{0x1b}, out...)
	}
	return out
}

func encodeRune(r rune, mods int) []byte {
	if mods&modShift != 0 {
		r = unicode.ToUpper(r)
	}
	var out []byte
	switch c := ctrlCode(r); {
	case mods&modCtrl != 0 && c >= 0:
		out = []byte{byte(c)}
	default:
		out = utf8.AppendRune(nil, r)
	}
	if mods&modMeta != 0 {
		out = append([]byte{0x1b}, out...)
	}
	return out
}

// ctrlCode returns the control character of Ctrl plus r, or -1.
func ctrlCode(r rune) int {
	switch {
	case r >= 'a' && r <= 'z':
		return int(r - 'a' + 1)
	case r >= 'A' && r <= 'Z':
		return int(r - 'A' + 1)
	case r == ' ' || r == '@':
		return 0
	case r >= '[' && r <= '_':
		return int(r - '[' + 0x1b)
	case r == '?':
		return 0x7f
	}
	return -1
}

// win32Event describes a key for Win32 input mode: the virtual key code, the
// scan code, the character and whether the key is an enhanced (keypad) key.
type win32Event struct {
	vk, sc, char int
	enhanced     bool
}

var win32Keys = map[string]win32Event{
	"Enter":   {vk: 0x0d, sc: 0x1c, char: '\r'},
	"KPEnter": {vk: 0x0d, sc: 0x1c, char: '\r', enhanced: true},
	"Escape":  {vk: 0x1b, sc: 0x01, char: 0x1b},
	"Tab":     {vk: 0x09, sc: 0x0f, char: '\t'},
	"BSpace":  {vk: 0x08, sc: 0x0e, char: 0x08},
	"Space":   {vk: 0x20, sc: 0x39, char: ' '},
}

// Console control key state bits (dwControlKeyState).
const (
	ctrlStateAlt      = 0x02 // LEFT_ALT_PRESSED
	ctrlStateCtrl     = 0x08 // LEFT_CTRL_PRESSED
	ctrlStateShift    = 0x10 // SHIFT_PRESSED
	ctrlStateEnhanced = 0x100
)

// win32Key encodes a key press and release as ESC [ Vk ; Sc ; Uc ; Kd ; Cs ; Rc _,
// the form ConPTY turns into one console key event per sequence with the
// modifier state intact.
func win32Key(ev win32Event, mods int) []byte {
	cs := 0
	if mods&modShift != 0 {
		cs |= ctrlStateShift
	}
	if mods&modCtrl != 0 {
		cs |= ctrlStateCtrl
	}
	if mods&modMeta != 0 {
		cs |= ctrlStateAlt
	}
	if ev.enhanced {
		cs |= ctrlStateEnhanced
	}
	char := ev.char
	if mods&modCtrl != 0 && ev.vk == 0x0d {
		char = '\n' // the console reports Ctrl+Enter as LF
	}
	var out []byte
	for _, down := range []int{1, 0} {
		out = append(out, fmt.Sprintf("\x1b[%d;%d;%d;%d;%d;1_", ev.vk, ev.sc, char, down, cs)...)
	}
	return out
}
