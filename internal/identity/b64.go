package identity

import "encoding/base64"

// b64 is base64url (RFC 4648 section 5) without padding. Strict mode rejects
// non-zero trailing bits, so every value has exactly one encoding.
var b64 = base64.RawURLEncoding.Strict()

// EncodeB64 returns the base64url encoding of b without padding. Every binary
// field in JSON (keys, nonces, signatures) uses it.
func EncodeB64(b []byte) string { return b64.EncodeToString(b) }

// DecodeB64 decodes base64url without padding. Padding, whitespace including
// CR and LF, the standard alphabet and non-canonical trailing bits are
// rejected: only the canonical encoding is accepted.
func DecodeB64(s string) ([]byte, error) {
	b, err := b64.DecodeString(s)
	// Go's decoder silently skips CR and LF; requiring that the value
	// re-encodes to exactly the input rejects them and anything else
	// non-canonical.
	if err != nil || b64.EncodeToString(b) != s {
		return nil, newErr(kindInvalidArgument, "invalid base64url value")
	}
	return b, nil
}

// B64 is a byte slice that marshals to and from a base64url JSON string
// without padding.
type B64 []byte

// MarshalText implements encoding.TextMarshaler.
func (b B64) MarshalText() ([]byte, error) { return []byte(EncodeB64(b)), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (b *B64) UnmarshalText(text []byte) error {
	d, err := DecodeB64(string(text))
	if err != nil {
		return err
	}
	*b = d
	return nil
}
