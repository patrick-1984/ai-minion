package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
)

// keyFileHeader tags the key file format. The file holds the 32-byte Ed25519
// seed in base64url without padding, followed by a newline.
const keyFileHeader = "minion-ed25519-seed-v1:"

// Key is a long-lived Ed25519 identity key (AD-5).
//
// The private key is held in a closure on purpose: fmt cannot call methods
// of an unexported struct field and falls back to reflection, and for a func
// value reflection prints only an address (%v, %s, %x, %d, ... and the bad-verb
// path alike), never the captured bytes. A pointer would not do: fmt follows
// it at depth 0 when it reprints a value after a bad verb. A Key therefore
// prints no key material in any struct field, exported or not, held by value
// or by pointer. A side effect is that Key is not comparable and cannot be a
// map key.
type Key struct {
	priv func() ed25519.PrivateKey
}

// newKey wraps a private key.
func newKey(priv ed25519.PrivateKey) *Key {
	return &Key{priv: func() ed25519.PrivateKey { return priv }}
}

// key returns the private key, or nil for the zero Key.
func (k Key) key() ed25519.PrivateKey {
	if k.priv == nil {
		return nil
	}
	return k.priv()
}

// GenerateKey creates a new random identity key.
func GenerateKey() (*Key, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return newKey(priv), nil
}

// KeyFromSeed builds a key from a 32-byte Ed25519 seed.
func KeyFromSeed(seed []byte) (*Key, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, newErr(kindInvalidArgument, "seed must be 32 bytes")
	}
	return newKey(ed25519.NewKeyFromSeed(seed)), nil
}

// Public returns the raw 32-byte public key.
func (k Key) Public() ed25519.PublicKey {
	return k.key().Public().(ed25519.PublicKey)
}

// Fingerprint returns the fingerprint of the public key.
func (k Key) Fingerprint() Fingerprint {
	f, _ := FingerprintOf(k.Public()) // length is always valid
	return f
}

// Sign signs data for purpose p.
func (k Key) Sign(p Purpose, data []byte) ([]byte, error) {
	return Sign(k.key(), p, data)
}

// String returns the fingerprint, never key material. fmt and slog would
// otherwise reflect into the unexported private key. All redaction methods
// have value receivers, so they apply to a Key value, to a *Key and to a Key
// embedded in another value alike.
func (k Key) String() string {
	if len(k.key()) != ed25519.PrivateKeySize {
		return "<invalid key>"
	}
	return k.Fingerprint().String()
}

// GoString implements fmt.GoStringer: %#v prints the fingerprint.
func (k Key) GoString() string { return k.String() }

// Format implements fmt.Formatter so that no verb (%v, %+v, %#v, %x, %d, ...)
// prints the private key: every verb prints the fingerprint.
func (k Key) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, k.String()) }

// LogValue implements slog.LogValuer: a key is logged as its fingerprint.
func (k Key) LogValue() slog.Value { return slog.StringValue(k.String()) }

// MarshalText implements encoding.TextMarshaler so that JSON and text
// encoders write the fingerprint, never key material.
func (k Key) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// SaveKey writes the key to path atomically as a private file: mode 0600 on
// Unix, a protected owner-and-SYSTEM-only DACL on Windows (A-11).
func SaveKey(path string, k *Key) error {
	content := keyFileHeader + EncodeB64(k.key().Seed()) + "\n"
	return WritePrivate(path, []byte(content))
}

// LoadKey reads a key saved by SaveKey. It opens the file with [OpenPrivate]
// and so refuses a symbolic link, a file that is not owned by the current user
// and a file that other users can access (Unix: any group or world permission
// bit; Windows: a reparse point, a foreign owner or a DACL that grants access to
// another principal). All checks are made on the opened handle.
func LoadKey(path string) (*Key, error) {
	f, err := OpenPrivate(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(raw))
	enc, ok := strings.CutPrefix(s, keyFileHeader)
	if !ok {
		return nil, newErr(kindInvalidArgument, "not a Minion identity key file")
	}
	seed, err := DecodeB64(enc)
	if err != nil {
		return nil, newErr(kindInvalidArgument, "corrupt identity key file")
	}
	return KeyFromSeed(seed)
}

// formatSecret is the shared fmt.Formatter body of the secret types whose
// String method is their display form (invitation key, API key, pairing
// code). %s and %v print display, so that showing the value to the user
// keeps working. %q prints it quoted. %#v and every other verb (%d, %x, ...)
// print redacted, so a dump of the value never contains the secret.
func formatSecret(f fmt.State, verb rune, display, redacted string) {
	switch {
	case verb == 'v' && f.Flag('#'):
		_, _ = io.WriteString(f, redacted)
	case verb == 's' || verb == 'v':
		_, _ = io.WriteString(f, display)
	case verb == 'q':
		_, _ = io.WriteString(f, strconv.Quote(display))
	default:
		_, _ = io.WriteString(f, redacted)
	}
}
