package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"
)

const (
	apiKeyPrefix = "mn_"
	apiKeySize   = 32
	// APIKeyHintLen is the number of characters after the prefix that Hint
	// returns.
	APIKeyHintLen = 8
)

// APIKey is an API key mn_<base32>: the prefix plus 32 random bytes in
// lowercase base32 without padding (52 characters) (AD-11). The Hub stores
// only the SHA-256 and shows the key once.
type APIKey string

// NewAPIKey creates a random API key.
func NewAPIKey() APIKey {
	b := make([]byte, apiKeySize)
	_, _ = rand.Read(b) // never fails since Go 1.24
	return apiKeyFromBytes(b)
}

func apiKeyFromBytes(b []byte) APIKey { return APIKey(apiKeyPrefix + encodeB32(b)) }

// ParseAPIKey validates the textual form (lowercase only).
func ParseAPIKey(s string) (APIKey, error) {
	rest, ok := strings.CutPrefix(s, apiKeyPrefix)
	if !ok {
		return "", newErr(kindInvalidArgument, "malformed API key")
	}
	if _, ok := decodeB32Lower(rest, apiKeySize); !ok {
		return "", newErr(kindInvalidArgument, "malformed API key")
	}
	return APIKey(s), nil
}

// String returns the key text. It is a secret: never log it. Use Hint for
// listings.
func (k APIKey) String() string { return string(k) }

// LogValue implements slog.LogValuer: only the hint is logged
// ("mn_<hint>...[redacted]").
func (k APIKey) LogValue() slog.Value {
	return slog.StringValue(apiKeyPrefix + k.Hint() + "...[redacted]")
}

// GoString implements fmt.GoStringer: %#v prints the redacted form.
func (k APIKey) GoString() string { return k.LogValue().String() }

// Format implements fmt.Formatter, see formatSecret.
func (k APIKey) Format(f fmt.State, verb rune) {
	formatSecret(f, verb, k.String(), k.LogValue().String())
}

// Hint returns the first 8 characters after the prefix, safe to display.
func (k APIKey) Hint() string {
	rest := strings.TrimPrefix(string(k), apiKeyPrefix)
	if len(rest) > APIKeyHintLen {
		rest = rest[:APIKeyHintLen]
	}
	return rest
}

// Hash returns the SHA-256 of the full key text, the only form meant for
// storage.
func (k APIKey) Hash() [sha256.Size]byte { return sha256.Sum256([]byte(k)) }
