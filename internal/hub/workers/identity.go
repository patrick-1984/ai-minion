package workers

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/hub/hubid"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// IdentityFile is the name of the plain Hub root key file of a 0.6 Hub. A 0.7
// Hub keeps only a sealed root (root.key.age) once the passphrase is set; this
// file is read only to print the fingerprint of a Hub that has not been
// migrated yet ([ReadIdentity]).
const IdentityFile = "identity.key"

// IdentitySource is what the Worker side needs from the Hub identity service
// (hubid.Service implements it): the snapshot that a handshake takes once,
// the registry admission check of the activation fence (design 9.6), the hook
// that closes all sessions after an activation, and the status that names the
// root fingerprint also while the Hub does not serve.
type IdentitySource interface {
	Current() (hubid.Snapshot, error)
	Admit(gen uint64) bool
	SetSessionCloser(f func())
	Status(ctx context.Context) (hubid.Status, error)
}

var _ IdentitySource = (*hubid.Service)(nil)

// Identity is the Hub's two-tier identity as the Worker side sees it: a ROOT
// key (the Workers' pin, never used for a handshake signature) and an
// OPERATIONAL key with its certificate, which signs sig_hub, the pairing
// confirmation and every revoke. It has no key material of its own; every
// reader goes through [Identity.Snapshot], which takes the pair and the
// activation generation together.
type Identity struct {
	src IdentitySource
}

// NewIdentity wraps an identity source.
func NewIdentity(src IdentitySource) *Identity { return &Identity{src: src} }

// Snapshot returns the (root, operational key, certificate, generation) set a
// connection uses. It fails while the Hub does not serve: no certificate yet
// (states B0 to B2), or the certificate has expired.
func (i *Identity) Snapshot() (hubid.Snapshot, error) { return i.src.Current() }

// Admit is the registry check of the activation fence: a connection that took
// its snapshot at generation gen may register only while gen is current.
func (i *Identity) Admit(gen uint64) bool { return i.src.Admit(gen) }

// Public returns the ROOT public key, or nil while the Hub does not serve.
func (i *Identity) Public() ed25519.PublicKey {
	sn, err := i.src.Current()
	if err != nil {
		return nil
	}
	return sn.RootPub
}

// Fingerprint returns the fingerprint of the ROOT key, which is also what an
// invitation key carries. It is empty while the Hub has no root yet (B0).
func (i *Identity) Fingerprint() identity.Fingerprint {
	st, err := i.src.Status(context.Background())
	if err != nil || st.RootFingerprint == "" {
		return ""
	}
	f, err := identity.ParseFingerprint(st.RootFingerprint)
	if err != nil {
		return ""
	}
	return f
}

// ReadIdentity loads the identity key of a data directory without creating
// anything. It returns an error that wraps [fs.ErrNotExist] when the Hub has
// not started yet.
func ReadIdentity(dir string) (*Identity, error) {
	key, err := identity.LoadKey(filepath.Join(dir, IdentityFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("workers: no Hub identity in %s yet; it is created when the owner sets the Hub passphrase (minion hub identity set-passphrase, or the banner in the web UI): %w", dir, err)
		}
		return nil, err
	}
	return &Identity{src: plainSource{key: key}}, nil
}

// plainSource backs the Identity that ReadIdentity returns: only the
// fingerprint of a plain root file, no operational key.
type plainSource struct{ key *identity.Key }

func (plainSource) Current() (hubid.Snapshot, error) {
	return hubid.Snapshot{}, errors.New("workers: a plain identity file has no certificate")
}
func (plainSource) Admit(uint64) bool       { return false }
func (plainSource) SetSessionCloser(func()) {}
func (p plainSource) Status(context.Context) (hubid.Status, error) {
	return hubid.Status{RootFingerprint: p.key.Fingerprint().String()}, nil
}
