package transport

import (
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// tHub is a Hub identity for protocol 3.0 tests: a root key, an operational
// key and the certificate the root issued for it.
type tHub struct {
	t      *testing.T
	root   *identity.Key
	op     *identity.Key
	cert   *identity.HubCert
	signer *identity.RootSigner
}

// newHub returns a Hub with a fresh root and a valid certificate (serial 1,
// from one hour ago to 180 days ahead).
func newHub(t *testing.T) *tHub {
	t.Helper()
	seed, err := identity.NewRootSeed()
	if err != nil {
		t.Fatal(err)
	}
	return newHubFromSeed(t, seed)
}

func newHubFromSeed(t *testing.T, seed []byte) *tHub {
	t.Helper()
	signer, err := identity.RootSignerFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(signer.Destroy)
	root, err := identity.KeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	h := &tHub{t: t, root: root, signer: signer}
	h.reissue(1)
	return h
}

func window() (nb, na int64) {
	now := time.Now()
	return now.Add(-time.Hour).UnixMilli(), now.Add(180 * 24 * time.Hour).UnixMilli()
}

// reissue makes a new operational key and certificate with the given serial.
func (h *tHub) reissue(serial uint64) *identity.HubCert {
	h.t.Helper()
	nb, na := window()
	return h.reissueWindow(serial, nb, na)
}

func (h *tHub) reissueWindow(serial uint64, nb, na int64) *identity.HubCert {
	h.t.Helper()
	op, err := identity.GenerateKey()
	if err != nil {
		h.t.Fatal(err)
	}
	cert, err := h.signer.IssueHubCert(op.Public(), serial, nb, na)
	if err != nil {
		h.t.Fatal(err)
	}
	h.op, h.cert = op, cert
	return cert
}

func (h *tHub) cfg() HubConfig { return HubConfig{Key: h.op, Cert: h.cert} }
