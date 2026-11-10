package pake

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"

	"github.com/gtank/ristretto255"
)

// Suite constants for CPACE-RISTR255-SHA512 (draft-irtf-cfrg-cpace-21, 8.3).
const (
	// DSI is G_Ristretto255.DSI.
	DSI = "CPaceRistretto255"
	// ElementSize is the size of an encoded ristretto255 element.
	ElementSize = 32
	// KeySize is the size of the intermediate session key ISK (SHA-512).
	KeySize = sha512.Size
	// ConfirmationSize is the size of a key-confirmation tag (HMAC-SHA-512).
	ConfirmationSize = sha512.Size
	// MaxADSize bounds the associated data carried in one message.
	MaxADSize = 1024

	hashBlockSize = sha512.BlockSize // H.s_in_bytes = 128 for SHA-512
	scalarSize    = 32               // G.group_size_bytes
)

// Errors returned by the package. Every error aborts the exchange; the
// state that returned it must not be reused.
var (
	ErrInvalidParams      = errors.New("pake: invalid parameters")
	ErrInvalidMessage     = errors.New("pake: invalid message")
	ErrIdentityElement    = errors.New("pake: shared secret is the identity element")
	ErrConfirmationFailed = errors.New("pake: key confirmation failed")
	ErrInitiatorFinished  = errors.New("pake: initiator already finished")
	ErrReflected          = errors.New("pake: peer message reflects our own message")
)

// identityEncoding is G.I, the public encoding of the neutral element.
var identityEncoding = ristretto255.NewIdentityElement().Bytes()

// Params are the CPace inputs of one party.
type Params struct {
	// Password is the password-related string PRS. Must not be empty.
	Password []byte
	// SessionID is the CPace sid. AD-8 sets it to the AD-6 transcript_hash.
	// Must not be empty.
	SessionID []byte
	// ChannelID is the CPace channel identifier CI. It is never sent; both
	// parties must hold the same value. May be empty.
	ChannelID []byte
	// AD is this party's associated data (ADa or ADb). It is sent in clear
	// and authenticated by the exchange. At most MaxADSize bytes.
	AD []byte
}

func (p *Params) validate() error {
	switch {
	case len(p.Password) == 0:
		return fmt.Errorf("%w: empty password", ErrInvalidParams)
	case len(p.SessionID) == 0:
		return fmt.Errorf("%w: empty session id", ErrInvalidParams)
	case len(p.AD) > MaxADSize:
		return fmt.Errorf("%w: associated data longer than %d bytes", ErrInvalidParams, MaxADSize)
	}
	return nil
}

// Initiator is the state party A (the Worker) keeps between Start and Finish.
type Initiator struct {
	y    *ristretto255.Scalar
	sid  []byte
	ya   []byte
	ada  []byte
	done bool
}

// Session is the result of a successful CPace run.
type Session struct {
	isk     []byte
	macKey  []byte
	ownMsg  []byte // lv_cat(Y, AD) of this party
	peerMsg []byte // lv_cat(Y, AD) of the peer
	peerAD  []byte
}

// Start begins an exchange as initiator (party A) and returns the state to
// keep and message A to send to the responder.
func Start(p Params) (*Initiator, []byte, error) {
	y, err := sampleScalar(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return startWithScalar(p, y)
}

func startWithScalar(p Params, y *ristretto255.Scalar) (*Initiator, []byte, error) {
	if err := p.validate(); err != nil {
		return nil, nil, err
	}
	g := calculateGenerator(p.Password, p.ChannelID, p.SessionID)
	ya := ristretto255.NewElement().ScalarMult(y, g).Bytes()
	st := &Initiator{
		y:   y,
		sid: clone(p.SessionID),
		ya:  ya,
		ada: clone(p.AD),
	}
	return st, encodeMessage(ya, p.AD), nil
}

// Respond runs the responder side (party B, the Hub) on message A. It
// returns the established session and message B to send to the initiator.
// On error nothing must be sent except an abort.
func Respond(p Params, msgA []byte) (*Session, []byte, error) {
	y, err := sampleScalar(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return respondWithScalar(p, msgA, y)
}

func respondWithScalar(p Params, msgA []byte, y *ristretto255.Scalar) (*Session, []byte, error) {
	if err := p.validate(); err != nil {
		return nil, nil, err
	}
	ya, ada, err := decodeMessage(msgA)
	if err != nil {
		return nil, nil, err
	}
	g := calculateGenerator(p.Password, p.ChannelID, p.SessionID)
	yb := ristretto255.NewElement().ScalarMult(y, g).Bytes()
	k, err := scalarMultVfy(y, ya)
	if err != nil {
		return nil, nil, err
	}
	s := newSession(p.SessionID, k, ya, ada, yb, p.AD, false)
	return s, encodeMessage(yb, p.AD), nil
}

// Finish completes the exchange as initiator on message B. The Initiator
// can be finished only once; its secret scalar is erased afterwards, on
// success and on failure.
func (st *Initiator) Finish(msgB []byte) (*Session, error) {
	if st == nil || st.done {
		return nil, ErrInitiatorFinished
	}
	st.done = true
	defer func() { st.y.Zero(); st.y = nil }()

	yb, adb, err := decodeMessage(msgB)
	if err != nil {
		return nil, err
	}
	// Reject a reflected message: a responder share equal to our own share
	// (or a message identical to message A) cannot come from a party that
	// knows the password and would let our own confirmation tag be replayed.
	if subtle.ConstantTimeCompare(yb, st.ya) == 1 ||
		subtle.ConstantTimeCompare(msgB, encodeMessage(st.ya, st.ada)) == 1 {
		return nil, ErrReflected
	}
	k, err := scalarMultVfy(st.y, yb)
	if err != nil {
		return nil, err
	}
	return newSession(st.sid, k, st.ya, st.ada, yb, adb, true), nil
}

// Key returns a copy of the intermediate session key ISK (KeySize bytes).
func (s *Session) Key() []byte { return clone(s.isk) }

// PeerAD returns a copy of the associated data the peer sent.
func (s *Session) PeerAD() []byte { return clone(s.peerAD) }

// Confirmation returns this party's key-confirmation tag (draft 10.4):
// HMAC-SHA-512(mac_key, lv_cat(Y, AD)) over the message this party sent,
// with mac_key = SHA-512(b"CPaceMac" || sid || ISK).
func (s *Session) Confirmation() []byte { return s.tag(s.ownMsg) }

// VerifyConfirmation checks the peer's key-confirmation tag in constant
// time. A mismatch means the peer used a different password, session id,
// channel id or saw a different transcript; the caller must abort.
func (s *Session) VerifyConfirmation(peerTag []byte) error {
	// Direction binding: the expected tag is over the peer's message, and
	// our own tag is never accepted as the peer's.
	if subtle.ConstantTimeCompare(s.tag(s.ownMsg), peerTag) == 1 ||
		!hmac.Equal(s.tag(s.peerMsg), peerTag) {
		return ErrConfirmationFailed
	}
	return nil
}

func (s *Session) tag(msg []byte) []byte {
	m := hmac.New(sha512.New, s.macKey)
	m.Write(msg)
	return m.Sum(nil)
}

// newSession derives ISK with transcript_ir (draft 7.2, A.3.4).
func newSession(sid, k, ya, ada, yb, adb []byte, initiator bool) *Session {
	transcript := transcriptIR(ya, ada, yb, adb)

	h := sha512.New()
	h.Write(lvCat([]byte(DSI+"_ISK"), sid, k))
	h.Write(transcript)
	isk := h.Sum(nil)

	mk := sha512.New()
	mk.Write([]byte("CPaceMac"))
	mk.Write(sid)
	mk.Write(isk)

	a, b := lvCat(ya, ada), lvCat(yb, adb)
	s := &Session{isk: isk, macKey: mk.Sum(nil)}
	if initiator {
		s.ownMsg, s.peerMsg, s.peerAD = a, b, clone(adb)
	} else {
		s.ownMsg, s.peerMsg, s.peerAD = b, a, clone(ada)
	}
	return s
}

// calculateGenerator implements G_Ristretto255.calculate_generator (8.3).
func calculateGenerator(prs, ci, sid []byte) *ristretto255.Element {
	h := sha512.Sum512(generatorString([]byte(DSI), prs, ci, sid, hashBlockSize))
	g, err := ristretto255.NewElement().SetUniformBytes(h[:])
	if err != nil {
		panic("pake: SetUniformBytes rejected a 64-byte input: " + err.Error())
	}
	return g
}

// scalarMultVfy implements G_Ristretto255.scalar_mult_vfy (8.3) together
// with the abort rule of 7.2: it fails if X does not decode or if the result
// is the neutral element G.I. The identity comparison is constant time.
func scalarMultVfy(y *ristretto255.Scalar, x []byte) ([]byte, error) {
	if len(x) != ElementSize {
		return nil, ErrInvalidMessage
	}
	X, err := ristretto255.NewElement().SetCanonicalBytes(x)
	if err != nil {
		return nil, fmt.Errorf("%w: public share does not decode", ErrInvalidMessage)
	}
	k := ristretto255.NewElement().ScalarMult(y, X).Bytes()
	if subtle.ConstantTimeCompare(k, identityEncoding) == 1 {
		return nil, ErrIdentityElement
	}
	return k, nil
}

// sampleScalar implements the RECOMMENDED G.sample_scalar of 8.3: 32
// random bytes with the bits above group_size_bits (252) cleared,
// interpreted little-endian. Such a value is always below the group order.
func sampleScalar(r io.Reader) (*ristretto255.Scalar, error) {
	var b [scalarSize]byte
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, fmt.Errorf("pake: reading randomness: %w", err)
		}
		b[scalarSize-1] &= 0x0f
		s, err := ristretto255.NewScalar().SetCanonicalBytes(b[:])
		if err != nil {
			return nil, fmt.Errorf("pake: sampling scalar: %w", err)
		}
		if s.Equal(ristretto255.NewScalar()) == 0 { // reject zero
			clear(b[:])
			return s, nil
		}
	}
}

func encodeMessage(y, ad []byte) []byte {
	out := make([]byte, 0, len(y)+len(ad))
	out = append(out, y...)
	return append(out, ad...)
}

func decodeMessage(msg []byte) (y, ad []byte, err error) {
	if len(msg) < ElementSize || len(msg)-ElementSize > MaxADSize {
		return nil, nil, fmt.Errorf("%w: bad length %d", ErrInvalidMessage, len(msg))
	}
	return clone(msg[:ElementSize]), clone(msg[ElementSize:]), nil
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }
