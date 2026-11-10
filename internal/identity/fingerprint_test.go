package identity

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"testing/quick"
)

func TestFingerprintShape(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	f := k.Fingerprint()
	if len(f) != 52 {
		t.Fatalf("len = %d", len(f))
	}
	if string(f) != strings.ToLower(string(f)) || strings.Contains(string(f), "=") {
		t.Fatalf("not lowercase unpadded: %s", f)
	}
	if !f.Matches(k.Public()) {
		t.Fatal("Matches failed")
	}
}

func TestParseFingerprintCaseInsensitive(t *testing.T) {
	k, _ := KeyFromSeed(make([]byte, 32))
	f := k.Fingerprint()
	got, err := ParseFingerprint(strings.ToUpper(string(f)))
	if err != nil {
		t.Fatal(err)
	}
	if got != f {
		t.Fatalf("stored %s, want lowercase %s", got, f)
	}
}

func TestParseFingerprintRejects(t *testing.T) {
	k, _ := KeyFromSeed(make([]byte, 32))
	good := string(k.Fingerprint())
	cases := map[string]string{
		"empty":     "",
		"short":     good[:51],
		"long":      good + "a",
		"padding":   good[:51] + "=",
		"bad chars": good[:51] + "1",
		"space":     good[:51] + " ",
		// the last base32 character carries 4 trailing bits that must be zero
		"non-canonical": good[:51] + "b",
	}
	for name, in := range cases {
		if _, err := ParseFingerprint(in); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestFingerprintOfBadKey(t *testing.T) {
	if _, err := FingerprintOf(ed25519.PublicKey(make([]byte, 31))); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v", err)
	}
}

func TestFingerprintRoundTripProperty(t *testing.T) {
	err := quick.Check(func(seed [32]byte) bool {
		k, _ := KeyFromSeed(seed[:])
		f := k.Fingerprint()
		p, err := ParseFingerprint(string(f))
		q, err2 := ParseFingerprint(strings.ToUpper(string(f)))
		return err == nil && err2 == nil && p == f && q == f && len(f) == 52
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}
