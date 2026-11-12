package transport

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/pake"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Mode is the handshake mode a Worker announces in its hello (AD-6).
type Mode string

// The three handshake modes. They differ only in how the Worker decides to
// trust the Hub's public key; the frame order and the key schedule are shared.
const (
	// ModeConnect is the reconnect of an enrolled Worker: the Hub key is
	// checked against the Worker's pin, after applying a transition chain.
	ModeConnect Mode = "connect"
	// ModeEnrollInvite enrolls with an invitation key: the Hub key is
	// checked against the fingerprint in the invitation. The invitation
	// secret itself is sent later, inside the channel.
	ModeEnrollInvite Mode = "enroll_invite"
	// ModePairCode pairs with a pairing code: the Hub is authenticated by
	// CPace key confirmation (AD-8).
	ModePairCode Mode = "pair_code"
)

// Valid reports whether m is one of the three modes.
func (m Mode) Valid() bool {
	return m == ModeConnect || m == ModeEnrollInvite || m == ModePairCode
}

const (
	// NonceSize is the size of the random nonce in each hello.
	NonceSize = 32
	// DefaultHandshakeTimeout bounds the whole handshake when the config
	// sets no timeout.
	DefaultHandshakeTimeout = 30 * time.Second

	maxNameLen      = 255
	maxPairingIDLen = 32
)

// Errors of the handshake. Wrap-aware: use errors.Is. ErrorCode maps them to
// the wire codes of internal/proto.
var (
	// ErrPinMismatch: the Hub key, after applying the chain, is not the pin.
	ErrPinMismatch = errors.New("transport: hub key does not match the pinned key")
	// ErrChainInvalid: the transition chain of the Hub hello does not verify.
	ErrChainInvalid = errors.New("transport: hub transition chain is invalid")
	// ErrUnexpectedChain: a chain in a hello outside connect mode.
	ErrUnexpectedChain = errors.New("transport: transition chain is only allowed in connect mode")
	// ErrFingerprintMismatch: the Hub key is not the one the invitation names.
	ErrFingerprintMismatch = errors.New("transport: hub key does not match the invitation fingerprint")
	// ErrBadSignature: a handshake or pake-confirm signature did not verify.
	ErrBadSignature = errors.New("transport: handshake signature does not verify")
	// ErrPairingConfirm: the CPace confirmation failed, usually a wrong code.
	ErrPairingConfirm = errors.New("transport: pairing confirmation failed")
	// ErrProtocol: a malformed, unexpected or out-of-order handshake frame.
	ErrProtocol = errors.New("transport: handshake protocol violation")
	// ErrConfig: an unusable handshake configuration.
	ErrConfig = errors.New("transport: invalid handshake configuration")
	// ErrExpiredBeforeSigWorker is returned when the Hub certificate reached
	// its not_after (either clock) after the window check and before sig_worker
	// was sent: the handshake ends without sig_worker. It wraps
	// identity.ErrCertExpired.
	ErrExpiredBeforeSigWorker = fmt.Errorf("transport: the Hub certificate reached its not_after before sig_worker: %w", identity.ErrCertExpired)
)

// ErrorCode maps a handshake or channel error to the wire code a peer sees.
// An error carrying a *proto.Error keeps its code.
func ErrorCode(err error) proto.Code {
	var pe *proto.Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &pe):
		return pe.Code
	case errors.Is(err, ErrCertMissing), errors.Is(err, identity.ErrCertMalformed):
		return proto.CodeInvalidArgument
	case errors.Is(err, ErrCertSerialTooLow), errors.Is(err, ErrCertSerialConflict),
		errors.Is(err, ErrResumeRootMismatch), errors.Is(err, ErrHubSigInvalid),
		errors.Is(err, identity.ErrCertRootMismatch), errors.Is(err, identity.ErrCertBadSignature),
		errors.Is(err, identity.ErrCertNotYetValid), errors.Is(err, identity.ErrCertExpired):
		return proto.CodeUnauthenticated
	case errors.Is(err, ErrPinMismatch), errors.Is(err, ErrFingerprintMismatch),
		errors.Is(err, ErrChainInvalid), errors.Is(err, ErrBadSignature):
		return proto.CodeUnauthenticated
	case errors.Is(err, ErrPairingConfirm):
		return proto.CodePairingCodeInvalid
	case errors.Is(err, ErrProtocol), errors.Is(err, ErrUnexpectedChain), errors.Is(err, ErrConfig):
		return proto.CodeInvalidArgument
	}
	return proto.CodeInternal
}

// WorkerHello is the first handshake frame, sent by the Worker.
type WorkerHello struct {
	Type string `json:"type"`
	proto.Range
	Ops []string `json:"ops"`
	// Features are the behaviours the Worker implements that its op names do
	// not show (proto.Feature*). They are part of the hello, so the transcript
	// hash and both handshake signatures cover them. An older Worker omits them.
	Features  []string     `json:"features,omitempty"`
	WorkerPub identity.B64 `json:"worker_pub"`
	EphW      identity.B64 `json:"eph_w"`
	Nonce     identity.B64 `json:"nonce"`
	Mode      Mode         `json:"mode"`
	Name      string       `json:"name"`
	PairingID string       `json:"pairing_id,omitempty"`
}

// HubHello is the Hub's reply. Chain is present in connect mode only.
//
// Protocol 3.0: HubPub is the ROOT public key and must equal the root of
// HubCert; HubCert is the 153-byte certificate of the operational key, mandatory
// in every mode. The certificate is part of the hello, so the transcript hash
// binds it before any CPace message and any signature.
type HubHello struct {
	Type    string                `json:"type"`
	Version proto.Version         `json:"version"`
	HubPub  identity.B64          `json:"hub_pub"`
	HubCert identity.B64          `json:"hub_cert"`
	EphH    identity.B64          `json:"eph_h"`
	Nonce   identity.B64          `json:"nonce"`
	Chain   []identity.Transition `json:"chain,omitempty"`
}

type sigFrame struct {
	Type string       `json:"type"`
	Sig  identity.B64 `json:"sig"`
}

type pakeMsgFrame struct {
	Type string       `json:"type"`
	Msg  identity.B64 `json:"msg"`
}

// pakeConfirmFrame carries the CPace confirmation tag and the signer's
// pake-confirm signature over transcript_hash || tag (amendment A-1).
type pakeConfirmFrame struct {
	Type string       `json:"type"`
	Tag  identity.B64 `json:"tag"`
	Sig  identity.B64 `json:"sig"`
}

// ---------------------------------------------------------------- Worker

// WorkerConfig configures WorkerHandshake.
type WorkerConfig struct {
	// Key is the Worker's identity key.
	Key *identity.Key
	// Name is the display name sent in the hello.
	Name string
	// Ops are the operations this Worker implements (AD-4).
	Ops []string
	// Features are the proto.Feature* names this Worker implements. It sends
	// the normalised list in its hello; nil announces none.
	Features []string
	// Protocols is the supported protocol range. The zero value means only
	// proto.Current.
	Protocols proto.Range
	// Mode selects the trust rule; exactly one of the three fields below
	// is used, matching the mode.
	Mode Mode
	// Pin is the remembered Hub key and chain position (connect).
	Pin identity.Pin
	// HubFingerprint is the fingerprint from the invitation key
	// (enroll_invite).
	HubFingerprint identity.Fingerprint
	// PairingCode is the code shown by this Worker (pair_code).
	PairingCode identity.PairingCode
	// Ratchet is the stored certificate memory: in connect mode the
	// enrollment's max_serial and cert_sha256 (its Root defaults to Pin.Pub);
	// in pair_code mode the pair_resume record (candidate root, serial and
	// hash), whose non-empty Root also binds the Hub's root (section 11.2 4b).
	// Empty in enroll_invite mode and for a first pairing.
	Ratchet Ratchet
	// Clock supplies the wall clock and the monotonic clock of the window
	// check; nil means SystemWallMono.
	Clock WallMono
	// BeforeSigWorker, when set, runs after the Hub is authenticated and no
	// serial refusal is pending, and before sig_worker is sent. A pair_code
	// Worker writes its pair_resume record here, durably; an error aborts the
	// handshake and sig_worker is never sent (section 12.2).
	BeforeSigWorker func(HubAuth) error
	// Timeout bounds the whole handshake; zero means
	// DefaultHandshakeTimeout.
	Timeout time.Duration
}

// WorkerResult is the outcome of a successful Worker handshake.
type WorkerResult struct {
	// Channel is the encrypted channel over the same connection.
	Channel *Channel
	// HubPub is the authenticated Hub identity key.
	HubPub ed25519.PublicKey
	// Version is the protocol version the Hub chose.
	Version proto.Version
	// TranscriptHash identifies this handshake (AD-6).
	TranscriptHash [TranscriptHashSize]byte
	// Pin is the pin the Worker must store: in connect mode the pin after
	// applying the Hub's chain, otherwise the Hub key with Seq 0.
	Pin identity.Pin
	// PinChanged is true when Pin differs from the configured pin (connect)
	// or is new (other modes); the caller rewrites its stored pin atomically
	// before relying on the connection.
	PinChanged bool
	// Cert is the Hub certificate validated in this handshake and OpPub its
	// operational key: the key that signs a revoke on this connection and
	// that was verified for sig_hub. CertSHA256 is the hash of its 153 bytes.
	Cert       *identity.HubCert
	OpPub      ed25519.PublicKey
	CertSHA256 [32]byte
	// Ratchet is the certificate memory to commit with the pin (step 8), scoped
	// to Pin.Pub; RatchetChanged is true when it differs from the configured
	// one (a commit is needed also when only the serial rose).
	Ratchet        Ratchet
	RatchetChanged bool
	// Expiry is the end of this connection (section 11.3).
	Expiry Expiry
}

func (cfg *WorkerConfig) validate() error {
	if cfg.Key == nil {
		return fmt.Errorf("%w: no identity key", ErrConfig)
	}
	if len(cfg.Name) > maxNameLen {
		return fmt.Errorf("%w: name longer than %d bytes", ErrConfig, maxNameLen)
	}
	switch cfg.Mode {
	case ModeConnect:
		if len(cfg.Pin.Pub) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: connect mode needs a pinned Hub key", ErrConfig)
		}
	case ModeEnrollInvite:
		if cfg.HubFingerprint == "" {
			return fmt.Errorf("%w: enroll_invite mode needs the invitation fingerprint", ErrConfig)
		}
	case ModePairCode:
		if cfg.PairingCode.PairingID() == "" {
			return fmt.Errorf("%w: pair_code mode needs a pairing code", ErrConfig)
		}
	default:
		return fmt.Errorf("%w: unknown mode %q", ErrConfig, cfg.Mode)
	}
	if cfg.Protocols == (proto.Range{}) {
		cfg.Protocols = proto.Range{Min: proto.Current, Max: proto.Current}
	}
	return cfg.Protocols.Validate()
}

// WorkerHandshake runs the Worker side of the AD-6 handshake over conn and
// returns the encrypted channel. conn carries raw handshake frames until the
// handshake ends and then the channel's records. On any error conn is closed.
//
// sig_worker (purpose hs-worker) is sent only after the Hub is authenticated,
// in all three modes: in connect and enroll_invite mode the Hub key is
// checked against the pin or the fingerprint first, in pair_code mode the
// Hub's pake.confirm (CPace tag and pake-confirm signature) must verify
// first.
//
// In pair_code mode the Worker does send its own pake.confirm (the CPace tag
// and the Ed25519 pake-confirm signature over transcript_hash || tag) before
// the Hub has proved that it knows the code. The Worker confirms first on
// purpose: the Hub, which alone counts attempts (AD-8), never reveals its tag
// to an unverified peer, so a peer that gets nothing from the Hub without
// submitting a confirmation cannot learn anything about the code without
// spending a counted attempt.
func WorkerHandshake(conn net.Conn, cfg WorkerConfig) (res *WorkerResult, err error) {
	if err := cfg.validate(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	finish := startDeadline(conn, cfg.Timeout)
	defer func() { err = finish(err) }()

	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	hello := WorkerHello{
		Type:      proto.TypeHello,
		Range:     cfg.Protocols,
		Ops:       cfg.Ops,
		Features:  proto.NormaliseFeatures(cfg.Features),
		WorkerPub: identity.B64(cfg.Key.Public()),
		EphW:      eph.PublicKey().Bytes(),
		Nonce:     make([]byte, NonceSize),
		Mode:      cfg.Mode,
		Name:      cfg.Name,
	}
	if hello.Ops == nil {
		hello.Ops = []string{}
	}
	if _, err := rand.Read(hello.Nonce); err != nil {
		return nil, err
	}
	if cfg.Mode == ModePairCode {
		hello.PairingID = cfg.PairingCode.PairingID()
	}
	workerBytes, err := sendFrame(conn, hello)
	if err != nil {
		return nil, err
	}

	hubBytes, err := expectFrame(conn, proto.TypeHello)
	if err != nil {
		return nil, err
	}
	var hub HubHello
	if err := decode(hubBytes, &hub); err != nil {
		return nil, err
	}
	// Step a: shape, including the certificate's structure.
	cert, err := checkHubHello(&hub, cfg.Mode, cfg.Protocols)
	if err != nil {
		return nil, err
	}
	hubPub := ed25519.PublicKey(hub.HubPub)
	hash := TranscriptHash(workerBytes, hubBytes)

	// Step b: the root. The candidate stays in memory until the commit.
	pin := identity.Pin{Pub: bytes.Clone(hubPub)}
	switch cfg.Mode {
	case ModeConnect:
		if pin, err = resolvePin(cfg.Pin, hubPub, hub.Chain); err != nil {
			return nil, err
		}
	case ModeEnrollInvite:
		if !cfg.HubFingerprint.Matches(hubPub) {
			return nil, ErrFingerprintMismatch
		}
	case ModePairCode:
		if len(cfg.Ratchet.Root) > 0 && !bytes.Equal(cfg.Ratchet.Root, hubPub) {
			return nil, ErrResumeRootMismatch
		}
	}
	candidate := ed25519.PublicKey(pin.Pub)

	// Step c: the certificate under the candidate root.
	if !bytes.Equal(cert.RootPub, hubPub) {
		return nil, identity.ErrCertRootMismatch
	}
	if err := cert.Verify(candidate); err != nil {
		return nil, err
	}

	// Step d: the window. The wall clock and the monotonic clock are read back
	// to back.
	clk := cfg.Clock
	if clk == nil {
		clk = SystemWallMono
	}
	now, mono0 := clk.Wall(), clk.Mono()
	if err := cert.CheckWindow(now.UnixMilli()); err != nil {
		return nil, err
	}
	expiry, err := newExpiry(cert, now.UnixMilli(), mono0)
	if err != nil {
		return nil, err
	}

	// Step e: the serial scope. The verdict is kept and applied after sig_hub.
	configured := cfg.Ratchet
	if cfg.Mode == ModeConnect && len(configured.Root) == 0 {
		configured.Root = cfg.Pin.Pub
	}
	scope := configured
	if cfg.Mode == ModeEnrollInvite {
		scope = Ratchet{}
	}
	certHashV := certHash(cert)
	verdict := scope.verdict(candidate, cert.Serial, certHashV)

	var session *pake.Session
	if cfg.Mode == ModePairCode {
		if session, err = workerPake(conn, cfg, hash[:], cert.OpPub); err != nil {
			return nil, err
		}
	}

	// sig_hub is made by the operational key of the certificate, never by the
	// root.
	if err := expectSig(conn, cert.OpPub, identity.PurposeHandshakeHub, hash[:]); err != nil {
		if errors.Is(err, ErrBadSignature) {
			return nil, fmt.Errorf("%w: %w", ErrHubSigInvalid, err)
		}
		return nil, err
	}
	if verdict != nil {
		// Authenticated but refused: close without sig_worker; nothing was
		// written.
		return nil, verdict
	}
	newRatchet := scope.Raise(candidate, cert.Serial, certHashV)
	auth := HubAuth{
		Mode: cfg.Mode, Root: candidate, Pin: identity.Pin{Pub: bytes.Clone(pin.Pub), Seq: pin.Seq},
		Cert: cert, CertSHA256: certHashV, Ratchet: newRatchet,
		RatchetChanged: cfg.Mode != ModeConnect || !newRatchet.Equal(configured), Expiry: expiry,
	}
	if cfg.BeforeSigWorker != nil {
		if err := cfg.BeforeSigWorker(auth); err != nil {
			return nil, err
		}
	}
	// Both clocks are read again immediately before sig_worker (section 11.3):
	// the PAKE, sig_hub and BeforeSigWorker (a durable write) can block, and no
	// signature leaves after the certificate's end.
	if expiry.Expired(clk) {
		return nil, ErrExpiredBeforeSigWorker
	}
	sig, err := cfg.Key.Sign(identity.PurposeHandshakeWorker, hash[:])
	if err != nil {
		return nil, err
	}
	if _, err := sendFrame(conn, sigFrame{Type: proto.TypeSig, Sig: sig}); err != nil {
		return nil, err
	}

	ch, err := openChannel(conn, eph, hub.EphH, session, hash[:], true)
	if err != nil {
		return nil, err
	}
	return &WorkerResult{
		Channel:        ch,
		HubPub:         hubPub,
		Version:        hub.Version,
		TranscriptHash: hash,
		Pin:            pin,
		PinChanged:     cfg.Mode != ModeConnect || !bytes.Equal(pin.Pub, cfg.Pin.Pub) || pin.Seq != cfg.Pin.Seq,
		Cert:           cert,
		OpPub:          bytes.Clone(cert.OpPub),
		CertSHA256:     certHashV,
		Ratchet:        newRatchet,
		RatchetChanged: auth.RatchetChanged,
		Expiry:         expiry,
	}, nil
}

// checkHubHello is step a of the Worker's validation: the lengths, the version
// range, the chain only in connect mode, and a present, well-formed
// certificate (the structural part of step c is in the shape tier of the
// precedence, so a malformed certificate wins over a root mismatch).
func checkHubHello(h *HubHello, mode Mode, own proto.Range) (*identity.HubCert, error) {
	if err := checkLens(map[string]struct {
		got  int
		want int
	}{
		"hub_pub": {len(h.HubPub), ed25519.PublicKeySize},
		"eph_h":   {len(h.EphH), 32},
		"nonce":   {len(h.Nonce), NonceSize},
	}); err != nil {
		return nil, err
	}
	if !own.Contains(h.Version) {
		return nil, proto.NewError(proto.CodeProtoIncompatible,
			fmt.Sprintf("hub chose protocol %s outside the supported range %s..%s", h.Version, own.Min, own.Max))
	}
	if len(h.Chain) > 0 && mode != ModeConnect {
		return nil, ErrUnexpectedChain
	}
	if len(h.HubCert) == 0 {
		return nil, ErrCertMissing
	}
	return identity.ParseHubCert(h.HubCert)
}

func checkLens(m map[string]struct{ got, want int }) error {
	for name, l := range m {
		if l.got != l.want {
			return fmt.Errorf("%w: %s must be %d bytes", ErrProtocol, name, l.want)
		}
	}
	return nil
}

// resolvePin applies the part of the Hub's chain that starts at the pinned
// key and checks that the result is the key the Hub presented. Links before
// the pinned key (older rotations the Worker already knows) are skipped.
func resolvePin(pin identity.Pin, hubPub ed25519.PublicKey, chain []identity.Transition) (identity.Pin, error) {
	var links []identity.Transition
	for i, t := range chain {
		if bytes.Equal(t.OldPub, pin.Pub) {
			links = chain[i:]
			break
		}
	}
	next, err := identity.VerifyChain(pin, links)
	if err != nil {
		return identity.Pin{}, fmt.Errorf("%w: %w", ErrChainInvalid, err)
	}
	if !bytes.Equal(next.Pub, hubPub) {
		return identity.Pin{}, ErrPinMismatch
	}
	return next, nil
}

// workerPake runs the Worker side of the AD-8 exchange: pake.msg, pake.msg,
// pake.confirm in both directions. It returns the CPace session once the
// Hub's confirmation tag and pake-confirm signature verified.
func workerPake(conn net.Conn, cfg WorkerConfig, hash []byte, opPub ed25519.PublicKey) (*pake.Session, error) {
	params := pake.Params{Password: cfg.PairingCode.Password(), SessionID: hash}
	init, msgA, err := pake.Start(params)
	if err != nil {
		return nil, err
	}
	if _, err := sendFrame(conn, pakeMsgFrame{Type: proto.TypePakeMsg, Msg: msgA}); err != nil {
		return nil, err
	}
	body, err := expectFrame(conn, proto.TypePakeMsg)
	if err != nil {
		return nil, err
	}
	var msgB pakeMsgFrame
	if err := decode(body, &msgB); err != nil {
		return nil, err
	}
	session, err := init.Finish(msgB.Msg)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProtocol, err)
	}
	if err := sendConfirm(conn, session, cfg.Key, hash); err != nil {
		return nil, err
	}
	if err := expectConfirm(conn, session, opPub, hash); err != nil {
		return nil, err
	}
	return session, nil
}

// ------------------------------------------------------------------- Hub

// HubConfig configures HubHandshake.
type HubConfig struct {
	// Key is the Hub's OPERATIONAL key: it makes sig_hub, the pake-confirm
	// signature and (outside the handshake) the revoke of this connection.
	Key *identity.Key
	// Cert is the certificate of Key, issued by the root. Its root is sent as
	// hub_pub and the certificate itself in hub_cert, in every mode. Key and
	// Cert are one pair, taken together from the Hub identity service.
	Cert *identity.HubCert
	// Now is the Hub's wall clock; nil means time.Now. A certificate whose
	// not_after has been reached (now >= not_after) is never sent: the
	// handshake fails with hub_cert_expired.
	Now func() time.Time
	// Current is the Hub's protocol version; the Hub accepts it and its
	// previous minor (AD-4). The zero value means proto.Current.
	Current proto.Version
	// Chain is the Hub's transition chain, sent in connect mode only.
	Chain []identity.Transition
	// Admit, when set, is called with the validated Worker hello before
	// anything else is sent. Returning an error aborts the handshake; a
	// *proto.Error is also sent to the Worker as an error frame (use it for
	// rate limiting and pairing id collisions).
	Admit func(WorkerHello) error
	// PairingPassword returns the CPace password of the live pairing code
	// with the given public pairing id. It is required for pair_code mode;
	// an error aborts the handshake and is sent like an Admit error.
	PairingPassword func(pairingID string) ([]byte, error)
	// Timeout bounds the whole handshake; zero means
	// DefaultHandshakeTimeout.
	Timeout time.Duration
}

// HubResult is the outcome of a successful Hub handshake.
type HubResult struct {
	// Channel is the encrypted channel over the same connection.
	Channel *Channel
	// Worker is the Worker hello as received; the Worker proved possession
	// of Worker.WorkerPub, but whether it is enrolled is the caller's
	// decision (AD-6: answer inside the channel).
	Worker WorkerHello
	// WorkerPub is the authenticated Worker identity key.
	WorkerPub ed25519.PublicKey
	// Version is the negotiated protocol version.
	Version proto.Version
	// TranscriptHash identifies this handshake; signed revocations use it.
	TranscriptHash [TranscriptHashSize]byte
}

// HubHandshake runs the Hub side of the AD-6 handshake (protocol 3.0) over conn. On any
// error conn is closed. In pair_code mode a failed confirmation returns
// ErrPairingConfirm after an error frame pairing_code_invalid was sent, so the
// caller can count the attempt (AD-8).
func HubHandshake(conn net.Conn, cfg HubConfig) (res *HubResult, err error) {
	if cfg.Key == nil || cfg.Cert == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: no operational key or certificate", ErrConfig)
	}
	if !bytes.Equal(cfg.Cert.OpPub, cfg.Key.Public()) || len(cfg.Cert.Bytes()) != identity.HubCertSize {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: the certificate does not belong to the operational key", ErrConfig)
	}
	if cfg.Current == (proto.Version{}) {
		cfg.Current = proto.Current
	}
	finish := startDeadline(conn, cfg.Timeout)
	defer func() { err = finish(err) }()

	workerBytes, err := expectFrame(conn, proto.TypeHello)
	if err != nil {
		return nil, err
	}
	var wh WorkerHello
	if err := decode(workerBytes, &wh); err != nil {
		return nil, err
	}
	if err := checkWorkerHello(&wh); err != nil {
		return nil, err
	}
	version, err := proto.Negotiate(wh.Range, proto.Supported(cfg.Current))
	if err != nil {
		return nil, abortWith(conn, err)
	}
	if cfg.Admit != nil {
		if err := cfg.Admit(wh); err != nil {
			return nil, abortWith(conn, err)
		}
	}
	// The Hub sends its hello only with a certificate whose not_after is still
	// in the future.
	now := time.Now
	if cfg.Now != nil {
		now = cfg.Now
	}
	if now().UnixMilli() >= cfg.Cert.NotAfterMs {
		return nil, abortWith(conn, proto.NewError(proto.CodeHubCertExpired, "the Hub certificate has expired"))
	}
	var password []byte
	if wh.Mode == ModePairCode {
		if cfg.PairingPassword == nil {
			return nil, abortWith(conn, proto.NewError(proto.CodePairingCodeInvalid, "pairing is not available"))
		}
		if password, err = cfg.PairingPassword(wh.PairingID); err != nil {
			return nil, abortWith(conn, err)
		}
	}

	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	hh := HubHello{
		Type:    proto.TypeHello,
		Version: version,
		HubPub:  identity.B64(cfg.Cert.RootPub),
		HubCert: cfg.Cert.Bytes(),
		EphH:    eph.PublicKey().Bytes(),
		Nonce:   make([]byte, NonceSize),
	}
	if _, err := rand.Read(hh.Nonce); err != nil {
		return nil, err
	}
	if wh.Mode == ModeConnect {
		hh.Chain = cfg.Chain
	}
	// PairingPassword may block (the Hub waits for the user to claim the code),
	// so the deadline is read again right before the hello goes out.
	if now().UnixMilli() >= cfg.Cert.NotAfterMs {
		return nil, abortWith(conn, proto.NewError(proto.CodeHubCertExpired, "the Hub certificate has expired"))
	}
	hubBytes, err := sendFrame(conn, hh)
	if err != nil {
		return nil, err
	}
	hash := TranscriptHash(workerBytes, hubBytes)
	workerPub := ed25519.PublicKey(wh.WorkerPub)

	var session *pake.Session
	if wh.Mode == ModePairCode {
		if session, err = hubPake(conn, cfg.Key, password, hash[:], workerPub); err != nil {
			return nil, err
		}
	}

	sig, err := cfg.Key.Sign(identity.PurposeHandshakeHub, hash[:])
	if err != nil {
		return nil, err
	}
	if _, err := sendFrame(conn, sigFrame{Type: proto.TypeSig, Sig: sig}); err != nil {
		return nil, err
	}
	if err := expectSig(conn, workerPub, identity.PurposeHandshakeWorker, hash[:]); err != nil {
		return nil, err
	}

	ch, err := openChannel(conn, eph, wh.EphW, session, hash[:], false)
	if err != nil {
		return nil, err
	}
	return &HubResult{Channel: ch, Worker: wh, WorkerPub: workerPub, Version: version, TranscriptHash: hash}, nil
}

func checkWorkerHello(h *WorkerHello) error {
	if err := checkLens(map[string]struct{ got, want int }{
		"worker_pub": {len(h.WorkerPub), ed25519.PublicKeySize},
		"eph_w":      {len(h.EphW), 32},
		"nonce":      {len(h.Nonce), NonceSize},
	}); err != nil {
		return err
	}
	if !h.Mode.Valid() {
		return fmt.Errorf("%w: unknown mode %q", ErrProtocol, h.Mode)
	}
	if len(h.Name) > maxNameLen {
		return fmt.Errorf("%w: name longer than %d bytes", ErrProtocol, maxNameLen)
	}
	if (h.Mode == ModePairCode) != (h.PairingID != "") || len(h.PairingID) > maxPairingIDLen {
		return fmt.Errorf("%w: pairing_id is required in pair_code mode and only there", ErrProtocol)
	}
	return nil
}

// hubPake runs the Hub side of the AD-8 exchange. It sends its confirmation
// only after the Worker's confirmation verified, so a wrong code learns
// nothing from the Hub beyond the error frame.
func hubPake(conn net.Conn, key *identity.Key, password, hash []byte, workerPub ed25519.PublicKey) (*pake.Session, error) {
	body, err := expectFrame(conn, proto.TypePakeMsg)
	if err != nil {
		return nil, err
	}
	var msgA pakeMsgFrame
	if err := decode(body, &msgA); err != nil {
		return nil, err
	}
	session, msgB, err := pake.Respond(pake.Params{Password: password, SessionID: hash}, msgA.Msg)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProtocol, err)
	}
	if _, err := sendFrame(conn, pakeMsgFrame{Type: proto.TypePakeMsg, Msg: msgB}); err != nil {
		return nil, err
	}
	if err := expectConfirm(conn, session, workerPub, hash); err != nil {
		if errors.Is(err, ErrPairingConfirm) {
			_ = abortWith(conn, proto.NewError(proto.CodePairingCodeInvalid, "pairing confirmation failed"))
		}
		return nil, err
	}
	if err := sendConfirm(conn, session, key, hash); err != nil {
		return nil, err
	}
	return session, nil
}

// ---------------------------------------------------------------- shared

func sendConfirm(conn net.Conn, s *pake.Session, key *identity.Key, hash []byte) error {
	tag := s.Confirmation()
	sig, err := key.Sign(identity.PurposePAKEConfirm, pakeConfirmData(hash, tag))
	if err != nil {
		return err
	}
	_, err = sendFrame(conn, pakeConfirmFrame{Type: proto.TypePakeConfirm, Tag: tag, Sig: sig})
	return err
}

// expectConfirm reads a pake.confirm and checks the CPace tag against the
// session and the signature against the peer's identity key.
func expectConfirm(conn net.Conn, s *pake.Session, peerPub ed25519.PublicKey, hash []byte) error {
	body, err := expectFrame(conn, proto.TypePakeConfirm)
	if err != nil {
		return err
	}
	var f pakeConfirmFrame
	if err := decode(body, &f); err != nil {
		return err
	}
	if err := s.VerifyConfirmation(f.Tag); err != nil {
		return ErrPairingConfirm
	}
	if err := identity.Verify(peerPub, identity.PurposePAKEConfirm, pakeConfirmData(hash, f.Tag), f.Sig); err != nil {
		return fmt.Errorf("%w: pake-confirm", ErrBadSignature)
	}
	return nil
}

func expectSig(conn net.Conn, pub ed25519.PublicKey, p identity.Purpose, hash []byte) error {
	body, err := expectFrame(conn, proto.TypeSig)
	if err != nil {
		return err
	}
	var f sigFrame
	if err := decode(body, &f); err != nil {
		return err
	}
	if err := identity.Verify(pub, p, hash, f.Sig); err != nil {
		return fmt.Errorf("%w: %s", ErrBadSignature, p)
	}
	return nil
}

// openChannel derives the keys (AD-6 step 6) and wraps conn. peerEph is the
// peer's ephemeral X25519 key; session is the CPace session in pair_code mode
// and nil otherwise.
func openChannel(conn net.Conn, eph *ecdh.PrivateKey, peerEph []byte, session *pake.Session, hash []byte, worker bool) (*Channel, error) {
	pub, err := ecdh.X25519().NewPublicKey(peerEph)
	if err != nil {
		return nil, fmt.Errorf("%w: ephemeral key: %w", ErrProtocol, err)
	}
	dh, err := eph.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("%w: ephemeral key agreement: %w", ErrProtocol, err)
	}
	var isk []byte
	if session != nil {
		isk = session.Key()
		defer clear(isk)
	}
	defer clear(dh)
	keys, err := deriveKeys(dh, isk, hash)
	if err != nil {
		return nil, err
	}
	defer keys.wipe()
	if worker {
		return newChannel(conn, keys.workerToHub[:], keys.hubToWorker[:])
	}
	return newChannel(conn, keys.hubToWorker[:], keys.workerToHub[:])
}

// startDeadline arms the handshake deadline and returns the function that
// finishes the handshake: it closes conn on error and clears the deadline on
// success.
func startDeadline(conn net.Conn, timeout time.Duration) func(error) error {
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	return func(err error) error {
		if err != nil {
			_ = conn.Close()
			return err
		}
		return conn.SetDeadline(time.Time{})
	}
}

// sendFrame marshals v, writes it as one handshake frame and returns the
// exact body bytes sent (the bytes the transcript hashes).
func sendFrame(w io.Writer, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProtocol, err)
	}
	if len(body) > proto.HandshakeFrameLimit {
		return nil, proto.NewError(proto.CodeFrameTooLarge, "handshake frame exceeds 64 KiB")
	}
	buf := make([]byte, 4, 4+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	buf = append(buf, body...)
	if _, err := w.Write(buf); err != nil {
		return nil, err
	}
	return body, nil
}

// expectFrame reads one handshake frame and returns its body when it has the
// wanted type. An error or not_enrolled frame from the peer is returned as a
// *proto.Error. These frames arrive before the peer is authenticated (the
// transport accepts any certificate until the inner key authentication), so
// they are information for the user and never a reason to change stored state,
// and their wording is never shown: the error has the peer's code when this
// build knows it and a fixed local text, see [unauthenticatedPeerError].
func expectFrame(r io.Reader, want string) ([]byte, error) {
	body, err := proto.ReadFrame(r, proto.HandshakeFrameLimit)
	if err != nil {
		return nil, err
	}
	got, err := proto.FrameType(body)
	if err != nil {
		return nil, err
	}
	switch got {
	case want:
		return body, nil
	case proto.TypeError:
		var f proto.ErrorFrame
		if err := proto.DecodeFrame(body, &f); err != nil {
			return nil, err
		}
		return nil, unauthenticatedPeerError(f.Code, f.Details)
	case proto.TypeNotEnrolled:
		return nil, proto.NewError(proto.CodeNotEnrolled, "the Hub does not know this Worker")
	}
	return nil, fmt.Errorf("%w: got %q frame, want %q", ErrProtocol, got, want)
}

// peerReasons are the reasons a pre-authentication error frame may name in
// details.reason. The peer picks one token of this closed set; the sentence that
// the operator reads is written here, so the token is the only thing that
// travels. Tokens are lower-case ASCII; anything else is ignored.
var peerReasons = map[proto.Code]map[string]string{
	proto.CodePairingCodeInvalid: {
		"expired": "the pairing code expired; the Worker shows a new one",
	},
}

// maxPeerAttempts bounds details.attempts_left, which is shown as a number.
const maxPeerAttempts = 99

// unauthenticatedPeerError is the error for an error frame that arrived before
// the peer proved its identity. Anyone who can answer the connection can send
// it, so nothing the peer chose is shown but its code, and only a code this
// build declares (the text comes from the local code table): an unknown code
// becomes untrusted_peer. Two details are kept because they are useful and carry
// no free text: a reason token of the closed set [peerReasons] and a bounded
// attempts_left number; both are rendered from local text. The text says that
// the peer is not authenticated, so it cannot be taken for an instruction of the
// Hub, and the peer's message and any other detail are dropped.
func unauthenticatedPeerError(code proto.Code, details json.RawMessage) *proto.Error {
	if !code.Known() {
		return proto.NewError(proto.CodeUntrustedPeer,
			"the peer answered with an error code this client does not know; the peer is not authenticated, its wording is not shown")
	}
	text := code.Description()
	var d struct {
		Reason       string `json:"reason"`
		AttemptsLeft *int   `json:"attempts_left"`
	}
	if len(details) > 0 && len(details) <= 256 && json.Unmarshal(details, &d) == nil {
		if r, ok := peerReasons[code][d.Reason]; ok {
			text = r
		}
		if d.AttemptsLeft != nil && *d.AttemptsLeft >= 0 && *d.AttemptsLeft <= maxPeerAttempts {
			text += fmt.Sprintf(" (%d attempts left)", *d.AttemptsLeft)
		}
	}
	return proto.NewError(code, "reported by a peer that is not authenticated (its wording is not shown): "+text)
}

func decode(body []byte, v any) error {
	if err := proto.DecodeFrame(body, v); err != nil {
		return fmt.Errorf("%w: %w", ErrProtocol, err)
	}
	return nil
}

// abortWith sends err to the peer as an error frame (best effort) and returns
// err. A *proto.Error is sent as it is; any other error is sent as a bare
// internal error so that no internal detail leaves the process.
func abortWith(w io.Writer, err error) error {
	var pe *proto.Error
	if !errors.As(err, &pe) {
		pe = proto.NewError(proto.CodeInternal, "")
	}
	_, _ = sendFrame(w, pe.Frame())
	return err
}
