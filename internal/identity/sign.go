package identity

import (
	"crypto/ed25519"
)

// Purpose tags a signature so that it is valid for exactly one use (AD-5).
type Purpose string

// The accepted signature purposes.
const (
	PurposeHandshakeHub    Purpose = "hs-hub"
	PurposeHandshakeWorker Purpose = "hs-worker"
	PurposeTransition      Purpose = "transition"
	PurposeRevoke          Purpose = "revoke"
	PurposePAKEConfirm     Purpose = "pake-confirm"
	PurposeHubCert         Purpose = "hub-cert"
)

const signPrefix = "minion/v1/"

// Valid reports whether p is one of the accepted purposes.
func (p Purpose) Valid() bool {
	switch p {
	case PurposeHandshakeHub, PurposeHandshakeWorker, PurposeTransition, PurposeRevoke, PurposePAKEConfirm, PurposeHubCert:
		return true
	}
	return false
}

// SignedMessage returns the exact bytes that are signed:
// "minion/v1/" + purpose + 0x00 + data.
func SignedMessage(p Purpose, data []byte) ([]byte, error) {
	if !p.Valid() {
		return nil, newErr(kindInvalidArgument, "unknown signature purpose")
	}
	msg := make([]byte, 0, len(signPrefix)+len(p)+1+len(data))
	msg = append(msg, signPrefix...)
	msg = append(msg, p...)
	msg = append(msg, 0)
	msg = append(msg, data...)
	return msg, nil
}

// Sign signs data for purpose p with priv.
func Sign(priv ed25519.PrivateKey, p Purpose, data []byte) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, newErr(kindInvalidArgument, "private key must be 64 bytes")
	}
	msg, err := SignedMessage(p, data)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, msg), nil
}

// Verify checks sig over data for purpose p. A signature made for another
// purpose never verifies. It returns ErrInvalidArgument for a malformed
// key or purpose and ErrUnauthenticated for a wrong signature.
func Verify(pub ed25519.PublicKey, p Purpose, data, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return newErr(kindInvalidArgument, "public key must be 32 bytes")
	}
	msg, err := SignedMessage(p, data)
	if err != nil {
		return err
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, msg, sig) {
		return newErr(kindUnauthenticated, "signature does not verify")
	}
	return nil
}
