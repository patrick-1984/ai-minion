package transport

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"math"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// Refusal reasons of protocol 3.0 that have no wire code (design
// hub-identity-v2, section 11.2): the Worker closes the connection and records
// the reason locally. The certificate reasons live in internal/identity
// (cert_malformed, cert_root_mismatch, cert_bad_signature, cert_not_yet_valid,
// cert_expired).
const (
	ReasonCertMissing        = "cert_missing"
	ReasonResumeRootMismatch = "resume_root_mismatch"
	ReasonHubSigInvalid      = "hub_sig_invalid"
	ReasonCertSerialTooLow   = "cert_serial_too_low"
	ReasonCertSerialConflict = "cert_serial_conflict"
)

// RefusalError is a Worker-side refusal of the Hub's certificate or identity.
// Two RefusalErrors match under errors.Is when their reasons are equal. For the
// two serial reasons Serial and MaxSerial carry the numbers of the local log
// line (section 6, recovery N1); they are never sent to the Hub.
type RefusalError struct {
	Reason    string
	Serial    uint64
	MaxSerial uint64
}

// Error implements error. The text never contains certificate bytes.
func (e *RefusalError) Error() string {
	switch e.Reason {
	case ReasonCertSerialTooLow, ReasonCertSerialConflict:
		return fmt.Sprintf("transport: %s: the Hub certificate has serial %d but this Worker has already seen serial %d; "+
			"on the Hub run: %s", e.Reason, e.Serial, e.MaxSerial, e.Command())
	}
	return "transport: " + e.Reason
}

// Is matches another RefusalError with the same reason.
func (e *RefusalError) Is(target error) bool {
	t, ok := target.(*RefusalError)
	return ok && t.Reason == e.Reason
}

// Command is the exact operator command that repairs a serial refusal: the
// highest max_serial among the refusing Workers is the floor (section 6).
func (e *RefusalError) Command() string {
	return fmt.Sprintf("minion hub identity reissue --serial-floor %d", e.MaxSerial)
}

// Sentinels for errors.Is.
var (
	ErrCertMissing        = &RefusalError{Reason: ReasonCertMissing}
	ErrResumeRootMismatch = &RefusalError{Reason: ReasonResumeRootMismatch}
	ErrHubSigInvalid      = &RefusalError{Reason: ReasonHubSigInvalid}
	ErrCertSerialTooLow   = &RefusalError{Reason: ReasonCertSerialTooLow}
	ErrCertSerialConflict = &RefusalError{Reason: ReasonCertSerialConflict}
)

// Ratchet is the certificate memory of a Worker, scoped to one root: the
// highest serial it has seen and the SHA-256 of the certificate at that serial
// (section 6). A pin from 0.6 has MaxSerial 0 and no hash. The zero value is an
// empty scope.
type Ratchet struct {
	// Root is the root the numbers belong to.
	Root ed25519.PublicKey
	// MaxSerial is the highest serial seen under Root.
	MaxSerial uint64
	// CertSHA256 is the hash of the certificate at MaxSerial, or nil.
	CertSHA256 []byte
}

// verdict applies the two refusals of section 6 for a certificate of root
// (verified) with the given serial and hash. A scope for another root counts as
// empty: a new root starts at 0.
func (r Ratchet) verdict(root ed25519.PublicKey, serial uint64, hash [32]byte) error {
	if !bytes.Equal(r.Root, root) {
		return nil
	}
	switch {
	case serial < r.MaxSerial:
		return &RefusalError{Reason: ReasonCertSerialTooLow, Serial: serial, MaxSerial: r.MaxSerial}
	case serial == r.MaxSerial && len(r.CertSHA256) > 0 && !bytes.Equal(r.CertSHA256, hash[:]):
		return &RefusalError{Reason: ReasonCertSerialConflict, Serial: serial, MaxSerial: r.MaxSerial}
	}
	return nil
}

// Raise returns the ratchet after a certificate of root was accepted: a new
// root resets the scope first, the serial never goes down, and at an equal
// serial the first hash is kept.
func (r Ratchet) Raise(root ed25519.PublicKey, serial uint64, hash [32]byte) Ratchet {
	if !bytes.Equal(r.Root, root) {
		r = Ratchet{Root: bytes.Clone(root)}
	}
	out := Ratchet{Root: bytes.Clone(root), MaxSerial: r.MaxSerial, CertSHA256: bytes.Clone(r.CertSHA256)}
	switch {
	case serial > out.MaxSerial:
		out.MaxSerial, out.CertSHA256 = serial, bytes.Clone(hash[:])
	case serial == out.MaxSerial && len(out.CertSHA256) == 0:
		out.CertSHA256 = bytes.Clone(hash[:])
	}
	return out
}

// Equal reports whether a and b hold the same scope.
func (r Ratchet) Equal(o Ratchet) bool {
	return bytes.Equal(r.Root, o.Root) && r.MaxSerial == o.MaxSerial && bytes.Equal(r.CertSHA256, o.CertSHA256)
}

// WallMono is the Worker's time source for certificate expiry (section 11.3).
// Wall is the wall clock (it can jump), Mono a monotonic reading as a duration
// since a fixed origin (it cannot). Tests inject a fake so that both can be
// moved independently.
type WallMono interface {
	Wall() time.Time
	Mono() time.Duration
}

// SystemWallMono is the WallMono backed by the time package.
var SystemWallMono WallMono = systemWallMono{}

var monoBase = time.Now()

type systemWallMono struct{}

func (systemWallMono) Wall() time.Time     { return time.Now() }
func (systemWallMono) Mono() time.Duration { return time.Since(monoBase) }

// Expiry is the end of one connection, fixed at the window check of the
// handshake (section 11.3): the certificate's not_after on the wall clock and
// the same instant on the monotonic clock, computed when the certificate was
// accepted. No allowance is added after not_after.
type Expiry struct {
	// WallMs is not_after in Unix milliseconds.
	WallMs int64
	// DeadlineMono is mono0 + (not_after - now_ms).
	DeadlineMono time.Duration
}

// Expired reports whether the connection is over: now_wall >= expiry_wall OR
// mono_now >= deadline_mono. Equal counts as expired on both clocks. Both
// clocks are read on every call, and the wall-clock test does not depend on
// the monotonic one, so a wall clock that jumps to exactly not_after closes the
// session before the monotonic deadline. A wall clock moved backwards cannot
// extend the session beyond the monotonic deadline.
func (e Expiry) Expired(c WallMono) bool {
	if c == nil {
		c = SystemWallMono
	}
	wall, mono := c.Wall(), c.Mono()
	return wall.UnixMilli() >= e.WallMs || mono >= e.DeadlineMono
}

// newExpiry fixes the expiry of cert, which passed the window check at nowMs
// (read back to back with mono0).
func newExpiry(cert *identity.HubCert, nowMs int64, mono0 time.Duration) (Expiry, error) {
	rem := cert.NotAfterMs - nowMs
	// The window check bounds rem by the validity plus the tolerance; the
	// guards keep the arithmetic safe for every caller.
	const maxMs = int64(math.MaxInt64/int64(time.Millisecond)) / 2
	if rem < 0 || rem > maxMs {
		return Expiry{}, identity.ErrCertMalformed
	}
	d := time.Duration(rem) * time.Millisecond
	if mono0 > math.MaxInt64-d {
		return Expiry{}, identity.ErrCertMalformed
	}
	return Expiry{WallMs: cert.NotAfterMs, DeadlineMono: mono0 + d}, nil
}

// HubAuth is what the Worker knows once the Hub is authenticated (certificate
// chain of trust, window and sig_hub verified, no serial refusal pending) and
// before it sends sig_worker.
type HubAuth struct {
	// Mode is the handshake mode.
	Mode Mode
	// Root is the candidate root (the pin in connect mode when no chain was
	// needed; in the enrollment modes the claimed root).
	Root ed25519.PublicKey
	// Pin is the pin the Worker would store.
	Pin identity.Pin
	// Cert is the validated certificate and CertSHA256 its hash.
	Cert       *identity.HubCert
	CertSHA256 [32]byte
	// Ratchet is the certificate memory after this connection (scoped to
	// Root); RatchetChanged tells whether it differs from the configured one.
	Ratchet        Ratchet
	RatchetChanged bool
	// Expiry is the end of this connection.
	Expiry Expiry
}

func certHash(c *identity.HubCert) [32]byte { return sha256.Sum256(c.Bytes()) }
