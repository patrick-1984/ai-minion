package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
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

// MaxAPIKeyFileBytes bounds an API key file; a real key is 55 bytes.
const MaxAPIKeyFileBytes = 4 << 10

// ReadAPIKeyFile reads a Minion API key from the file at path. The file is
// opened with [OpenPrivate] (owner-checked: a regular file of this user that
// group and others cannot access; a symbolic link, a pipe and a directory are
// refused without blocking), holds exactly one line of at most
// [MaxAPIKeyFileBytes] bytes, and that line must be a well-formed key. The
// file is never repaired. It is meant to be called for every use, so that a
// rotated key takes effect at once. The errors name the path and never the
// content.
func ReadAPIKeyFile(path string) (APIKey, error) {
	if path == "" {
		return "", newErr(kindInvalidArgument, "no API key file is configured")
	}
	f, err := OpenPrivate(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxAPIKeyFileBytes+1))
	if err != nil {
		return "", newErr(kindInvalidArgument, "cannot read "+path)
	}
	if len(data) > MaxAPIKeyFileBytes {
		return "", newErr(kindInvalidArgument, path+" is too large to be an API key file")
	}
	s := string(data)
	if t, ok := strings.CutSuffix(s, "\n"); ok {
		s = strings.TrimSuffix(t, "\r")
	}
	k, err := ParseAPIKey(s)
	if err != nil {
		return "", newErr(kindInvalidArgument, path+" does not hold a Minion API key (mn_...) as its only content")
	}
	return k, nil
}
