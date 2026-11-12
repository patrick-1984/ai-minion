package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"strconv"
	"sync"
	"unicode/utf8"

	"filippo.io/age"
)

// Sealed root file (owner decision 2026-11-12): the root key is stored as an
// age file with a single scrypt recipient. The plaintext is the 32-byte
// Ed25519 seed. The work factor is fixed, the same as for backups.
const (
	// SealedRootWorkFactor is the scrypt work factor (log2 N) used to seal.
	SealedRootWorkFactor = 18
	// MinPassphraseRunes is the minimum passphrase length in runes
	// (reviewed design revision 4).
	MinPassphraseRunes = 14
	// maxSealedRootFile bounds what is read from disk or memory.
	maxSealedRootFile = 8192
)

func errTooShort() error { return ErrPassphraseTooShort }

// SealRootKey encrypts the root private key with the passphrase. The
// passphrase must have at least MinPassphraseRunes runes. It never stores or
// returns the passphrase.
func SealRootKey(priv ed25519.PrivateKey, passphrase string) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, newErr(kindInvalidArgument, "private key must be 64 bytes")
	}
	if utf8.RuneCountInString(passphrase) < MinPassphraseRunes {
		return nil, errTooShort()
	}
	rcp, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, ErrSealedRootInvalid
	}
	rcp.SetWorkFactor(SealedRootWorkFactor)
	var out bytes.Buffer
	w, err := age.Encrypt(&out, rcp)
	if err != nil {
		return nil, ErrSealedRootInvalid
	}
	seed := priv.Seed()
	defer WipeBytes(seed)
	if _, err := w.Write(seed); err != nil {
		return nil, ErrSealedRootInvalid
	}
	if err := w.Close(); err != nil {
		return nil, ErrSealedRootInvalid
	}
	return out.Bytes(), nil
}

// OpenSealedRootSigner decrypts a file made by SealRootKey and returns an
// operation-scoped signer that owns the only copy of the private key this
// package makes. A wrong passphrase gives ErrWrongPassphrase. A file that is
// not an age file, has not exactly one scrypt recipient with exactly
// SealedRootWorkFactor (a lower factor is refused as firmly as a higher one),
// or is too large gives ErrSealedRootInvalid before any key derivation. A
// defect found after the one derivation (header MAC, payload authentication,
// plaintext size) gives ErrSealedRootPayload, which also matches
// ErrSealedRootInvalid. The errors reveal nothing beyond that. The decrypted
// plaintext and the seed copies are wiped before return. The caller must call
// Destroy as soon as the operation is done.
//
// Go limitation: erasure is best effort. Copies made inside the age and
// scrypt libraries, the Ed25519 implementation, the runtime (stack growth,
// garbage collector moves) and swap cannot be reached or guaranteed erased,
// and the passphrase string is immutable and cannot be wiped at all. The
// signer only shortens the time the key stays readable in the slice it owns.
func OpenSealedRootSigner(sealed []byte, passphrase string) (*RootSigner, error) {
	if utf8.RuneCountInString(passphrase) < MinPassphraseRunes {
		return nil, errTooShort()
	}
	if len(sealed) > maxSealedRootFile {
		return nil, ErrSealedRootInvalid
	}
	inner, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, ErrSealedRootInvalid
	}
	// The wrapper refuses every header but the one the sealer writes before
	// the derivation starts, so a crafted file never costs more (or less)
	// than one derivation at the fixed work factor.
	id := &sealedRootIdentity{inner: inner}
	r, err := age.Decrypt(bytes.NewReader(sealed), id)
	if err != nil {
		var nm *age.NoIdentityMatchError
		switch {
		case errors.As(err, &nm) && id.derived:
			return nil, ErrWrongPassphrase
		case id.derived:
			return nil, ErrSealedRootPayload
		}
		return nil, ErrSealedRootInvalid
	}
	seed, err := io.ReadAll(io.LimitReader(r, ed25519.SeedSize+1))
	defer WipeBytes(seed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, ErrSealedRootPayload
	}
	return &RootSigner{priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// sealedRootIdentity wraps the scrypt identity with the exact-header check
// and records whether the derivation was started.
type sealedRootIdentity struct {
	inner   age.Identity
	derived bool
}

// Unwrap implements age.Identity.
func (w *sealedRootIdentity) Unwrap(stanzas []*age.Stanza) ([]byte, error) {
	if !sealedRootHeaderOK(stanzas) {
		return nil, ErrSealedRootInvalid
	}
	w.derived = true
	return w.inner.Unwrap(stanzas)
}

// sealedRootHeaderOK is true for exactly one scrypt recipient with a 16-byte
// salt, the work factor SealedRootWorkFactor and a 32-byte wrapped file key.
func sealedRootHeaderOK(stanzas []*age.Stanza) bool {
	if len(stanzas) != 1 {
		return false
	}
	st := stanzas[0]
	if st.Type != "scrypt" || len(st.Args) != 2 || len(st.Body) != 32 {
		return false
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(st.Args[0])
	if err != nil || len(salt) != 16 {
		return false
	}
	return st.Args[1] == strconv.Itoa(SealedRootWorkFactor)
}

// RootSigner signs with the sealed root key for the duration of one
// operation. It never exposes the private key. It is safe for concurrent use.
type RootSigner struct {
	mu   sync.Mutex
	priv ed25519.PrivateKey // nil after Destroy
	pub  ed25519.PublicKey
}

// Public returns a copy of the root public key. It stays valid after Destroy.
func (s *RootSigner) Public() ed25519.PublicKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pub == nil && s.priv != nil {
		s.pub = bytes.Clone(s.priv.Public().(ed25519.PublicKey))
	}
	return bytes.Clone(s.pub)
}

// Sign signs data for purpose p with the same domain separation as Sign. It
// returns ErrSignerDestroyed after Destroy.
func (s *RootSigner) Sign(p Purpose, data []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.priv == nil {
		return nil, ErrSignerDestroyed
	}
	return Sign(s.priv, p, data)
}

// IssueHubCert issues a hub certificate signed by the root key; see the
// package-level IssueHubCert.
func (s *RootSigner) IssueHubCert(opPub ed25519.PublicKey, serial uint64, notBeforeMs, notAfterMs int64) (*HubCert, error) {
	pub := s.Public()
	return issueHubCert(pub, s.Sign, opPub, serial, notBeforeMs, notAfterMs)
}

// Destroy zeroes the whole private key slice (best effort, see
// OpenSealedRootSigner) and makes every later Sign fail with
// ErrSignerDestroyed. It is idempotent.
func (s *RootSigner) Destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.priv != nil {
		if s.pub == nil {
			s.pub = bytes.Clone(s.priv.Public().(ed25519.PublicKey))
		}
		clear(s.priv)
		s.priv = nil
	}
}

// SaveSealedRootKey seals priv and writes it to path with WritePrivate
// (atomic, mode 0600 on Unix, owner-only DACL on Windows).
func SaveSealedRootKey(path string, priv ed25519.PrivateKey, passphrase string) error {
	b, err := SealRootKey(priv, passphrase)
	if err != nil {
		return err
	}
	return WritePrivate(path, b)
}

// LoadSealedRootSigner opens path with OpenPrivate (symlink, owner and
// permission checks) and decrypts it with the passphrase; see
// OpenSealedRootSigner.
func LoadSealedRootSigner(path, passphrase string) (*RootSigner, error) {
	f, err := OpenPrivate(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxSealedRootFile+1))
	if err != nil {
		return nil, err
	}
	return OpenSealedRootSigner(raw, passphrase)
}

// WipeBytes overwrites b with zeros. This is best effort only: Go gives no
// guarantee that no other copy exists (the garbage collector may have moved
// the data, the runtime and the scrypt and Ed25519 libraries make internal
// copies, memory may be swapped out), so it only shortens the time the
// secret stays readable in this slice. Use it as ed25519.PrivateKey too:
// WipeBytes(priv).
func WipeBytes(b []byte) {
	clear(b)
}
