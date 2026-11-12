package hubid

import (
	"golang.org/x/crypto/scrypt"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// Sealer seals and opens the root key file. The default [AgeSealer] is the
// implementation of the contract (age, one scrypt recipient, work factor 18);
// tests replace it with a fast fake, because each real derivation costs about
// a second and 256 MiB.
type Sealer interface {
	// Seal encrypts the root with the given seed under passphrase.
	Seal(seed []byte, passphrase string) ([]byte, error)
	// Reseal encrypts the key held by signer under a new passphrase.
	Reseal(signer *identity.RootSigner, passphrase string) ([]byte, error)
	// Open decrypts sealed. It returns identity.ErrWrongPassphrase for a
	// wrong passphrase and identity.ErrSealedRootPayload for a defect found
	// after the key derivation ran (header MAC, payload, plaintext size);
	// both did the work. Every other defect (not an age file, not exactly one
	// scrypt recipient at exactly work factor 18, too large, passphrase too
	// short) returns identity.ErrSealedRootInvalid before any derivation, and
	// the service then runs one bounded dummy derivation (section 10.5):
	// exactly one derivation happens in every attempt.
	Open(sealed []byte, passphrase string) (*identity.RootSigner, error)
	// Dummy runs one bounded derivation at the current work factor over the
	// passphrase with a fixed salt, to equalize the time of an early failure.
	Dummy(passphrase string)
}

// AgeSealer is the production [Sealer].
type AgeSealer struct{}

// Seal implements Sealer.
func (AgeSealer) Seal(seed []byte, passphrase string) ([]byte, error) {
	return identity.SealRootSeed(seed, passphrase)
}

// Reseal implements Sealer.
func (AgeSealer) Reseal(signer *identity.RootSigner, passphrase string) ([]byte, error) {
	return signer.Seal(passphrase)
}

// Open implements Sealer.
func (AgeSealer) Open(sealed []byte, passphrase string) (*identity.RootSigner, error) {
	return identity.OpenSealedRootSigner(sealed, passphrase)
}

var dummySalt = []byte("minion-hub-dummy-derivation-salt")

// Dummy implements Sealer: scrypt N=2^18, r=8, p=1, the age cost at work factor 18.
func (AgeSealer) Dummy(passphrase string) {
	_, _ = scrypt.Key([]byte(passphrase), dummySalt, 1<<identity.SealedRootWorkFactor, 8, 1, 32)
}
