package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// FingerprintLen is the length of a fingerprint in characters.
const FingerprintLen = 52

// b32 is RFC 4648 base32 (alphabet A-Z2-7) in lowercase, without padding.
// It is the encoding of fingerprints, invitation secrets and API keys.
var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// encodeB32 returns lowercase unpadded base32.
func encodeB32(b []byte) string { return strings.ToLower(b32.EncodeToString(b)) }

// decodeB32Lower decodes exactly n bytes from lowercase unpadded base32. The
// input must be the canonical encoding: lowercase alphabet only and zero
// trailing bits.
func decodeB32Lower(s string, n int) ([]byte, bool) {
	if len(s) != b32.EncodedLen(n) {
		return nil, false
	}
	b, err := b32.DecodeString(strings.ToUpper(s))
	if err != nil || len(b) != n || encodeB32(b) != s {
		return nil, false
	}
	return b, true
}

// Fingerprint is the identity of a public key: the lowercase RFC 4648 base32
// of SHA-256 of the raw 32-byte key, 52 characters, no padding (AD-5). The
// zero value is invalid; build one with FingerprintOf or ParseFingerprint.
type Fingerprint string

// FingerprintOf returns the fingerprint of a raw Ed25519 public key.
func FingerprintOf(pub ed25519.PublicKey) (Fingerprint, error) {
	if len(pub) != ed25519.PublicKeySize {
		return "", newErr(kindInvalidArgument, "public key must be 32 bytes")
	}
	sum := sha256.Sum256(pub)
	return Fingerprint(encodeB32(sum[:])), nil
}

// ParseFingerprint accepts a fingerprint in any letter case and returns it in
// the canonical lowercase form. Anything that is not exactly 52 characters of
// the base32 alphabet is rejected.
func ParseFingerprint(s string) (Fingerprint, error) {
	lower := strings.ToLower(s)
	if _, ok := decodeB32Lower(lower, sha256.Size); !ok {
		return "", newErr(kindInvalidArgument, "invalid fingerprint")
	}
	return Fingerprint(lower), nil
}

// String returns the canonical lowercase form.
func (f Fingerprint) String() string { return string(f) }

// Matches reports whether pub has this fingerprint.
func (f Fingerprint) Matches(pub ed25519.PublicKey) bool {
	got, err := FingerprintOf(pub)
	return err == nil && got == f
}
