package proto

import (
	"encoding/json"
	"errors"
	"testing"
)

func v(maj, min uint16) Version { return Version{maj, min} }

func TestCurrentIsTwoPointZero(t *testing.T) {
	if Current != v(2, 0) || Current.String() != "2.0" {
		t.Fatalf("Current = %v", Current)
	}
}

func TestParseVersion(t *testing.T) {
	good := map[string]Version{"1.0": v(1, 0), "1.12": v(1, 12), "0.0": v(0, 0), "65535.65535": v(65535, 65535)}
	for s, want := range good {
		got, err := ParseVersion(s)
		if err != nil || got != want || got.String() != s {
			t.Errorf("ParseVersion(%q) = %v, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "1", "1.", ".1", "a.b", "1.0.0", "-1.0", "1.-1", "+1.0", "65536.0", "1. 0", "v1.0"} {
		if _, err := ParseVersion(s); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("ParseVersion(%q) err = %v", s, err)
		}
	}
}

func TestVersionJSONAndCompare(t *testing.T) {
	r := Range{Min: v(1, 0), Max: v(1, 3)}
	b, err := json.Marshal(r)
	if err != nil || string(b) != `{"proto_min":"1.0","proto_max":"1.3"}` {
		t.Fatalf("%s %v", b, err)
	}
	var back Range
	if err := json.Unmarshal(b, &back); err != nil || back != r {
		t.Fatalf("%+v %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"proto_min":"x"}`), &back); err == nil {
		t.Fatal("bad version accepted")
	}
	if v(1, 2).Compare(v(1, 3)) != -1 || v(2, 0).Compare(v(1, 9)) != 1 || v(1, 1).Compare(v(1, 1)) != 0 {
		t.Fatal("Compare")
	}
}

func TestSupportedWindow(t *testing.T) {
	tests := []struct {
		cur  Version
		want []Version
	}{
		{v(1, 0), []Version{v(1, 0)}},
		{v(1, 1), []Version{v(1, 1), v(1, 0)}},
		{v(1, 5), []Version{v(1, 5), v(1, 4)}},
		{v(2, 0), []Version{v(2, 0)}},
	}
	for _, tc := range tests {
		got := Supported(tc.cur)
		if len(got) != len(tc.want) {
			t.Fatalf("Supported(%v) = %v", tc.cur, got)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("Supported(%v) = %v", tc.cur, got)
			}
		}
	}
	if r := SupportedRange(v(1, 5)); r != (Range{v(1, 4), v(1, 5)}) {
		t.Fatalf("range = %+v", r)
	}
}

func TestNegotiateMatrix(t *testing.T) {
	hub := Supported(v(1, 3)) // 1.3 and 1.2
	tests := []struct {
		name   string
		worker Range
		hub    []Version
		want   Version
		code   Code // empty means success
	}{
		{"exact current", Range{v(1, 3), v(1, 3)}, hub, v(1, 3), ""},
		{"worker wider picks highest", Range{v(1, 0), v(1, 9)}, hub, v(1, 3), ""},
		{"worker max below hub current picks N-1", Range{v(1, 0), v(1, 2)}, hub, v(1, 2), ""},
		{"worker is exactly N-1", Range{v(1, 2), v(1, 2)}, hub, v(1, 2), ""},
		{"worker newer than hub overlaps N", Range{v(1, 3), v(1, 7)}, hub, v(1, 3), ""},
		{"worker too old (below N-1)", Range{v(1, 0), v(1, 1)}, hub, v(0, 0), CodeProtoIncompatible},
		{"worker too new (above hub)", Range{v(1, 4), v(1, 6)}, hub, v(0, 0), CodeProtoIncompatible},
		{"major mismatch lower", Range{v(0, 1), v(0, 9)}, hub, v(0, 0), CodeProtoIncompatible},
		{"major mismatch higher", Range{v(2, 0), v(2, 1)}, hub, v(0, 0), CodeProtoIncompatible},
		{"range spanning majors", Range{v(0, 5), v(2, 0)}, hub, v(1, 3), ""},
		{"release 2 both sides", Range{Current, Current}, Supported(Current), v(2, 0), ""},
		{"a 1.x worker is refused by a 2.0 Hub", Range{v(1, 0), v(1, 9)}, Supported(Current), v(0, 0), CodeProtoIncompatible},
		{"a 2.0 worker is refused by a 1.x Hub", Range{Current, Current}, Supported(v(1, 3)), v(0, 0), CodeProtoIncompatible},
		{"inverted worker range", Range{v(1, 3), v(1, 0)}, hub, v(0, 0), CodeInvalidArgument},
		{"empty hub set", Range{v(1, 0), v(1, 0)}, nil, v(0, 0), CodeInvalidArgument},
		{"hub set unordered", Range{v(1, 0), v(1, 9)}, []Version{v(1, 2), v(1, 3)}, v(1, 3), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Negotiate(tc.worker, tc.hub)
			if tc.code == "" {
				if err != nil || got != tc.want {
					t.Fatalf("got %v, %v; want %v", got, err, tc.want)
				}
				return
			}
			if err == nil || CodeOf(err) != tc.code {
				t.Fatalf("err = %v, want code %s", err, tc.code)
			}
			if tc.code == CodeProtoIncompatible && !errors.Is(err, ErrProtoIncompatible) {
				t.Fatal("errors.Is(ErrProtoIncompatible) failed")
			}
		})
	}
}
