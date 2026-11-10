package identity

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

func TestAPIKey(t *testing.T) {
	k := NewAPIKey()
	s := k.String()
	rest, ok := strings.CutPrefix(s, "mn_")
	if !ok || len(rest) != 52 || rest != strings.ToLower(rest) {
		t.Fatalf("bad format %q", s)
	}
	if k.Hint() != rest[:8] || len(k.Hint()) != 8 {
		t.Fatalf("hint %q", k.Hint())
	}
	if k.Hash() != sha256.Sum256([]byte(s)) {
		t.Fatal("hash is not SHA-256 of the key text")
	}
	p, err := ParseAPIKey(s)
	if err != nil || p != k {
		t.Fatalf("%v %v", p, err)
	}
	if NewAPIKey() == k {
		t.Fatal("keys repeat")
	}
}

func TestAPIKeyGolden(t *testing.T) {
	b := make([]byte, 32)
	for i := range b {
		b[i] = byte(i)
	}
	// base32 of bytes 0..31 (RFC 4648, lowercase, no padding)
	k := apiKeyFromBytes(b)
	if len(k) != 3+52 {
		t.Fatalf("len %d", len(k))
	}
	if string(k) != "mn_aaaqeayeaudaocajbifqydiob4ibceqtcqkrmfyydenbwha5dypq" {
		t.Fatalf("got %s", k)
	}
	if k.Hint() != "aaaqeaye" {
		t.Fatalf("hint %s", k.Hint())
	}
}

func TestParseAPIKeyRejects(t *testing.T) {
	good := string(NewAPIKey())
	cases := map[string]string{
		"empty":         "",
		"no prefix":     good[3:],
		"wrong prefix":  "mnk_" + good[3:],
		"short":         good[:len(good)-1],
		"long":          good + "a",
		"uppercase":     strings.ToUpper(good),
		"bad alphabet":  good[:len(good)-1] + "1",
		"non-canonical": good[:len(good)-1] + "b",
	}
	for name, in := range cases {
		if _, err := ParseAPIKey(in); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
