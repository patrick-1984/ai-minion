package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"time"
)

// Helpers for the Hub identity service (I2). They keep the root private key
// inside a RootSigner: a caller that needs to create, seal or sign with a root
// never holds an ed25519.PrivateKey or a KeyFromSeed closure.

// NewRootSeed returns a fresh random 32-byte root seed. The caller wipes it
// (WipeBytes) as soon as it was sealed and given to RootSignerFromSeed.
func NewRootSeed() ([]byte, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := io.ReadFull(rand.Reader, seed); err != nil {
		return nil, err
	}
	return seed, nil
}

// RootSignerFromSeed builds a RootSigner from a 32-byte seed. The signer
// copies the seed into storage it owns; the caller still wipes its own copy.
func RootSignerFromSeed(seed []byte) (*RootSigner, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, newErr(kindInvalidArgument, "seed must be 32 bytes")
	}
	return &RootSigner{priv: ed25519.NewKeyFromSeed(seed)}, nil
}

// SealRootSeed seals the root with the given seed; see SealRootKey.
func SealRootSeed(seed []byte, passphrase string) ([]byte, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, newErr(kindInvalidArgument, "seed must be 32 bytes")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	defer WipeBytes(priv)
	return SealRootKey(priv, passphrase)
}

// Seal seals the signer's key under passphrase (a passphrase change); the key
// does not leave the signer. It returns ErrSignerDestroyed after Destroy.
func (s *RootSigner) Seal(passphrase string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.priv == nil {
		return nil, ErrSignerDestroyed
	}
	return SealRootKey(s.priv, passphrase)
}

// maxKeyFile bounds what is read from a plain key file.
const maxKeyFile = 4096

// ReadKeyFileSeed reads the seed of a plain key file written by SaveKey (the
// 0.6 identity.key), with the checks of LoadKey. The file is read into one
// fixed buffer and parsed in place, so no string or growing copy of the
// secret is made; the buffer is wiped before return and the caller wipes the
// seed.
func ReadKeyFileSeed(path string) ([]byte, error) {
	f, err := OpenPrivate(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, maxKeyFile+1)
	defer WipeBytes(buf)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if n > maxKeyFile {
		return nil, newErr(kindInvalidArgument, "not a Minion identity key file")
	}
	enc, ok := bytes.CutPrefix(bytes.TrimSpace(buf[:n]), []byte(keyFileHeader))
	if !ok {
		return nil, newErr(kindInvalidArgument, "not a Minion identity key file")
	}
	seed := make([]byte, b64.DecodedLen(len(enc)))
	m, err := b64.Decode(seed, enc)
	if err != nil || m != ed25519.SeedSize || len(enc) != b64.EncodedLen(ed25519.SeedSize) {
		WipeBytes(seed)
		return nil, newErr(kindInvalidArgument, "corrupt identity key file")
	}
	return seed[:m], nil
}

// PlainKeyPublic returns the public key of a plain key file without ever
// building a Key: the seed is read into a wipeable buffer, handed to a
// RootSigner, wiped, and the signer is destroyed before return. observe, when
// not nil, sees the signer before it is destroyed (tests assert destruction).
func PlainKeyPublic(path string, observe func(*RootSigner)) (ed25519.PublicKey, error) {
	seed, err := ReadKeyFileSeed(path)
	if err != nil {
		return nil, err
	}
	defer WipeBytes(seed)
	signer, err := RootSignerFromSeed(seed)
	if err != nil {
		return nil, err
	}
	defer signer.Destroy()
	if observe != nil {
		observe(signer)
	}
	return signer.Public(), nil
}

// TransitionSigner signs a transition record; *RootSigner implements it.
type TransitionSigner interface {
	Public() ed25519.PublicKey
	Sign(p Purpose, data []byte) ([]byte, error)
}

// NewTransitionBy creates and signs the record that hands over from the
// signer's key to newPub as link seq of the chain, in the format of
// NewTransition.
func NewTransitionBy(old TransitionSigner, newPub ed25519.PublicKey, seq uint64, createdAt time.Time) (Transition, error) {
	if len(newPub) != ed25519.PublicKeySize {
		return Transition{}, newErr(kindInvalidArgument, "new public key must be 32 bytes")
	}
	t := Transition{
		Seq:       seq,
		OldPub:    B64(old.Public()),
		NewPub:    B64(newPub),
		CreatedAt: createdAt.Truncate(time.Millisecond).UTC(),
	}
	sig, err := old.Sign(PurposeTransition, t.signedData())
	if err != nil {
		return Transition{}, err
	}
	t.SigByOld = sig
	return t, nil
}
