package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strings"
)

const (
	invitePrefix     = "mnk_"
	inviteSecretSize = 32
)

// InviteKey is a parsed invitation key mnk_<fingerprint>.<secret> (AD-7).
// The fingerprint is the Hub's, so the Worker can verify the Hub before it
// sends the secret. The secret is 32 random bytes in lowercase base32 without
// padding (52 characters).
type InviteKey struct {
	Fingerprint Fingerprint
	Secret      []byte
}

// NewInviteKey creates an invitation key for the Hub with fingerprint hubFpr.
func NewInviteKey(hubFpr Fingerprint) (InviteKey, error) {
	fpr, err := ParseFingerprint(string(hubFpr))
	if err != nil {
		return InviteKey{}, err
	}
	secret := make([]byte, inviteSecretSize)
	_, _ = rand.Read(secret) // never fails since Go 1.24
	return InviteKey{Fingerprint: fpr, Secret: secret}, nil
}

// String returns the textual form mnk_<fingerprint>.<secret>. It contains the
// secret: never log it.
func (k InviteKey) String() string {
	return invitePrefix + string(k.Fingerprint) + "." + encodeB32(k.Secret)
}

// LogValue implements slog.LogValuer: the fingerprint is public, the secret is
// redacted ("mnk_<fingerprint>.[redacted]").
func (k InviteKey) LogValue() slog.Value {
	return slog.StringValue(invitePrefix + string(k.Fingerprint) + ".[redacted]")
}

// GoString implements fmt.GoStringer: %#v prints the redacted form.
func (k InviteKey) GoString() string { return k.LogValue().String() }

// Format implements fmt.Formatter, see formatSecret.
func (k InviteKey) Format(f fmt.State, verb rune) {
	formatSecret(f, verb, k.String(), k.LogValue().String())
}

// SecretHash returns SHA-256 of the secret bytes. The Hub stores only this.
func (k InviteKey) SecretHash() [sha256.Size]byte { return sha256.Sum256(k.Secret) }

// ParseInviteKey parses mnk_<fingerprint>.<secret>. The fingerprint may be in
// any letter case and is returned lowercase; the secret must be lowercase
// base32. A wrong prefix, wrong lengths or a bad alphabet fail with
// ErrInviteInvalid. The error never echoes the input.
func ParseInviteKey(s string) (InviteKey, error) {
	bad := func() (InviteKey, error) {
		return InviteKey{}, newErr(kindInviteInvalid, "malformed invitation key")
	}
	rest, ok := strings.CutPrefix(s, invitePrefix)
	if !ok {
		return bad()
	}
	fprText, secretText, ok := strings.Cut(rest, ".")
	if !ok {
		return bad()
	}
	fpr, err := ParseFingerprint(fprText)
	if err != nil {
		return bad()
	}
	secret, ok := decodeB32Lower(secretText, inviteSecretSize)
	if !ok {
		return bad()
	}
	return InviteKey{Fingerprint: fpr, Secret: secret}, nil
}
