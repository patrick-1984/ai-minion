package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"time"
)

// Transition is one link of the Hub key transition chain (AD-5): the old key
// hands over to the new key and signs the record with purpose "transition".
// Binary fields are base64url without padding in JSON. CreatedAt is a UTC
// instant with millisecond precision: in JSON it is an RFC 3339 UTC string
// with exactly three fractional digits (for example
// "2023-11-14T22:13:20.000Z"), and in the signed data it is the int64 Unix
// milliseconds of that instant.
type Transition struct {
	Seq       uint64    `json:"seq"`
	OldPub    B64       `json:"old_pub"`
	NewPub    B64       `json:"new_pub"`
	CreatedAt time.Time `json:"created_at"`
	SigByOld  B64       `json:"sig_by_old"`
}

// createdAtLayout is the JSON form of Transition.CreatedAt: RFC 3339, UTC,
// exactly three fractional digits.
const createdAtLayout = "2006-01-02T15:04:05.000Z"

// transitionJSON is the wire form of Transition.
type transitionJSON struct {
	Seq       uint64 `json:"seq"`
	OldPub    B64    `json:"old_pub"`
	NewPub    B64    `json:"new_pub"`
	CreatedAt string `json:"created_at"`
	SigByOld  B64    `json:"sig_by_old"`
}

// MarshalJSON implements json.Marshaler. created_at is written as an RFC 3339
// UTC string with exactly three fractional digits.
func (t Transition) MarshalJSON() ([]byte, error) {
	return json.Marshal(transitionJSON{
		Seq:       t.Seq,
		OldPub:    t.OldPub,
		NewPub:    t.NewPub,
		CreatedAt: t.CreatedAt.UTC().Format(createdAtLayout),
		SigByOld:  t.SigByOld,
	})
}

// UnmarshalJSON implements json.Unmarshaler. created_at is an RFC 3339 string;
// any UTC offset is accepted and the value is converted to UTC, but a
// fraction finer than one millisecond is rejected because the signature
// covers milliseconds only.
func (t *Transition) UnmarshalJSON(data []byte) error {
	var w transitionJSON
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339Nano, w.CreatedAt)
	if err != nil || at.Nanosecond()%int(time.Millisecond) != 0 {
		return newErr(kindInvalidArgument, "transition created_at must be an RFC 3339 timestamp with millisecond precision")
	}
	*t = Transition{
		Seq:       w.Seq,
		OldPub:    w.OldPub,
		NewPub:    w.NewPub,
		CreatedAt: at.UTC(),
		SigByOld:  w.SigByOld,
	}
	return nil
}

// signedData is the data signed by the old key, 80 bytes in this order:
//
//	seq        8 bytes, big endian uint64
//	old_pub   32 bytes
//	new_pub   32 bytes
//	created_at 8 bytes, big endian int64, Unix milliseconds
//
// The offset of created_at does not matter: only the instant is signed.
func (t Transition) signedData() []byte {
	b := make([]byte, 0, 8+2*ed25519.PublicKeySize+8)
	b = binary.BigEndian.AppendUint64(b, t.Seq)
	b = append(b, t.OldPub...)
	b = append(b, t.NewPub...)
	b = binary.BigEndian.AppendUint64(b, uint64(t.CreatedAt.UnixMilli()))
	return b
}

// NewTransition creates and signs the record that hands over from old to
// newPub as link seq of the chain. createdAt is truncated to milliseconds and
// converted to UTC.
func NewTransition(old *Key, newPub ed25519.PublicKey, seq uint64, createdAt time.Time) (Transition, error) {
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

// Verify checks the structure of the record and its signature by the old key.
func (t Transition) Verify() error {
	if len(t.OldPub) != ed25519.PublicKeySize || len(t.NewPub) != ed25519.PublicKeySize {
		return newErr(kindInvalidArgument, "transition keys must be 32 bytes")
	}
	if bytes.Equal(t.OldPub, t.NewPub) {
		return newErr(kindInvalidArgument, "transition does not change the key")
	}
	if err := Verify(ed25519.PublicKey(t.OldPub), PurposeTransition, t.signedData(), t.SigByOld); err != nil {
		return newErr(kindInvalidArgument, "transition signature does not verify")
	}
	return nil
}

// Pin is what a Worker remembers about its Hub: the current public key and
// the position in the transition chain. A fresh pin (from an invitation or a
// pairing) has Seq 0, meaning the position is unknown: no message carries the
// Hub's chain position to a new Worker, so the Hub may already be at link 3
// or more. The first link of a chain has seq 1.
type Pin struct {
	Pub ed25519.PublicKey
	Seq uint64
}

// VerifyChain verifies the links the Hub sent from the Worker's pin forward
// and returns the new pin. Each link's old key must be the key reached so far
// (the pin key for the first link) and its signature must verify with purpose
// transition. Seq rules: when pin.Seq is 0 (position unknown) the first link
// may have any seq >= 1; when pin.Seq > 0 the first link must be exactly
// pin.Seq+1; every later link must be exactly the previous seq + 1. A gap, a
// replayed or decreasing seq, a link that does not continue from the key
// reached so far or a bad signature fails with ErrInvalidArgument. An empty
// chain returns pin unchanged. The caller rewrites its stored pin atomically
// with the result.
func VerifyChain(pin Pin, chain []Transition) (Pin, error) {
	if len(pin.Pub) != ed25519.PublicKeySize {
		return Pin{}, newErr(kindInvalidArgument, "pinned key must be 32 bytes")
	}
	cur := Pin{Pub: bytes.Clone(pin.Pub), Seq: pin.Seq}
	for i, t := range chain {
		switch {
		case i == 0 && pin.Seq == 0:
			if t.Seq < 1 {
				return Pin{}, newErr(kindInvalidArgument, "transition seq must be at least 1")
			}
		case t.Seq <= cur.Seq:
			return Pin{}, newErr(kindInvalidArgument, "transition seq is not increasing")
		case t.Seq != cur.Seq+1:
			return Pin{}, newErr(kindInvalidArgument, "gap in transition chain")
		}
		if !bytes.Equal(t.OldPub, cur.Pub) {
			return Pin{}, newErr(kindInvalidArgument, "transition does not continue from the pinned key")
		}
		if err := t.Verify(); err != nil {
			return Pin{}, err
		}
		cur = Pin{Pub: bytes.Clone(t.NewPub), Seq: t.Seq}
	}
	return cur, nil
}
