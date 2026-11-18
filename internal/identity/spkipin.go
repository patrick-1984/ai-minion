package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// TLS key pins in the RFC 7469 form that curl --pinnedpubkey takes:
// "sha256//" and the standard base64 of the SHA-256 of a certificate's
// DER-encoded SubjectPublicKeyInfo. A pin names the key, not the
// certificate, so it survives a renewal of the certificate with the same key.

// SPKIPinPrefix starts every TLS key pin.
const SPKIPinPrefix = "sha256//"

// ErrSPKIPin means a TLS key pin is not in the form sha256//<base64>.
var ErrSPKIPin = errors.New("not a TLS key pin")

// SPKIPin returns the key pin of a DER-encoded SubjectPublicKeyInfo (the
// RawSubjectPublicKeyInfo of an x509.Certificate).
func SPKIPin(spki []byte) string {
	sum := sha256.Sum256(spki)
	return SPKIPinPrefix + base64.StdEncoding.EncodeToString(sum[:])
}

// ParseSPKIPins parses one or more key pins separated by ";" (spaces around
// them are ignored) and returns them in the canonical form. An empty list, an
// empty entry or an entry that is not sha256// and the base64 of 32 bytes is
// [ErrSPKIPin]; the error repeats no value, only the position of the entry.
func ParseSPKIPins(v string) ([]string, error) {
	parts := strings.Split(v, ";")
	pins := make([]string, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		rest, ok := strings.CutPrefix(p, SPKIPinPrefix)
		sum, err := base64.StdEncoding.Strict().DecodeString(rest)
		if !ok || err != nil || len(sum) != sha256.Size {
			return nil, fmt.Errorf("%w: entry %d of %d must be sha256// and the base64 of a SHA-256 (44 characters), as curl --pinnedpubkey takes it", ErrSPKIPin, i+1, len(parts))
		}
		pins = append(pins, SPKIPinPrefix+base64.StdEncoding.EncodeToString(sum))
	}
	return pins, nil
}
