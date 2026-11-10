package ops

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func sampleScreen() Screen {
	return Screen{
		Width: 20, Height: 3,
		Lines: []string{"$ ls --color", "file.txt  dir", ""},
		Spans: []StyleSpan{
			{Line: 0, Start: 0, End: 1, FG: "p2", Bold: true},
			{Line: 1, Start: 0, End: 8, FG: "#ff8800", BG: "p0", Underline: true},
			{Line: 1, Start: 10, End: 13, FG: "p4", Italic: true, Inverse: true},
		},
		Cursor: &Cursor{Row: 2, Col: 0},
	}
}

func TestScreenJSONShapeGolden(t *testing.T) {
	b, err := json.MarshalIndent(sampleScreen(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "screen.golden.json", string(b)+"\n")
}

func TestScreenJSONRoundTrip(t *testing.T) {
	in := sampleScreen()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Screen
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(out)
	if string(b) != string(b2) {
		t.Fatalf("round trip differs:\n%s\n%s", b, b2)
	}
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyScreenUsesArrays(t *testing.T) {
	b, err := json.Marshal(Screen{})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"width":0,"height":0,"lines":[],"spans":[],"cursor":null}`
	if string(b) != want {
		t.Fatalf("got %s want %s", b, want)
	}
}

func TestScreenValidate(t *testing.T) {
	mutate := func(f func(*Screen)) Screen {
		s := sampleScreen()
		f(&s)
		return s
	}
	tests := []struct {
		name    string
		s       Screen
		wantErr bool
	}{
		{"valid", sampleScreen(), false},
		{"empty", Screen{}, false},
		{"unicode", Screen{Lines: []string{"h\u00e9llo w\u00f6rld \u65e5\u672c\u8a9e"}, Spans: []StyleSpan{{Line: 0, Start: 0, End: 5, Bold: true}}}, false},
		{"escape byte", mutate(func(s *Screen) { s.Lines[0] = "\x1b[31mred" }), true},
		{"C1 control", mutate(func(s *Screen) { s.Lines[0] = "a\u009bb" }), true},
		{"newline in line", mutate(func(s *Screen) { s.Lines[0] = "a\nb" }), true},
		{"DEL", mutate(func(s *Screen) { s.Lines[0] = "a\x7f" }), true},
		{"invalid utf8", mutate(func(s *Screen) { s.Lines[0] = "a\xffb" }), true},
		{"span line missing", mutate(func(s *Screen) { s.Spans[0].Line = 9 }), true},
		{"span negative start", mutate(func(s *Screen) { s.Spans[0].Start = -1 }), true},
		{"span empty range", mutate(func(s *Screen) { s.Spans[0].End = s.Spans[0].Start }), true},
		{"span past end", mutate(func(s *Screen) { s.Spans[1].End = 99 }), true},
		{"span out of order", mutate(func(s *Screen) { s.Spans[0], s.Spans[2] = s.Spans[2], s.Spans[0] }), true},
		{"span overlap", mutate(func(s *Screen) { s.Spans[2].Start = 7 }), true},
		{"bad fg", mutate(func(s *Screen) { s.Spans[0].FG = "red" }), true},
		{"bad bg", mutate(func(s *Screen) { s.Spans[0].BG = "p256" }), true},
		{"negative cursor", mutate(func(s *Screen) { s.Cursor.Col = -1 }), true},
		{"negative size", mutate(func(s *Screen) { s.Width = -1 }), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.s.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error does not match ErrInvalidArgument: %v", err)
			}
		})
	}
}

func TestValidColor(t *testing.T) {
	good := []string{"p0", "p7", "p15", "p16", "p255", "#000000", "#ffffff", "#0a1b2c"}
	bad := []string{"", "p", "p256", "p01", "p-1", "pa", "#fff", "#FFFFFF", "#gggggg", "#0a1b2c0", "red", "P1", "p1000"}
	for _, c := range good {
		if !ValidColor(c) {
			t.Errorf("ValidColor(%q) = false", c)
		}
	}
	for _, c := range bad {
		if ValidColor(c) {
			t.Errorf("ValidColor(%q) = true", c)
		}
	}
}

func TestScreenString(t *testing.T) {
	if got := sampleScreen().String(); got != "$ ls --color\nfile.txt  dir\n" {
		t.Fatalf("String() = %q", got)
	}
	if got := (Screen{}).String(); got != "" {
		t.Fatalf("empty String() = %q", got)
	}
	if strings.Contains(sampleScreen().String(), "\x1b") {
		t.Fatal("escape in text")
	}
}

func TestStyleSpanIsDefault(t *testing.T) {
	if !(StyleSpan{Line: 1, Start: 0, End: 2}).IsDefault() {
		t.Error("plain span should be default")
	}
	if (StyleSpan{Bold: true}).IsDefault() || (StyleSpan{FG: "p1"}).IsDefault() {
		t.Error("styled span should not be default")
	}
}
