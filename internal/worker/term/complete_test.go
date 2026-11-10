package term

import (
	"errors"
	"strings"
	"testing"
)

// BuildScreenChecked says whether the builder kept every character; a screen
// that lost content must never be the basis of a binding.
func TestBuildScreenCheckedReportsWhatTheBoundsDropped(t *testing.T) {
	for name, tc := range map[string]struct {
		text     string
		complete bool
	}{
		"empty":                         {"", true},
		"plain rows":                    {"one\ntwo\nthree", true},
		"a row of exactly the cap":      {strings.Repeat("x", maxCols), true},
		"a row one cell over the cap":   {strings.Repeat("x", maxCols+1), false},
		"a wide row of 5000 cells":      {strings.Repeat("x", 5000) + " command-B", false},
		"a wide character over the cap": {strings.Repeat("x", maxCols-1) + "\u4e2d", false},
		"too many combining marks":      {"e" + strings.Repeat("́", maxCombining+1), false},
		"more rows than the cap":        {strings.Repeat("a\n", maxRows+5), false},
		"a cursor move is no loss":      {"abc\x1b[9999Ddef", true},
	} {
		scr, complete := BuildScreenChecked([]byte(tc.text))
		if complete != tc.complete {
			t.Errorf("%s: complete = %v, want %v", name, complete, tc.complete)
		}
		if len(scr.Lines) == 0 && tc.text != "" {
			t.Errorf("%s: no screen at all: a partial screen is still returned", name)
		}
	}
	// BuildScreen is the same screen.
	a, _ := BuildScreenChecked([]byte("x\ny"))
	if b := BuildScreen([]byte("x\ny")); strings.Join(a.Lines, "|") != strings.Join(b.Lines, "|") {
		t.Error("BuildScreen and BuildScreenChecked differ")
	}
	if !errors.Is(errors.Join(ErrIncomplete), ErrIncomplete) {
		t.Error("ErrIncomplete does not match itself")
	}
}
