package authz

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TrustRecord is one stored mark of a receiver.
type TrustRecord struct {
	WorkerID string
	// SessionName is empty for a mark that covers the whole Worker.
	SessionName string
	CreatedAt   int64 // unix milliseconds
}

// TrustStore is the port to the trust table (AD-16: the authz domain owns
// it); the Hub composition adapts the store to it, as for the user and Worker
// lookups, so this package imports no storage. Every read hits the table;
// nothing is cached. Times are unix milliseconds.
type TrustStore interface {
	PutTrust(ctx context.Context, receiverID, workerID, sessionName string, createdAt int64) error
	// PutTrustIfOwner stores the mark only while the Worker belongs to
	// expectedOwnerID, atomically with the check, and returns
	// [ErrOwnerChanged] otherwise ([ErrNotFound] for an unknown Worker).
	PutTrustIfOwner(ctx context.Context, receiverID, workerID, sessionName string, createdAt int64, expectedOwnerID string) error
	WorkerOwner(ctx context.Context, workerID string) (string, error)
	DeleteTrust(ctx context.Context, receiverID, workerID, sessionName string) (bool, error)
	HasTrust(ctx context.Context, receiverID, workerID, sessionName string) (bool, error)
	ListTrust(ctx context.Context, receiverID string) ([]TrustRecord, error)
}

var (
	// ErrTrustInvalid is returned for a trust call without a receiver or Worker.
	ErrTrustInvalid = errors.New("authz: a trust call needs a receiver and a Worker")
	// ErrOwnerChanged is returned by [Trust.SetAuthorized] when the Worker was
	// reassigned after the call was decided.
	ErrOwnerChanged = errors.New("authz: the Worker changed owner")
)

// Trust records which sources a user reads as trusted (AD-17, R29). It is a
// marker for a model reading output and nothing else:
//
//   - it never takes part in [Authorizer.Decide], so it cannot restrict
//     anybody, an admin least of all: whoever may read a session sees all its
//     output whether or not they trust it;
//   - it is stored per receiver, so one user's trust never makes a source
//     trusted for another;
//   - a reassignment or a removal of the Worker deletes every row of the
//     Worker and its sessions, the admin's included (the store does it in the
//     transaction of the change).
//
// Every method needs the receiver explicitly: the caller derives it with
// [TrustReceiver].
type Trust struct {
	st  TrustStore
	now func() time.Time
}

// NewTrust returns a Trust over st. now is the clock for created_at; nil
// means time.Now.
func NewTrust(st TrustStore, now func() time.Time) *Trust {
	if st == nil {
		panic("authz: NewTrust needs a store")
	}
	if now == nil {
		now = time.Now
	}
	return &Trust{st: st, now: now}
}

// TrustReceiver returns the user whose trust applies to p: the admin who acts
// as another user (trust is evaluated for the actor, never for the user acted
// as, AD-17), otherwise the user of the principal. A Worker identity has none:
// the result is empty and every Trust call refuses it.
func TrustReceiver(p Principal) string {
	if p.Kind == KindActAs {
		return p.ActorID
	}
	return p.UserID
}

// Set marks the Worker, or one of its sessions when sessionName is not empty,
// as trusted by receiver. Setting an existing row changes nothing.
func (t *Trust) Set(ctx context.Context, receiverID, workerID, sessionName string) error {
	if receiverID == "" || workerID == "" {
		return ErrTrustInvalid
	}
	return t.st.PutTrust(ctx, receiverID, workerID, sessionName, t.now().UnixMilli())
}

// Owner returns the current owner of the Worker (the ownership generation of
// a trust call: a reassignment changes it and deletes every mark of the
// Worker). A request captures it before its first authorization decision and
// hands it to [Trust.SetUnder]. It returns [ErrNotFound] for an unknown
// Worker.
func (t *Trust) Owner(ctx context.Context, workerID string) (string, error) {
	if workerID == "" {
		return "", ErrTrustInvalid
	}
	return t.st.WorkerOwner(ctx, workerID)
}

// SetAuthorized is [Trust.SetUnder] for a caller that did not capture the
// owner before its first decision: it reads the owner now. A reassignment that
// lands before this read is not seen as a change, so a caller that decided
// earlier must use [Trust.Owner] and [Trust.SetUnder] instead.
func (t *Trust) SetAuthorized(ctx context.Context, receiverID, workerID, sessionName string, recheck func(context.Context) error) error {
	if receiverID == "" || workerID == "" {
		return ErrTrustInvalid
	}
	owner, err := t.st.WorkerOwner(ctx, workerID)
	if err != nil {
		return err
	}
	return t.SetUnder(ctx, receiverID, workerID, sessionName, owner, recheck)
}

// SetUnder is [Trust.Set] for a call whose authorization was decided under
// the owner expectedOwnerID, captured with [Trust.Owner] before the first
// decision. It calls recheck (which decides the call again against the
// current state) and stores the mark only if the Worker still has that owner,
// atomically with the insert. Whenever a reassignment lands after the capture
// (before the first decision, before recheck or right before the insert), the
// insert fails with [ErrOwnerChanged], so a mark never survives a
// reassignment it was not decided under, whoever the caller is (an admin
// passes any decision). Any error of recheck is returned and stores nothing.
func (t *Trust) SetUnder(ctx context.Context, receiverID, workerID, sessionName, expectedOwnerID string, recheck func(context.Context) error) error {
	if receiverID == "" || workerID == "" || expectedOwnerID == "" {
		return ErrTrustInvalid
	}
	if recheck != nil {
		if err := recheck(ctx); err != nil {
			return err
		}
	}
	return t.st.PutTrustIfOwner(ctx, receiverID, workerID, sessionName, t.now().UnixMilli(), expectedOwnerID)
}

// Remove deletes exactly one mark and reports whether it existed. Removing the
// whole-Worker mark (sessionName empty) keeps the marks of single sessions.
func (t *Trust) Remove(ctx context.Context, receiverID, workerID, sessionName string) (bool, error) {
	if receiverID == "" || workerID == "" {
		return false, ErrTrustInvalid
	}
	return t.st.DeleteTrust(ctx, receiverID, workerID, sessionName)
}

// Trusted reports whether receiver reads the output of the session as
// trusted: a mark for the session or one for its whole Worker. A failure of
// the lookup is returned and counts as not trusted (fail closed).
func (t *Trust) Trusted(ctx context.Context, receiverID, workerID, sessionName string) (bool, error) {
	if receiverID == "" || workerID == "" {
		return false, nil
	}
	ok, err := t.st.HasTrust(ctx, receiverID, workerID, sessionName)
	if err != nil {
		return false, fmt.Errorf("authz: read trust: %w", err)
	}
	return ok, nil
}

// Entry is one mark of a receiver.
type Entry struct {
	WorkerID string
	// SessionName is empty for a mark that covers the whole Worker.
	SessionName string
	CreatedAt   time.Time
}

// List returns the marks of one receiver, ordered by Worker and session.
func (t *Trust) List(ctx context.Context, receiverID string) ([]Entry, error) {
	if receiverID == "" {
		return nil, ErrTrustInvalid
	}
	rows, err := t.st.ListTrust(ctx, receiverID)
	if err != nil {
		return nil, fmt.Errorf("authz: list trust: %w", err)
	}
	out := make([]Entry, len(rows))
	for i, r := range rows {
		out[i] = Entry{WorkerID: r.WorkerID, SessionName: r.SessionName, CreatedAt: time.UnixMilli(r.CreatedAt)}
	}
	return out, nil
}
