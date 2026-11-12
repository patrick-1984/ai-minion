// Package hubidtest builds Hub identities for tests: a fast stand-in for the
// age sealer (each real derivation costs about a second and 256 MiB), a helper
// that brings a data directory to the established state B3 with a valid
// certificate, and a static identity source for tests that need a certified
// Hub without a database. It is test support only.
package hubidtest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubstore"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/testutil/hubcert"
)

// Passphrase is the passphrase of every identity these helpers set up.
const Passphrase = "correct horse battery staple"

// Actor is the actor of the set-up operations.
var Actor = hubid.Actor{ID: "hubidtest", Channel: hubid.ChannelUI}

// Sealer is a fast fake of the age scrypt sealer: the file is "FKS1", the
// SHA-256 of the passphrase and the seed. It is not secret; it only has to
// open with the passphrase it was sealed with.
type Sealer struct {
	mu    sync.Mutex
	seeds map[string][]byte
}

// NewSealer returns a fast sealer.
func NewSealer() *Sealer { return &Sealer{seeds: map[string][]byte{}} }

var _ hubid.Sealer = (*Sealer)(nil)

// Seal implements hubid.Sealer.
func (f *Sealer) Seal(seed []byte, pass string) ([]byte, error) {
	h := sha256.Sum256([]byte(pass))
	k, _ := identity.KeyFromSeed(seed)
	f.mu.Lock()
	f.seeds[string(k.Public())] = bytes.Clone(seed)
	f.mu.Unlock()
	return append(append([]byte("FKS1"), h[:]...), seed...), nil
}

// Reseal implements hubid.Sealer.
func (f *Sealer) Reseal(s *identity.RootSigner, pass string) ([]byte, error) {
	f.mu.Lock()
	seed := f.seeds[string(s.Public())]
	f.mu.Unlock()
	return f.Seal(seed, pass)
}

// Open implements hubid.Sealer.
func (f *Sealer) Open(sealed []byte, pass string) (*identity.RootSigner, error) {
	if len(sealed) != 4+32+32 || string(sealed[:4]) != "FKS1" {
		return nil, identity.ErrSealedRootInvalid
	}
	h := sha256.Sum256([]byte(pass))
	if !bytes.Equal(sealed[4:36], h[:]) {
		return nil, identity.ErrWrongPassphrase
	}
	k, _ := identity.KeyFromSeed(sealed[36:])
	f.mu.Lock()
	f.seeds[string(k.Public())] = bytes.Clone(sealed[36:])
	f.mu.Unlock()
	return identity.RootSignerFromSeed(sealed[36:])
}

// Dummy implements hubid.Sealer.
func (f *Sealer) Dummy(string) {}

// Option adjusts the hubid options of Open.
type Option func(*hubid.Options)

// WithNow sets the wall clock of the identity service.
func WithNow(now func() time.Time) Option { return func(o *hubid.Options) { o.Now = now } }

// WithSealer sets the sealer (Open uses a fresh NewSealer by default).
func WithSealer(s hubid.Sealer) Option { return func(o *hubid.Options) { o.Sealer = s } }

// WithOptions edits the options directly.
func WithOptions(f func(*hubid.Options)) Option { return func(o *hubid.Options) { f(o) } }

// Open starts a Hub identity service on dir and st with the fast sealer. When
// the data directory is pristine (B0) it sets the passphrase, which issues the
// first certificate and leaves the service established (B3) and serving.
func Open(t testing.TB, st *store.Store, dir string, opts ...Option) *hubid.Service {
	t.Helper()
	o := hubid.Options{DataDir: dir, Store: st, Sealer: NewSealer(), LockWait: 2 * time.Second}
	for _, f := range opts {
		f(&o)
	}
	svc, err := hubid.Open(context.Background(), o)
	if err != nil {
		t.Fatalf("hubidtest: open the Hub identity: %v", err)
	}
	if svc.State() == hubid.StatePristine {
		if _, err := svc.SetPassphrase(context.Background(), Actor, Passphrase, Passphrase, hubid.IssueOptions{}); err != nil {
			t.Fatalf("hubidtest: set the passphrase: %v", err)
		}
	}
	return svc
}

// Provision brings the data directory dataDir to the established state B3
// (sealed root, operational key, valid certificate, 180 days) and closes
// everything again, so that a Hub started on the directory finds a serving
// identity and needs no passphrase. It returns the root public key (the pin of
// a Worker). The sealed file is a fake: it can be opened
// only by a service that uses a hubidtest sealer, which the production start
// never does.
func Provision(t testing.TB, dataDir string, opts ...Option) ed25519.PublicKey {
	t.Helper()
	st, err := hubstore.Open(t, dataDir)
	if err != nil {
		t.Fatalf("hubidtest: open the store: %v", err)
	}
	defer func() { _ = st.Close() }()
	sn, err := Open(t, st, dataDir, opts...).Current()
	if err != nil {
		t.Fatalf("hubidtest: the provisioned Hub does not serve: %v", err)
	}
	return sn.RootPub
}

// Static is an identity source with a fixed certified Hub and no database. It
// satisfies workers.IdentitySource. Reissue and Rotate replace the Hub and
// raise the generation the way an activation does, and call the registered
// session closer.
type Static struct {
	mu     sync.Mutex
	hub    *hubcert.Hub
	gen    uint64
	closer func()
	now    func() time.Time
}

// NewStatic returns a source for h.
func NewStatic(h *hubcert.Hub) *Static { return &Static{hub: h, gen: 1, now: time.Now} }

// SetNow sets the clock of the expiry check.
func (s *Static) SetNow(now func() time.Time) { s.mu.Lock(); s.now = now; s.mu.Unlock() }

// Hub returns the current Hub.
func (s *Static) Hub() *hubcert.Hub { s.mu.Lock(); defer s.mu.Unlock(); return s.hub }

// Current implements the identity source.
func (s *Static) Current() (hubid.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.now().UnixMilli() >= s.hub.Cert.NotAfterMs {
		return hubid.Snapshot{}, &hubid.Error{Code: hubid.CodeNotServing, Msg: "the Hub certificate has expired", Err: identity.ErrCertExpired}
	}
	return hubid.Snapshot{RootPub: s.hub.RootPub(), OpKey: s.hub.Op, Cert: s.hub.Cert, Generation: s.gen}, nil
}

// Admit implements the identity source.
func (s *Static) Admit(gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen == gen && s.now().UnixMilli() < s.hub.Cert.NotAfterMs
}

// SetSessionCloser implements the identity source.
func (s *Static) SetSessionCloser(f func()) { s.mu.Lock(); s.closer = f; s.mu.Unlock() }

// Status implements the identity source (the root fingerprint only).
func (s *Static) Status(context.Context) (hubid.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return hubid.Status{RootFingerprint: s.hub.Root.Fingerprint().String(), Generation: s.gen}, nil
}

// Activate swaps in a new Hub (a re-issue under the same root, or a rotation to
// another root), raises the generation and closes all sessions: swap first,
// close second, as hubid does.
func (s *Static) Activate(h *hubcert.Hub) {
	s.mu.Lock()
	s.hub = h
	s.gen++
	closer := s.closer
	s.mu.Unlock()
	if closer != nil {
		closer()
	}
}

// Swap replaces the Hub and raises the generation WITHOUT closing the
// sessions: the state between steps 6 and 8 of the activation, for fence tests.
func (s *Static) Swap(h *hubcert.Hub) {
	s.mu.Lock()
	s.hub = h
	s.gen++
	s.mu.Unlock()
}

// CloseSessions calls the registered session closer (step 8).
func (s *Static) CloseSessions() {
	s.mu.Lock()
	closer := s.closer
	s.mu.Unlock()
	if closer != nil {
		closer()
	}
}
