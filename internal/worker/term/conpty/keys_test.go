package conpty

import (
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

func TestEncodeKey(t *testing.T) {
	cases := map[string]string{
		"a": "a", "Z": "Z", "\u00e9": "\u00e9", "\u4e2d": "\u4e2d", " ": " ", "Space": " ",
		"Enter": "\r", "KPEnter": "\r", "Escape": "\x1b", "Tab": "\t", "BSpace": "\x7f",
		"BTab": "\x1b[Z", "S-BTab": "\x1b[Z", "M-BTab": "\x1b\x1b[Z",
		"Up": "\x1b[A", "Down": "\x1b[B", "Right": "\x1b[C", "Left": "\x1b[D",
		"Home": "\x1b[H", "End": "\x1b[F", "IC": "\x1b[2~", "DC": "\x1b[3~",
		"PageUp": "\x1b[5~", "PPage": "\x1b[5~", "PgUp": "\x1b[5~",
		"PageDown": "\x1b[6~", "NPage": "\x1b[6~", "PgDn": "\x1b[6~",
		"F1": "\x1bOP", "F4": "\x1bOS", "F5": "\x1b[15~", "F10": "\x1b[21~", "F12": "\x1b[24~",
		"KP0": "0", "KP9": "9", "KP/": "/", "KP*": "*", "KP-": "-", "KP+": "+", "KP.": ".",
		"C-c": "\x03", "C-C": "\x03", "C-a": "\x01", "C-z": "\x1a", "C-[": "\x1b", "C-\\": "\x1c",
		"C-]": "\x1d", "C-^": "\x1e", "C-_": "\x1f", "C-@": "\x00", "C-?": "\x7f", "C- ": "\x00",
		"C-1": "1",
		"M-x": "\x1bx", "M-Enter": "\x1b\r", "M-C-a": "\x1b\x01",
		"S-a": "A", "S-Tab": "\x1b[Z", "C-BSpace": "\x08",
		"C-Up": "\x1b[1;5A", "S-Up": "\x1b[1;2A", "M-Left": "\x1b[1;3D", "C-S-Right": "\x1b[1;6C",
		"S-Enter":    "\x1b[13;28;13;1;16;1_\x1b[13;28;13;0;16;1_",
		"C-Enter":    "\x1b[13;28;10;1;8;1_\x1b[13;28;10;0;8;1_",
		"S-KPEnter":  "\x1b[13;28;13;1;272;1_\x1b[13;28;13;0;272;1_",
		"M-S-Enter":  "\x1b[13;28;13;1;18;1_\x1b[13;28;13;0;18;1_",
		"S-Escape":   "\x1b[27;1;27;1;16;1_\x1b[27;1;27;0;16;1_",
		"C-S-Tab":    "\x1b[9;15;9;1;24;1_\x1b[9;15;9;0;24;1_",
		"C-BTab":     "\x1b[9;15;9;1;24;1_\x1b[9;15;9;0;24;1_",
		"C-S-BTab":   "\x1b[9;15;9;1;24;1_\x1b[9;15;9;0;24;1_",
		"C-S-BSpace": "\x1b[8;14;8;1;24;1_\x1b[8;14;8;0;24;1_",
		"S-Space":    "\x1b[32;57;32;1;16;1_\x1b[32;57;32;0;16;1_",
		"C-S-Space":  "\x1b[32;57;32;1;24;1_\x1b[32;57;32;0;24;1_",
		"M-C-BTab":   "\x1b[9;15;9;1;26;1_\x1b[9;15;9;0;26;1_",
		"C-PageUp":   "\x1b[5;5~", "S-F5": "\x1b[15;2~", "C-F1": "\x1b[1;5P",
	}
	for name, want := range cases {
		got, err := encodeKey(name)
		if err != nil {
			t.Errorf("%q: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%q = %q, want %q", name, got, want)
		}
	}
}

func TestEncodeKeyRejectsUnknown(t *testing.T) {
	for _, name := range []string{"", "Nope", "ab", "\x1b", "\n", "\xff", "C-", "X-a", "F13", "enter"} {
		if got, err := encodeKey(name); err == nil {
			t.Errorf("%q accepted as %q", name, got)
		}
	}
}

// Every key name that send_keys accepts must be one the adapter can encode,
// with and without modifiers. The ops allow-list is not exported, so the test
// probes a candidate list through ops.SendKeysArgs.Validate: every candidate
// that ops accepts must encode, and the names that ops is known to accept must
// still be accepted by ops (a dropped name fails here, not silently).
func TestEncodeKeyCoversTheOpsAllowList(t *testing.T) {
	known := []string{
		"Enter", "Escape", "Tab", "BTab", "BSpace", "Space",
		"Up", "Down", "Left", "Right", "Home", "End",
		"PageUp", "PPage", "PgUp", "PageDown", "NPage", "PgDn",
		"IC", "DC",
		"KP0", "KP1", "KP2", "KP3", "KP4", "KP5", "KP6", "KP7", "KP8", "KP9",
		"KP/", "KP*", "KP-", "KP+", "KP.", "KPEnter",
	}
	for i := 1; i <= 12; i++ {
		known = append(known, "F"+strconv.Itoa(i))
	}
	candidates := append([]string{"Insert", "Delete", "Return", "BackSpace", "Pause", "Menu"}, known...)
	for i := 13; i <= 24; i++ {
		candidates = append(candidates, "F"+strconv.Itoa(i))
	}
	for name := range keyTable {
		candidates = append(candidates, name)
	}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	opsAccepts := func(key string) bool {
		args := ops.SendKeysArgs{
			WriteArgs: ops.WriteArgs{RequestID: id.String()},
			Target:    ops.Target{SessionName: "minion-1"},
			Keys:      []string{key},
		}
		return args.Validate() == nil
	}
	for _, name := range known {
		if !opsAccepts(name) {
			t.Errorf("ops no longer accepts %q: update this test", name)
		}
	}
	for _, name := range candidates {
		if !opsAccepts(name) {
			continue
		}
		for _, variant := range []string{name, "C-" + name, "M-" + name, "S-" + name} {
			if !opsAccepts(variant) {
				t.Errorf("ops rejects %q", variant)
				continue
			}
			if got, err := encodeKey(variant); err != nil || len(got) == 0 {
				t.Errorf("ops accepts %q but encodeKey fails: %q, %v", variant, got, err)
			}
		}
	}
}

// Shift+Enter must not collapse into Enter: that would submit pending input
// instead of inserting a newline.
func TestEncodeKeyShiftEnterDiffersFromEnter(t *testing.T) {
	plain, err := encodeKey("Enter")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"S-Enter", "C-Enter", "S-KPEnter", "C-S-Enter"} {
		got, err := encodeKey(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) == string(plain) {
			t.Errorf("%s encodes like Enter: %q", name, got)
		}
	}
}
