// Package pake implements the CPace balanced password-authenticated key
// exchange as specified in draft-irtf-cfrg-cpace-21, cipher suite
// CPACE-RISTR255-SHA512 (group ristretto255, hash SHA-512), in the
// initiator-responder setting.
//
// STATUS: this is an in-house implementation. It is pending external
// cryptographic review, which is required before the 1.0 release (see the
// architecture spine, AD-8 and the deferred-decisions table). Until then it
// must be treated as unreviewed cryptographic code.
//
// The package exposes exactly three calls so that a vetted library can
// replace it without changing callers (AD-8):
//
//	Start   (initiator, the Worker)  -> *Initiator, message A
//	Respond (responder, the Hub)     -> *Session,   message B
//	Finish  (initiator)              -> *Session
//
// Both sides must use the same Params.Password (the CPace PRS, the whole
// pairing code), the same Params.SessionID (AD-8: the AD-6 transcript_hash)
// and the same Params.ChannelID (CPace CI). Params.AD is each party's own
// associated data; it travels in clear inside the message and is
// authenticated by the key exchange.
//
// A Session carries the intermediate session key ISK (64 bytes) and an
// explicit key-confirmation tag per draft section 10.4. Callers are expected
// to exchange and check the confirmation tags (AD-8 "confirmations") and to
// run ISK through a KDF before use (draft section 4, AD-6 step 6).
//
// The wire form of a CPace message is the 32-byte ristretto255 encoding of
// the public share Y followed by the associated data AD.
//
// This package is a kernel package (AD-1): it imports nothing from
// internal/, and it is the only importer of github.com/gtank/ristretto255.
//
// Owner: Story 1.7.
package pake
