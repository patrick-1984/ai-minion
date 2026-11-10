package identity

import (
	"errors"
	"strings"
	"testing"
)

func TestNewPairingCode(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		c := NewPairingCode()
		s := c.String()
		if len(s) != 12 || s[:3] != "MN-" || s[7] != '-' {
			t.Fatalf("bad format %q", s)
		}
		for _, ch := range strings.ReplaceAll(s[3:], "-", "") {
			if !strings.ContainsRune(crockford, ch) {
				t.Fatalf("char %q not Crockford in %q", ch, s)
			}
		}
		if c.PairingID() != s[3:7] || len(c.PairingID()) != 4 {
			t.Fatalf("pairing id %q for %q", c.PairingID(), s)
		}
		p, err := ParsePairingCode(s)
		if err != nil || p != c {
			t.Fatalf("round trip %v %v", p, err)
		}
		seen[s] = true
	}
	if len(seen) < 190 {
		t.Fatalf("codes repeat: %d unique of 200", len(seen))
	}
}

func TestParsePairingCodeForms(t *testing.T) {
	want, err := ParsePairingCode("MN-AB12-CD34")
	if err != nil {
		t.Fatal(err)
	}
	if want.String() != "MN-AB12-CD34" || want.Canonical() != "AB12CD34" || string(want.Password()) != "AB12CD34" || want.PairingID() != "AB12" {
		t.Fatalf("unexpected %+v", want)
	}
	for _, in := range []string{
		"MN-AB12-CD34", "mn-ab12-cd34", "Mn-aB12-Cd34", "AB12CD34", "ab12cd34",
		"AB12-CD34", "MNAB12CD34", "mnab12cd34", " MN-AB12-CD34 ", "MN AB12 CD34",
		"M-N-A-B-1-2-C-D-3-4",
	} {
		got, err := ParsePairingCode(in)
		if err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
}

func TestParsePairingCodeAliases(t *testing.T) {
	// O -> 0, I -> 1, L -> 1
	want, _ := ParsePairingCode("MN-0011-0011")
	for _, in := range []string{"MN-OOII-OOLL", "mn-ooll-oo11", "MN-0o1i-ool1"} {
		got, err := ParsePairingCode(in)
		if err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	// A code that starts with the letters MN and has no prefix is 8 chars.
	got, err := ParsePairingCode("MN123456")
	if err != nil || got.Canonical() != "MN123456" {
		t.Errorf("bare MN code: %v %v", got, err)
	}
}

func TestParsePairingCodeRejects(t *testing.T) {
	for _, in := range []string{
		"", "MN-", "MN-ABCD", "MN-ABCD-EFG", "MN-ABCD-EFGHJ", "ABCDEFGHJ", "MN-MN-ABCD-EFGH",
		"MN-ABCU-EFGH", "MN-AB_D-EFGH", "MN-AB+D-EFGH", "MN-ABCD-EFG\u00e9", "MN-ABCD-EFG\x00", "XX-ABCD-EFGH",
	} {
		_, err := ParsePairingCode(in)
		if !errors.Is(err, ErrPairingCodeInvalid) {
			t.Errorf("%q: err = %v", in, err)
		}
	}
}

func TestZeroPairingCode(t *testing.T) {
	var c PairingCode
	if c.String() != "" || c.PairingID() != "" {
		t.Fatal("zero value should be empty")
	}
}

func FuzzParsePairingCode(f *testing.F) {
	for _, s := range []string{"MN-AB12-CD34", "", "mn-oiol-zzzz", "MN", "\xff\xfe", "MN-ABCD-EFGH-"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c, err := ParsePairingCode(s)
		if err != nil {
			if !errors.Is(err, ErrPairingCodeInvalid) {
				t.Fatalf("unexpected error: %v", err)
			}
			return
		}
		if len(c.Canonical()) != 8 || len(c.PairingID()) != 4 {
			t.Fatalf("bad canonical %q", c.Canonical())
		}
		for _, ch := range c.Canonical() {
			if !strings.ContainsRune(crockford, ch) {
				t.Fatalf("non-Crockford char in %q", c.Canonical())
			}
		}
		again, err := ParsePairingCode(c.String())
		if err != nil || again != c {
			t.Fatalf("round trip failed for %q", s)
		}
	})
}
