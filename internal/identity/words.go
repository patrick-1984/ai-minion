package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed" // the BIP39 English word list
	"fmt"
	"strings"
	"sync"
)

// Fingerprint words (AD-5 word form): the first 66 bits of the SHA-256 digest
// that a fingerprint encodes, read as six 11-bit indices into the BIP39
// English word list, joined with "-". The words are not a BIP39 mnemonic (no
// checksum, no seed derivation): the list is used only as a well-reviewed set
// of 2048 words whose first four letters are unique.
//
// Words are an acceptance test on a key that was presented and proved; what a
// program stores is always the full key. All six words are compared, never a
// subset.

// WordCount is the number of words of a fingerprint in word form.
const WordCount = 6

// wordBits is the number of bits one word carries (2048 = 2^11 words).
const wordBits = 11

// minPrefix is the shortest prefix that selects a word: the first four
// letters of every word of the list are unique.
const minPrefix = 4

//go:embed bip39_english.txt
var bip39English string

var (
	wordsOnce  sync.Once
	wordList   []string
	wordLookup map[string]uint16
)

func loadWords() {
	wordList = strings.Fields(bip39English)
	wordLookup = make(map[string]uint16, len(wordList))
	for i, w := range wordList {
		wordLookup[w] = uint16(i) //nolint:gosec // i < 2048
	}
}

func words() []string {
	wordsOnce.Do(loadWords)
	return wordList
}

// digestIndices returns the six 11-bit indices of the first 66 bits of a
// SHA-256 digest, most significant bit first.
func digestIndices(digest []byte) [WordCount]uint16 {
	var out [WordCount]uint16
	for k := range WordCount {
		var v uint16
		for b := range wordBits {
			bit := k*wordBits + b
			v = v<<1 | uint16(digest[bit/8]>>(7-bit%8)&1)
		}
		out[k] = v
	}
	return out
}

func joinIndices(idx [WordCount]uint16) string {
	list := words()
	parts := make([]string, WordCount)
	for i, v := range idx {
		parts[i] = list[v]
	}
	return strings.Join(parts, "-")
}

// indices returns the word indices of f, or false for an invalid value.
func (f Fingerprint) indices() ([WordCount]uint16, bool) {
	digest, ok := decodeB32Lower(string(f), sha256.Size)
	if !ok {
		return [WordCount]uint16{}, false
	}
	return digestIndices(digest), true
}

// Words returns the six words of the fingerprint joined with "-", or "" for
// an invalid value.
func (f Fingerprint) Words() string {
	idx, ok := f.indices()
	if !ok {
		return ""
	}
	return joinIndices(idx)
}

// WordsOfKey returns the six words of the fingerprint of pub, or "" for a
// key of the wrong size.
func WordsOfKey(pub ed25519.PublicKey) string {
	f, err := FingerprintOf(pub)
	if err != nil {
		return ""
	}
	return f.Words()
}

// FingerprintMatcher is an expected fingerprint as a human gave it: either
// the full 52 characters or the six words. The zero value matches nothing.
type FingerprintMatcher struct {
	full  Fingerprint
	idx   [WordCount]uint16
	words bool
}

// MatcherOf returns a matcher for the full fingerprint f.
func MatcherOf(f Fingerprint) FingerprintMatcher { return FingerprintMatcher{full: f} }

// ParseFingerprintInput parses what a human typed or pasted as a fingerprint:
// the 52 base32 characters, or six words of the BIP39 English list separated
// by "-", spaces or commas, in any letter case. A word may be typed in full or
// as a prefix of at least four letters; a shorter token must be a whole word.
// Errors wrap ErrInvalidArgument and echo a token only after it was checked to
// consist of letters.
func ParseFingerprintInput(s string) (FingerprintMatcher, error) {
	s = strings.TrimSpace(s)
	tokens := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '-' || r == ',' || r == ' ' || r == '\t'
	})
	if len(tokens) == 1 && len(tokens[0]) == FingerprintLen {
		f, err := ParseFingerprint(tokens[0])
		if err != nil {
			return FingerprintMatcher{}, newErr(kindInvalidArgument, "expected six words or 52 characters")
		}
		return FingerprintMatcher{full: f}, nil
	}
	if len(tokens) != WordCount {
		return FingerprintMatcher{}, newErr(kindInvalidArgument, "expected six words or 52 characters")
	}
	words() // load the lookup
	var m FingerprintMatcher
	m.words = true
	for i, tok := range tokens {
		v, err := lookupWord(tok)
		if err != nil {
			return FingerprintMatcher{}, newErr(kindInvalidArgument, fmt.Sprintf("word %d %s", i+1, err.Error()))
		}
		m.idx[i] = v
	}
	return m, nil
}

type wordErr string

func (e wordErr) Error() string { return string(e) }

func lookupWord(tok string) (uint16, error) {
	letters := true
	for _, r := range tok {
		if r < 'a' || r > 'z' {
			letters = false
			break
		}
	}
	if !letters || len(tok) > 16 {
		return 0, wordErr("is not in the word list")
	}
	if v, ok := wordLookup[tok]; ok {
		return v, nil
	}
	if len(tok) < minPrefix {
		return 0, wordErr(fmt.Sprintf("(%q) is too short: type the whole word or at least four letters", tok))
	}
	for i, w := range wordList {
		if strings.HasPrefix(w, tok) {
			return uint16(i), nil //nolint:gosec // i < 2048
		}
	}
	return 0, wordErr(fmt.Sprintf("(%q) is not in the word list", tok))
}

// IsZero reports whether the matcher is the zero value (no expectation).
func (m FingerprintMatcher) IsZero() bool { return m.full == "" && !m.words }

// Full reports whether the matcher was built from the 52 characters.
func (m FingerprintMatcher) Full() bool { return m.full != "" }

// Fingerprint returns the full fingerprint, or "" for a matcher built from
// words.
func (m FingerprintMatcher) Fingerprint() Fingerprint { return m.full }

// MatchesFingerprint reports whether f is the expected fingerprint. A matcher
// built from words compares all 66 bits the six words carry.
func (m FingerprintMatcher) MatchesFingerprint(f Fingerprint) bool {
	switch {
	case m.full != "":
		return f == m.full
	case m.words:
		idx, ok := f.indices()
		return ok && idx == m.idx
	default:
		return false
	}
}

// Matches reports whether pub has the expected fingerprint.
func (m FingerprintMatcher) Matches(pub ed25519.PublicKey) bool {
	f, err := FingerprintOf(pub)
	return err == nil && m.MatchesFingerprint(f)
}

// Words returns the six words the matcher stands for ("" for the zero value).
func (m FingerprintMatcher) Words() string {
	switch {
	case m.full != "":
		return m.full.Words()
	case m.words:
		return joinIndices(m.idx)
	default:
		return ""
	}
}

// String returns the 52 characters, else the six words, else "". It is the
// form written to a config file.
func (m FingerprintMatcher) String() string {
	if m.full != "" {
		return string(m.full)
	}
	return m.Words()
}
