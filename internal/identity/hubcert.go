package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"strings"
	"time"
)

// Hub certificate wire layout (design hub-identity-v2, section 5).
const (
	// HubCertVersion is the only certificate version.
	HubCertVersion = 1
	// HubCertPayloadSize is the signed part: 1+32+32+8+8+8.
	HubCertPayloadSize = 89
	// HubCertSize is the wire certificate: payload plus signature.
	HubCertSize = HubCertPayloadSize + ed25519.SignatureSize // 153
	// MaxHubCertValidityMs is 400 days in milliseconds.
	MaxHubCertValidityMs int64 = 400 * 24 * 3600 * 1000
	// HubCertToleranceMs is the clock tolerance before not_before (1 hour).
	HubCertToleranceMs int64 = 3600 * 1000
	// MaxHubCertSerial is 2^63-1.
	MaxHubCertSerial uint64 = math.MaxInt64
)

// HubCert is a parsed hub certificate. Signature is nil for a certificate
// parsed from a bare payload.
type HubCert struct {
	RootPub     ed25519.PublicKey
	OpPub       ed25519.PublicKey
	Serial      uint64
	NotBeforeMs int64
	NotAfterMs  int64
	Signature   []byte
}

func certMalformed() error { return ErrCertMalformed }

// subChecked returns a-b, and false on int64 overflow.
func subChecked(a, b int64) (int64, bool) {
	d := a - b
	if (b > 0 && d > a) || (b < 0 && d < a) {
		return 0, false
	}
	return d, true
}

// validate applies steps 3 to 5 of the parse order (version and length are
// checked by the callers that have the bytes).
func (c *HubCert) validate() error {
	if len(c.RootPub) != ed25519.PublicKeySize || len(c.OpPub) != ed25519.PublicKeySize {
		return certMalformed()
	}
	if bytes.Equal(c.RootPub, c.OpPub) {
		return certMalformed()
	}
	if c.Serial < 1 || c.Serial > MaxHubCertSerial {
		return certMalformed()
	}
	if c.NotBeforeMs < 0 {
		return certMalformed()
	}
	d, ok := subChecked(c.NotAfterMs, c.NotBeforeMs)
	if !ok || d <= 0 || d > MaxHubCertValidityMs {
		return certMalformed()
	}
	return nil
}

// Payload returns the 89-byte signed payload. It assumes a valid certificate.
func (c *HubCert) Payload() []byte {
	p := make([]byte, 0, HubCertPayloadSize)
	p = append(p, HubCertVersion)
	p = append(p, c.RootPub...)
	p = append(p, c.OpPub...)
	p = binary.BigEndian.AppendUint64(p, c.Serial)
	p = binary.BigEndian.AppendUint64(p, uint64(c.NotBeforeMs))
	p = binary.BigEndian.AppendUint64(p, uint64(c.NotAfterMs))
	return p
}

// Bytes returns the 153-byte wire certificate. It returns nil when the
// certificate carries no 64-byte signature.
func (c *HubCert) Bytes() []byte {
	if len(c.Signature) != ed25519.SignatureSize {
		return nil
	}
	return append(c.Payload(), c.Signature...)
}

// SHA256 returns the identity of the certificate: the SHA-256 of its 153
// bytes (zero value when unsigned).
func (c *HubCert) SHA256() [32]byte {
	b := c.Bytes()
	if b == nil {
		return [32]byte{}
	}
	return sha256.Sum256(b)
}

// Encode returns the base64url (no padding) form, 204 characters.
func (c *HubCert) Encode() string { return EncodeB64(c.Bytes()) }

// ParseHubCertPayload applies steps 2 to 5 of the parse order to a bare
// 89-byte payload (for a signing tool). The result has no signature.
func ParseHubCertPayload(p []byte) (*HubCert, error) {
	if len(p) != HubCertPayloadSize || p[0] != HubCertVersion {
		return nil, certMalformed()
	}
	c := &HubCert{
		RootPub:     ed25519.PublicKey(bytes.Clone(p[1:33])),
		OpPub:       ed25519.PublicKey(bytes.Clone(p[33:65])),
		Serial:      binary.BigEndian.Uint64(p[65:73]),
		NotBeforeMs: int64(binary.BigEndian.Uint64(p[73:81])),
		NotAfterMs:  int64(binary.BigEndian.Uint64(p[81:89])),
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseHubCert applies steps 1 to 5 of the parse order to a 153-byte wire
// certificate. It does not check the signature; call Verify.
func ParseHubCert(b []byte) (*HubCert, error) {
	if len(b) != HubCertSize {
		return nil, certMalformed()
	}
	c, err := ParseHubCertPayload(b[:HubCertPayloadSize])
	if err != nil {
		return nil, err
	}
	c.Signature = bytes.Clone(b[HubCertPayloadSize:])
	return c, nil
}

// ParseHubCertB64 decodes the base64url form and parses it.
func ParseHubCertB64(s string) (*HubCert, error) {
	b, err := DecodeB64(s)
	if err != nil {
		return nil, certMalformed()
	}
	return ParseHubCert(b)
}

// Verify is step 6: the certificate must name expectedRoot and carry a valid
// root signature (purpose hub-cert) over its payload. It returns
// ErrCertRootMismatch or ErrCertBadSignature.
func (c *HubCert) Verify(expectedRoot ed25519.PublicKey) error {
	if len(expectedRoot) != ed25519.PublicKeySize || !bytes.Equal(c.RootPub, expectedRoot) {
		return ErrCertRootMismatch
	}
	if len(c.Signature) != ed25519.SignatureSize ||
		Verify(c.RootPub, PurposeHubCert, c.Payload(), c.Signature) != nil {
		return ErrCertBadSignature
	}
	return nil
}

// IssueHubCert builds and signs a certificate with the root key. The root
// private key must be a valid Ed25519 key; the fields follow the parse rules.
func IssueHubCert(rootPriv ed25519.PrivateKey, opPub ed25519.PublicKey, serial uint64, notBeforeMs, notAfterMs int64) (*HubCert, error) {
	if len(rootPriv) != ed25519.PrivateKeySize {
		return nil, newErr(kindInvalidArgument, "private key must be 64 bytes")
	}
	sign := func(p Purpose, data []byte) ([]byte, error) { return Sign(rootPriv, p, data) }
	return issueHubCert(rootPriv.Public().(ed25519.PublicKey), sign, opPub, serial, notBeforeMs, notAfterMs)
}

// issueHubCert builds a certificate for rootPub and signs it with sign.
func issueHubCert(rootPub ed25519.PublicKey, sign func(Purpose, []byte) ([]byte, error), opPub ed25519.PublicKey, serial uint64, notBeforeMs, notAfterMs int64) (*HubCert, error) {
	c := &HubCert{
		RootPub:     bytes.Clone(rootPub),
		OpPub:       bytes.Clone(opPub),
		Serial:      serial,
		NotBeforeMs: notBeforeMs,
		NotAfterMs:  notAfterMs,
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	sig, err := sign(PurposeHubCert, c.Payload())
	if err != nil {
		return nil, err
	}
	c.Signature = sig
	return c, nil
}

// CheckWindow is the Worker's wall-clock rule: accept iff
// not_before - 1 h <= nowMs <= not_after. It returns ErrCertNotYetValid or
// ErrCertExpired.
func (c *HubCert) CheckWindow(nowMs int64) error {
	lo, ok := subChecked(c.NotBeforeMs, HubCertToleranceMs)
	if !ok {
		lo = math.MinInt64
	}
	if nowMs < lo {
		return ErrCertNotYetValid
	}
	if nowMs > c.NotAfterMs {
		return ErrCertExpired
	}
	return nil
}

// MonotonicDeadline returns the instant at which the certificate expires,
// computed from now so that the result carries now's monotonic clock reading
// (time.Now() has one): later wall-clock steps cannot move it. A certificate
// that is already expired at now gives ErrCertExpired. A horizon that Go
// cannot represent together with a monotonic reading (a remaining time beyond
// time.Duration, about 292 years, or a monotonic or wall value out of range)
// gives ErrCertMalformed; it is never silently clamped or stripped. If now has
// no monotonic reading, neither has the result.
func (c *HubCert) MonotonicDeadline(now time.Time) (time.Time, error) {
	rem, ok := subChecked(c.NotAfterMs, now.UnixMilli())
	if !ok {
		return time.Time{}, certMalformed()
	}
	if rem < 0 {
		return time.Time{}, ErrCertExpired
	}
	const maxMs = int64(math.MaxInt64 / int64(time.Millisecond))
	if rem > maxMs {
		return time.Time{}, certMalformed()
	}
	d := now.Add(time.Duration(rem) * time.Millisecond)
	if hasMonotonic(now) && !hasMonotonic(d) {
		return time.Time{}, certMalformed()
	}
	return d, nil
}

// hasMonotonic reports whether t carries a monotonic clock reading.
func hasMonotonic(t time.Time) bool {
	return strings.Contains(t.String(), " m=")
}
