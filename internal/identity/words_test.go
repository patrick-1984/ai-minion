package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// The published BIP39 English list (bitcoin/bips, bip-0039/english.txt).
const bip39EnglishSHA256 = "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda"

func TestWordListIsTheBIP39EnglishList(t *testing.T) {
	sum := sha256.Sum256([]byte(bip39English))
	if got := hex.EncodeToString(sum[:]); got != bip39EnglishSHA256 {
		t.Fatalf("word list SHA-256 = %s, want %s", got, bip39EnglishSHA256)
	}
	list := words()
	if len(list) != 2048 {
		t.Fatalf("word list has %d words, want 2048", len(list))
	}
	if list[0] != "abandon" || list[2047] != "zoo" {
		t.Fatalf("first/last word = %q/%q", list[0], list[2047])
	}
	seen := map[string]bool{}
	prefixes := map[string]string{}
	for _, w := range list {
		if seen[w] {
			t.Fatalf("repeated word %q", w)
		}
		seen[w] = true
		p := w
		if len(p) > minPrefix {
			p = p[:minPrefix]
		}
		if other, ok := prefixes[p]; ok && len(w) >= minPrefix && len(other) >= minPrefix {
			t.Fatalf("words %q and %q share the prefix %q", other, w, p)
		}
		prefixes[p] = w
	}
}

func TestWordVectors(t *testing.T) {
	zeroKeyFP, err := FingerprintOf(make(ed25519.PublicKey, 32))
	if err != nil {
		t.Fatal(err)
	}
	if zeroKeyFP != "mzuhvlpymk6xo3epygfy5h4oeaejofefn3rdhm4qfjmr2dk7fesq" {
		t.Fatalf("fingerprint of the zero key = %s", zeroKeyFP)
	}
	ff := make([]byte, 32)
	for i := range ff {
		ff[i] = 0xff
	}
	cases := []struct {
		fp   Fingerprint
		want string
	}{
		{"tr4byddbsemo3k5eduu2duwdkz3mir4gm2svhqd43lwga5obgm4q", "organ-science-adapt-segment-bag-item"},
		{zeroKeyFP, "grid-duck-problem-valid-cloth-roof"},
		{Fingerprint(encodeB32(make([]byte, 32))), "abandon-abandon-abandon-abandon-abandon-abandon"},
		{Fingerprint(encodeB32(ff)), "zoo-zoo-zoo-zoo-zoo-zoo"},
	}
	for _, c := range cases {
		if got := c.fp.Words(); got != c.want {
			t.Errorf("%s.Words() = %q, want %q", c.fp, got, c.want)
		}
	}
	if got := WordsOfKey(make(ed25519.PublicKey, 32)); got != "grid-duck-problem-valid-cloth-roof" {
		t.Errorf("WordsOfKey = %q", got)
	}
	if Fingerprint("nope").Words() != "" || WordsOfKey(nil) != "" {
		t.Error("invalid input must give no words")
	}
}

func TestParseFingerprintInputRoundTrip(t *testing.T) {
	for range 1000 {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		fp, _ := FingerprintOf(pub)
		w := fp.Words()
		parts := strings.Split(w, "-")
		prefixes := make([]string, len(parts))
		for i, p := range parts {
			if len(p) > 4 {
				p = p[:4]
			}
			prefixes[i] = p
		}
		inputs := []string{
			w,
			strings.ToUpper(w),
			strings.Join(parts, " "),
			strings.Join(parts, ", "),
			"  " + strings.Join(prefixes, "-") + "  ",
			strings.ToUpper(strings.Join(prefixes, " ")),
			string(fp),
			strings.ToUpper(string(fp)),
		}
		for _, in := range inputs {
			m, err := ParseFingerprintInput(in)
			if err != nil {
				t.Fatalf("ParseFingerprintInput(%q): %v", in, err)
			}
			if !m.Matches(pub) || !m.MatchesFingerprint(fp) {
				t.Fatalf("matcher from %q does not match", in)
			}
			if m.Words() != w {
				t.Fatalf("matcher words = %q, want %q", m.Words(), w)
			}
		}
	}
}

func TestParseFingerprintInputShortWords(t *testing.T) {
	// "act" is a whole word and also the prefix of "action": three letters
	// mean the word itself, four letters select by prefix.
	m, err := ParseFingerprintInput("act act act act act act")
	if err != nil {
		t.Fatal(err)
	}
	if m.Words() != "act-act-act-act-act-act" {
		t.Fatalf("words = %q", m.Words())
	}
	m, err = ParseFingerprintInput("acti-acti-acti-acti-acti-acti")
	if err != nil {
		t.Fatal(err)
	}
	if m.Words() != "action-action-action-action-action-action" {
		t.Fatalf("words = %q", m.Words())
	}
}

func TestParseFingerprintInputErrors(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "expected six words or 52 characters"},
		{"organ science adapt segment bag", "expected six words or 52 characters"},
		{"organ science adapt segment bag item item", "expected six words or 52 characters"},
		{"organ science xyzz segment bag item", `word 3 ("xyzz") is not in the word list`},
		{"organ science ac segment bag item", `word 3 ("ac") is too short`},
		{"organ science \x1b[2A segment bag item", "word 3 is not in the word list"},
		{"tr4byddbsemo3k5eduu2duwdkz3mir4gm2svhqd43lwga5obgm4", "expected six words or 52 characters"},
		{"tr4byddbsemo3k5eduu2duwdkz3mir4gm2svhqd43lwga5obgm41", "expected six words or 52 characters"},
	}
	for _, c := range cases {
		_, err := ParseFingerprintInput(c.in)
		if err == nil || !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseFingerprintInput(%q) = %v, want %q", c.in, err, c.want)
		}
		if err != nil && strings.ContainsRune(err.Error(), 0x1b) {
			t.Errorf("error echoes a control byte: %q", err)
		}
	}
}

func TestFingerprintMatcherZeroAndPrecision(t *testing.T) {
	var zero FingerprintMatcher
	if !zero.IsZero() || zero.Matches(make(ed25519.PublicKey, 32)) || zero.MatchesFingerprint("x") || zero.String() != "" {
		t.Fatal("the zero matcher must match nothing")
	}
	// Two fingerprints that share the first 66 bits: a matcher built from
	// words matches both, a matcher built from the 52 characters only one.
	a := make([]byte, 32)
	b := make([]byte, 32)
	b[31] = 1
	fa, fb := Fingerprint(encodeB32(a)), Fingerprint(encodeB32(b))
	mw, err := ParseFingerprintInput(fa.Words())
	if err != nil {
		t.Fatal(err)
	}
	if mw.Full() || !mw.MatchesFingerprint(fa) || !mw.MatchesFingerprint(fb) {
		t.Fatal("a word matcher compares the first 66 bits")
	}
	mf, err := ParseFingerprintInput(string(fa))
	if err != nil {
		t.Fatal(err)
	}
	if !mf.Full() || !mf.MatchesFingerprint(fa) || mf.MatchesFingerprint(fb) || mf.String() != string(fa) {
		t.Fatal("a full matcher compares all characters")
	}
	if mw.String() != fa.Words() || MatcherOf(fa) != mf {
		t.Fatal("String and MatcherOf")
	}
}
