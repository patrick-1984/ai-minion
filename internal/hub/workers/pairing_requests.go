package workers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Pairing requests (v0.12.0, design note 08-pairing-requests).
//
// A Worker that waits with a pairing code is also a pairing request: the Hub
// lists it (announced name, client address, waiting since, the fingerprint of
// the Worker key of its hello and the public pairing id) and an administrator
// admits it with one click instead of typing the code. The blueprint is
// NetBird's peer approval combined with the fingerprint comparison of SSH host
// keys and Signal safety numbers: the Worker's console shows its own words and
// the words of the Hub that answered, the Hub's admit dialog shows the same
// two, and the administrator admits only when they agree. Nothing new is
// computed: an admission sends pair.admit, the Hub signs the transcript with
// its certified operational key as in every mode (sig_hub), and the Worker
// proves the very key of its hello with sig_worker. The Hub enrolls only that
// key, and only when the fingerprint the administrator saw names it.
//
// Requests are the pending table itself, so the limits of pending
// connections apply (in total and per client address), they expire with the
// code and the sweep purges them.

// PairingRequest is one waiting Worker as the Pairing requests list shows it.
type PairingRequest struct {
	// PairingID is the public pairing id (the first four characters of the
	// code), which identifies the request.
	PairingID string
	// Name is the name the Worker announced, made a valid Worker name.
	Name string
	// Addr is the client address the Worker connected from.
	Addr string
	// Since is when the Worker started to wait; Expires when its code ends.
	Since   time.Time
	Expires time.Time
	// WorkerFingerprint is the fingerprint of the Worker key of the hello.
	WorkerFingerprint identity.Fingerprint
	// HubFingerprint is the fingerprint of the Hub root this connection's
	// hello carried: the words the Worker's box shows.
	HubFingerprint identity.Fingerprint
}

// AdmitRequest is one admission of a pairing request.
type AdmitRequest struct {
	// UserID owns the new Worker: the admitting user.
	UserID string
	// PairingID names the request; Fingerprint must be the fingerprint of the
	// Worker key the request presents (the value the dialog showed), so that
	// a request that was replaced meanwhile is never admitted.
	PairingID   string
	Fingerprint string
	// HubFingerprint must be the fingerprint of the Hub root the request's
	// handshake presented (the words the administrator compared).
	HubFingerprint string
	// Name optionally overrides the announced name.
	Name string
	// Approve approves the new Worker at once (the caller may approve it).
	Approve bool
	// Actor and ActingAs are the audit identity of the call.
	Actor    string
	ActingAs string
}

// RejectRequest is one rejection of a pairing request.
type RejectRequest struct {
	PairingID   string
	Fingerprint string
	Actor       string
	ActingAs    string
}

// errRejected is what the Worker of a rejected request is told. Only the reason
// token reaches its operator (the Worker does not trust the Hub yet).
func errRejected() *proto.Error {
	e := pairingErr(proto.CodePairingCodeInvalid, "the pairing request was rejected on the Hub")
	e.Details = json.RawMessage(`{"reason":"rejected"}`)
	return e
}

func errNoRequest() *proto.Error {
	return pairingErr(proto.CodeNotFound, "no Worker waits with this pairing id any more; refresh the list")
}

func errRequestChanged() *proto.Error {
	return pairingErr(proto.CodeConflict, "the Worker waiting with this pairing id presents another key than the one you checked; nothing was admitted, refresh the list and compare again")
}

// admitDigestPrefix marks the used-code record of an admission: no code was
// typed, so the record names the admission instead of a code digest.
const admitDigestPrefix = "admit:"

// admitDigest is the unique key of the used-code record of an admission.
func admitDigest(pairingID string, pub []byte) string {
	var nonce [16]byte
	_, _ = rand.Read(nonce[:])
	h := sha256.New()
	h.Write([]byte("ai-minion pairing admit v1\x00" + pairingID + "\x00"))
	h.Write(pub)
	h.Write(nonce[:])
	return admitDigestPrefix + hex.EncodeToString(h.Sum(nil))
}

func isAdmitDigest(d string) bool { return strings.HasPrefix(d, admitDigestPrefix) }

// fingerprintOfPub is the fingerprint of a raw key, "" for a malformed one.
func fingerprintOfPub(pub []byte) identity.Fingerprint {
	if len(pub) != ed25519.PublicKeySize {
		return ""
	}
	fp, err := identity.FingerprintOf(ed25519.PublicKey(pub))
	if err != nil {
		return ""
	}
	return fp
}

// sameFingerprint compares a fingerprint as given (52 characters, any case,
// optional hyphens or spaces are not accepted: the copy value is exact) with
// the key's.
func sameFingerprint(given string, pub []byte) bool {
	fp := fingerprintOfPub(pub)
	return fp != "" && strings.EqualFold(strings.TrimSpace(given), string(fp))
}

// listable reports whether p is an open request under ps.mu.
//
// A Worker that comes back to a pairing the Hub already committed (p.resume) is
// never a request: it recovers with its owner's code, never by an admission.
// Nor is a connection whose handshake used an identity generation that is no
// longer admitted (the Hub rotated since).
func (ps *Pairings) listable(p *pending, now time.Time) bool {
	if p.ended || p.cur != nil || !isReady(p) || !now.Before(p.expires) || !p.req.Admittable || p.resume != nil {
		return false
	}
	if p.req.Current != nil && !p.req.Current() {
		return false
	}
	until, hidden := ps.rejected[hex.EncodeToString(p.req.Pub)]
	return !hidden || !now.Before(until)
}

// Requests returns the open pairing requests, oldest first: Workers that wait
// with a code, announced that they accept an admission and are not being
// claimed right now.
func (ps *Pairings) Requests() []PairingRequest {
	now := ps.now()
	ps.mu.Lock()
	out := make([]PairingRequest, 0, len(ps.live))
	for _, p := range ps.live {
		if !ps.listable(p, now) {
			continue
		}
		out = append(out, PairingRequest{
			PairingID: p.id, Name: CleanWorkerName(p.req.Name), Addr: p.addr, Since: p.since, Expires: p.expires,
			WorkerFingerprint: fingerprintOfPub(p.req.Pub), HubFingerprint: fingerprintOfPub(p.req.HubRoot),
		})
	}
	ps.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		return out[i].PairingID < out[j].PairingID
	})
	return out
}

// openRequest returns the live request of id whose key has the fingerprint,
// under ps.mu.
func (ps *Pairings) openRequestLocked(id, fingerprint string, now time.Time) (*pending, error) {
	canon, ok := identity.CanonicalPairingID(id)
	if !ok {
		return nil, errNoRequest()
	}
	p := ps.live[canon]
	if p == nil || !ps.listable(p, now) {
		if p != nil && p.cur != nil {
			return nil, pairingErr(proto.CodeConflict, "the code of this Worker is being checked right now; try again in a moment")
		}
		return nil, errNoRequest()
	}
	if !sameFingerprint(fingerprint, p.req.Pub) {
		return nil, errRequestChanged()
	}
	return p, nil
}

// Admit admits a pairing request: the waiting connection continues with
// pair.admit instead of CPace, and the Worker is created for req.UserID with
// exactly the key of the request (sig_worker proves it). It returns once the
// Worker is created, or fails with not_found (no such request any more),
// conflict (another key waits under the id, or a claim is running) or the
// error that ended the handshake.
func (ps *Pairings) Admit(ctx context.Context, req AdmitRequest) (ClaimResult, error) {
	if req.UserID == "" {
		return ClaimResult{}, pairingErr(proto.CodeForbidden, "not allowed")
	}
	actor := req.Actor
	if actor == "" {
		actor = req.UserID
	}
	ps.mu.Lock()
	if ps.closed {
		ps.mu.Unlock()
		return ClaimResult{}, pairingErr(proto.CodeWorkerOffline, "the Hub is shutting down")
	}
	p, err := ps.openRequestLocked(req.PairingID, req.Fingerprint, ps.now())
	if err != nil {
		ps.mu.Unlock()
		return ClaimResult{}, err
	}
	if !sameFingerprint(req.HubFingerprint, p.req.HubRoot) {
		ps.mu.Unlock()
		return ClaimResult{}, pairingErr(proto.CodeConflict, "this Worker was answered with another Hub key than the one you compared; nothing was admitted, refresh the list and compare again")
	}
	c := &claim{
		userID: req.UserID, actor: actor, actingAs: req.ActingAs, name: req.Name,
		admit: true, approve: req.Approve, digest: admitDigest(p.id, p.req.Pub), done: make(chan claimOutcome, 1),
	}
	p.cur = c
	p.claims <- c // capacity 1 and p.cur was nil, so this never blocks
	ps.mu.Unlock()

	select {
	case o := <-c.done:
		return o.res, o.err
	case <-ctx.Done():
		return ClaimResult{}, ctx.Err()
	}
}

// Reject removes a pairing request: its Worker is told that the request was
// rejected and shows a new code, and requests of the same key stay hidden from
// the list for one code lifetime. The key can still pair with its code.
func (ps *Pairings) Reject(ctx context.Context, req RejectRequest) error {
	now := ps.now()
	ps.mu.Lock()
	p, err := ps.openRequestLocked(req.PairingID, req.Fingerprint, now)
	if err != nil {
		ps.mu.Unlock()
		return err
	}
	if len(ps.rejected) < ps.maxPending {
		ps.rejected[hex.EncodeToString(p.req.Pub)] = now.Add(ps.ttl)
	}
	ps.endLocked(p, errRejected())
	id := p.id
	ps.mu.Unlock()
	actor := req.Actor
	if actor == "" {
		actor = "unknown"
	}
	ps.record(ctx, audit.Event{Actor: actor, ActingAs: req.ActingAs, Action: AuditPairingReject, Target: id, Outcome: "rejected"})
	return nil
}
