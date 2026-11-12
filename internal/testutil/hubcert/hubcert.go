// Package hubcert builds certified Hub identities for tests: a root, an
// operational key and a certificate issued by the root, the way the Hub
// identity service does it for protocol 3.0. It is test support only; the
// production Hub never builds keys this way.
package hubcert

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Hub is a root key, an operational key and the certificate that binds them.
type Hub struct {
	// Root is the root key (the pin of a Worker) and Op the operational key
	// that signs sig_hub and revokes. Cert certifies Op under Root.
	Root *identity.Key
	Op   *identity.Key
	Cert *identity.HubCert

	seed   []byte
	signer *identity.RootSigner
	now    func() time.Time
	nb, na int64
	serial uint64
}

// Option configures New.
type Option func(*Hub)

// WithSerial sets the first certificate's serial (default 1).
func WithSerial(s uint64) Option { return func(h *Hub) { h.serial = s } }

// WithWindow sets not_before and not_after in Unix milliseconds. The default is
// from one hour ago to 180 days ahead of the clock.
func WithWindow(notBeforeMs, notAfterMs int64) Option {
	return func(h *Hub) { h.nb, h.na = notBeforeMs, notAfterMs }
}

// WithNow sets the clock the default window is computed from.
func WithNow(now func() time.Time) Option { return func(h *Hub) { h.now = now } }

// WithRootSeed uses a given 32-byte root seed (for a second Hub that shares a
// root, or a root rotation test).
func WithRootSeed(seed []byte) Option { return func(h *Hub) { h.seed = append([]byte(nil), seed...) } }

// New returns a Hub with a fresh root, a fresh operational key and a valid
// certificate.
func New(t testing.TB, opts ...Option) *Hub {
	t.Helper()
	h := &Hub{now: time.Now, serial: 1}
	for _, o := range opts {
		o(h)
	}
	if h.seed == nil {
		seed, err := identity.NewRootSeed()
		if err != nil {
			t.Fatal(err)
		}
		h.seed = seed
	}
	signer, err := identity.RootSignerFromSeed(h.seed)
	if err != nil {
		t.Fatal(err)
	}
	h.signer = signer
	root, err := identity.KeyFromSeed(h.seed)
	if err != nil {
		t.Fatal(err)
	}
	h.Root = root
	t.Cleanup(signer.Destroy)
	h.Reissue(t, h.serial)
	return h
}

// RootPub returns the root public key.
func (h *Hub) RootPub() ed25519.PublicKey { return h.Root.Public() }

// Reissue makes a new operational key and a new certificate with the given
// serial under the same root, with the Hub's window.
func (h *Hub) Reissue(t testing.TB, serial uint64) *identity.HubCert {
	t.Helper()
	op, err := identity.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return h.certify(t, op, serial)
}

// ReissueWithKey certifies a given operational key (a "stolen previous key"
// test certifies it under a new serial).
func (h *Hub) ReissueWithKey(t testing.TB, op *identity.Key, serial uint64) *identity.HubCert {
	t.Helper()
	return h.certify(t, op, serial)
}

func (h *Hub) certify(t testing.TB, op *identity.Key, serial uint64) *identity.HubCert {
	t.Helper()
	nb, na := h.nb, h.na
	if nb == 0 && na == 0 {
		now := h.now()
		nb = now.Add(-time.Hour).UnixMilli()
		na = now.Add(180 * 24 * time.Hour).UnixMilli()
	}
	cert, err := h.signer.IssueHubCert(op.Public(), serial, nb, na)
	if err != nil {
		t.Fatal(err)
	}
	h.Op, h.Cert = op, cert
	return cert
}

// IssueAt issues a certificate for the current operational key with its own
// window and serial, without changing h.
func (h *Hub) IssueAt(t testing.TB, serial uint64, notBeforeMs, notAfterMs int64) *identity.HubCert {
	t.Helper()
	cert, err := h.signer.IssueHubCert(h.Op.Public(), serial, notBeforeMs, notAfterMs)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// Config returns the HubConfig for this Hub (operational key and certificate).
func (h *Hub) Config() transport.HubConfig {
	return transport.HubConfig{Key: h.Op, Cert: h.Cert}
}

// Next returns a new Hub under the SAME root with a fresh operational key and a
// certificate with the given serial: the Hub after a re-issue, as a separate
// value, so that a test can hand it to a running source while the old value
// stays readable.
func (h *Hub) Next(t testing.TB, serial uint64) *Hub {
	t.Helper()
	return New(t, WithRootSeed(h.seed), WithSerial(serial), WithWindow(h.nb, h.na), WithNow(h.now))
}
