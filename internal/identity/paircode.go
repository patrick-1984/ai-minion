package identity

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
)

// crockford is the Crockford base32 alphabet (no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	pairCodeChars  = 8
	pairingIDChars = 4
	pairCodePrefix = "MN"
)

// PairingCode is a pairing code MN-XXXX-XXXX: 8 Crockford base32 characters
// (AD-8). The first 4 characters are the public pairing id. The canonical
// form is the 8 uppercase characters without hyphens; it is the CPace
// password.
type PairingCode struct {
	chars string // canonical: 8 uppercase Crockford characters
}

// NewPairingCode creates a random pairing code (40 bits).
func NewPairingCode() PairingCode {
	b := make([]byte, pairCodeChars)
	_, _ = rand.Read(b) // never fails since Go 1.24
	out := make([]byte, pairCodeChars)
	for i, v := range b {
		out[i] = crockford[v&31] // 256 is a multiple of 32: no modulo bias
	}
	return PairingCode{chars: string(out)}
}

// ParsePairingCode accepts any letter case, optional hyphens and spaces, an
// optional MN prefix, and the Crockford aliases O to 0 and I, L to 1. Any
// other character, including U, or a wrong length fails with
// ErrPairingCodeInvalid. The error never echoes the input.
func ParsePairingCode(s string) (PairingCode, error) {
	var norm []byte
	for _, r := range strings.ToUpper(s) {
		switch r {
		case '-', ' ':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if r > 127 {
			return PairingCode{}, newErr(kindPairingCodeInvalid, "malformed pairing code")
		}
		norm = append(norm, byte(r))
	}
	// The prefix "MN" survives normalisation unchanged. A code with 10
	// characters starting with MN carries the prefix.
	if len(norm) == pairCodeChars+len(pairCodePrefix) && string(norm[:2]) == pairCodePrefix {
		norm = norm[2:]
	}
	if len(norm) != pairCodeChars {
		return PairingCode{}, newErr(kindPairingCodeInvalid, "malformed pairing code")
	}
	for _, c := range norm {
		if strings.IndexByte(crockford, c) < 0 {
			return PairingCode{}, newErr(kindPairingCodeInvalid, "malformed pairing code")
		}
	}
	return PairingCode{chars: string(norm)}, nil
}

// String returns the display form MN-XXXX-XXXX. It is a secret until used:
// never log it.
func (c PairingCode) String() string {
	if c.chars == "" {
		return ""
	}
	return pairCodePrefix + "-" + c.chars[:4] + "-" + c.chars[4:]
}

// LogValue implements slog.LogValuer: the public pairing id is logged, the
// rest of the code is redacted ("MN-XXXX-[redacted]").
func (c PairingCode) LogValue() slog.Value {
	if c.chars == "" {
		return slog.StringValue("[redacted]")
	}
	return slog.StringValue(pairCodePrefix + "-" + c.PairingID() + "-[redacted]")
}

// GoString implements fmt.GoStringer: %#v prints the redacted form.
func (c PairingCode) GoString() string { return c.LogValue().String() }

// Format implements fmt.Formatter, see formatSecret.
func (c PairingCode) Format(f fmt.State, verb rune) {
	formatSecret(f, verb, c.String(), c.LogValue().String())
}

// Canonical returns the 8 uppercase characters without hyphens.
func (c PairingCode) Canonical() string { return c.chars }

// Password returns the CPace password bytes: the canonical form.
func (c PairingCode) Password() []byte { return []byte(c.chars) }

// PairingID returns the public pairing id: the first 4 canonical characters.
func (c PairingCode) PairingID() string {
	if len(c.chars) < pairingIDChars {
		return ""
	}
	return c.chars[:pairingIDChars]
}
