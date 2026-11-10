package workers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// IdentityFile is the name of the Hub identity key file in the data directory.
const IdentityFile = "identity.key"

// IdentityRepository is the port to the hub_identity row; *store.Store
// implements it.
type IdentityRepository interface {
	HubIdentity(ctx context.Context) (store.HubIdentityRow, bool, error)
	PutHubIdentity(ctx context.Context, row store.HubIdentityRow) error
}

// Identity is the Hub's permanent Ed25519 identity (AD-5). It is separate from
// any TLS key, never expires and never changes on restart, certificate
// rotation or restore.
//
// Only a planned rotation (Story 4.9, [Rotator]) replaces the key of a running
// Hub; every reader goes through [Identity.Key] and sees the new key from the
// next call on.
type Identity struct {
	mu      sync.RWMutex // guards key, which only a rotation replaces
	key     *identity.Key
	created bool
}

// Key returns the identity key. It signs the handshake and revocations; never
// log it (its String form is only the fingerprint).
func (i *Identity) Key() *identity.Key {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.key
}

// swap makes key the identity of the running Hub (a completed rotation).
func (i *Identity) swap(key *identity.Key) {
	i.mu.Lock()
	i.key = key
	i.mu.Unlock()
}

// Public returns the public key.
func (i *Identity) Public() ed25519.PublicKey { return i.Key().Public() }

// Fingerprint returns the 52-character base32 fingerprint of the public key.
func (i *Identity) Fingerprint() identity.Fingerprint { return i.Key().Fingerprint() }

// Created reports whether this call to [LoadOrCreateIdentity] generated the
// key (the first start of this Hub).
func (i *Identity) Created() bool { return i.created }

// NewIdentity wraps an existing key; it is meant for tests and for the later
// rotation story.
func NewIdentity(key *identity.Key) *Identity { return &Identity{key: key} }

// LoadOrCreateIdentity returns the Hub identity of the data directory.
//
// The first start generates the key and writes <dir>/identity.key with mode
// 0600 (a protected DACL on Windows), then records its public key in the
// hub_identity row. Every later start loads the file and checks it against the
// row. It refuses, with a message that says what to restore, a missing key
// file next to a recorded identity and a key file that differs from the
// recorded one: replacing the identity would strand every enrolled Worker, so
// it is never done silently (AD-5: a compromised key is replaced by a new Hub
// identity as a deliberate act, not by a missing file).
func LoadOrCreateIdentity(ctx context.Context, dir string, repo IdentityRepository, log *slog.Logger) (*Identity, error) {
	if log == nil {
		log = slog.Default()
	}
	path := filepath.Join(dir, IdentityFile)
	// A rotation that was interrupted between its database commit and the
	// rename of the new key file is completed first (Story 4.9).
	if err := recoverRotation(ctx, dir, repo, log); err != nil {
		return nil, err
	}
	recorded, haveRow, err := repo.HubIdentity(ctx)
	if err != nil {
		return nil, err
	}

	key, err := identity.LoadKey(path)
	switch {
	case err == nil:
		if haveRow && !bytes.Equal(recorded.Pub, key.Public()) {
			return nil, fmt.Errorf("workers: %s holds the key with fingerprint %s but this Hub's database records the identity %s; "+
				"restore the %s that belongs to this database (the Hub identity is permanent and is never replaced silently)",
				path, key.Fingerprint(), recorded.Fingerprint, IdentityFile)
		}
		if !haveRow {
			// A key file without a record: a restore of the file alone, or a
			// start that was interrupted between the two writes.
			if err := record(ctx, repo, key); err != nil {
				return nil, err
			}
		}
		log.Info("hub identity", "fingerprint", key.Fingerprint().String())
		return &Identity{key: key}, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("workers: load the Hub identity %s: %w", path, err)
	}

	if haveRow {
		return nil, fmt.Errorf("workers: %s is missing but this Hub's database records the identity %s; "+
			"restore %s from your backup; a new key would strand every enrolled Worker, so none is generated",
			path, recorded.Fingerprint, IdentityFile)
	}
	key, err = identity.GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("workers: generate the Hub identity: %w", err)
	}
	won, err := createKeyFile(path, key)
	if err != nil {
		return nil, fmt.Errorf("workers: write the Hub identity %s: %w", path, err)
	}
	if !won {
		// Another initializer of the same data directory created the file
		// between our check and our write. Its key is the identity; ours is
		// dropped and never written anywhere.
		key, err = identity.LoadKey(path)
		if err != nil {
			return nil, fmt.Errorf("workers: load the Hub identity %s: %w", path, err)
		}
	}
	if err := record(ctx, repo, key); err != nil {
		return nil, err
	}
	if !won {
		log.Info("hub identity", "fingerprint", key.Fingerprint().String())
		return &Identity{key: key}, nil
	}
	log.Info("hub identity created", "fingerprint", key.Fingerprint().String(), "file", path)
	return &Identity{key: key, created: true}, nil
}

// createKeyFile writes key to path only if path does not exist, and reports
// whether it did. The key is first written, synced and private, to a
// temporary file in the same directory and then hard-linked to path: the link
// fails with an "exists" error when another initializer won, so an existing
// identity.key is never overwritten (a rename would replace it) and a reader
// never sees a partial file. Where the file system has no hard links, it
// falls back to a check and a rename.
func createKeyFile(path string, key *identity.Key) (bool, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return false, err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".init-"+hex.EncodeToString(rnd[:]))
	if err := identity.SaveKey(tmp, key); err != nil {
		return false, err
	}
	defer func() { _ = os.Remove(tmp) }()
	err := os.Link(tmp, path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrExist):
		return false, nil
	}
	// No hard links here: best effort.
	if _, statErr := os.Lstat(path); statErr == nil {
		return false, nil
	}
	if renameErr := os.Rename(tmp, path); renameErr != nil {
		return false, fmt.Errorf("%w (and no hard link: %v)", renameErr, err)
	}
	return true, nil
}

// record stores the public key in the hub_identity row. When another
// initializer recorded the same key first, that is success; a different
// recorded key is the mismatch error.
func record(ctx context.Context, repo IdentityRepository, key *identity.Key) error {
	err := repo.PutHubIdentity(ctx, store.HubIdentityRow{
		Pub:         bytes.Clone(key.Public()),
		Fingerprint: key.Fingerprint().String(),
		CreatedAt:   time.Now().UnixMilli(),
	})
	if err == nil {
		return nil
	}
	if row, ok, rerr := repo.HubIdentity(ctx); rerr == nil && ok {
		if bytes.Equal(row.Pub, key.Public()) {
			return nil
		}
		return fmt.Errorf("workers: this Hub's database records the identity %s, not %s; "+
			"restore the %s that belongs to this database", row.Fingerprint, key.Fingerprint(), IdentityFile)
	}
	return fmt.Errorf("workers: record the Hub identity: %w", err)
}

// ReadIdentity loads the identity key of a data directory without creating
// anything. It returns an error that wraps [fs.ErrNotExist] when the Hub has
// not started yet.
func ReadIdentity(dir string) (*Identity, error) {
	key, err := identity.LoadKey(filepath.Join(dir, IdentityFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("workers: no Hub identity in %s yet; it is created when the Hub first starts (minion hub serve): %w", dir, err)
		}
		return nil, err
	}
	return &Identity{key: key}, nil
}
