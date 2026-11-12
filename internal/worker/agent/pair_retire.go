package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// Revoking a replaced Worker identity (AD-8, AD-9, Story 4.7).
//
// `minion worker pair --new-code` gives the Worker a new key. The old key is
// still enrolled at the old Hub, and anyone who kept a copy of it could connect
// as this Worker. So the old identity is retired at its Hub: the Worker
// connects once more with the OLD key, authenticated by the old pin like any
// connect (the handshake proves the key, nothing else is trusted), and asks the
// Hub to remove that enrollment with the catalog op [RetireOp]
// (ops.OpWorkerRetire, Hub scope, integrator amendment A-16) on a call stream.
// The Hub (internal/hub/workers, calls.go) removes the Worker row of the
// authenticated key, leaves a tombstone (AD-9) and ends the connection, so every
// later connect with the old key gets the signed revoke. Until then the old identity is kept in a private file in the state
// directory (a "pending revocation"): the Hub may be offline, the CLI may be run
// on a machine without the service, the service may be restarted. The record is
// deleted only after the Hub answered the request, or answered the old key's
// connect with a revoke it signed (already removed) or not_enrolled (it does not
// know the key, so there is nothing to revoke).

// RetireOp is the name of the catalog op (`op.request` on a `ca` stream) with
// which a Worker asks its Hub to remove the enrollment of the key that
// authenticated the connection. A Hub that does not implement it answers
// unsupported_op (or not at all), and the pending revocation stays.
const RetireOp = ops.OpWorkerRetire

const (
	retiredDirName = "retired"
	// retireCallTimeout bounds the wait for the Hub's answer to RetireOp.
	retireCallTimeout = 15 * time.Second
	// DefaultAdmittedRetry is the least wait before another attempt after a
	// connect that the Hub admitted ("welcome") but whose retirement it did
	// not carry out. Each such attempt makes the old identity appear online at
	// the Hub, so a Hub that does not support the op (an older release) must
	// not see it every minute.
	DefaultAdmittedRetry = 10 * time.Minute
	// DefaultRetirePoll is how often the running retirer looks for pending
	// revocations that another process (the CLI) added.
	DefaultRetirePoll = 30 * time.Second
)

// RetiredIdentity is an old Worker identity whose enrollment still has to be
// revoked at its Hub.
type RetiredIdentity struct {
	// Key is the old Worker key. It is a secret, kept in a private file.
	Key *identity.Key
	// HubURL is the Hub the old key was enrolled with.
	HubURL string
	// Pin is the Hub key the old enrollment pinned (the Hub is authenticated
	// by it when the revocation connects).
	Pin identity.Pin
	// WorkerID is the Worker id the Hub gave the old identity (informational).
	WorkerID string
	// MaxSerial and CertSHA256 are the certificate ratchet the enrollment had,
	// scoped to Pin.Pub (design section 12.5): carried here so that wiping the
	// enrollment does not reset it. RatchetSeeded is false for an entry written
	// by an earlier release (metadata version 1): it knows no serial until the
	// first validated retirement connection seeds it.
	MaxSerial     uint64
	CertSHA256    []byte
	RatchetSeeded bool
	// RetiredAt is when the identity was replaced.
	RetiredAt time.Time
}

// Fingerprint returns the fingerprint of the old key.
func (r RetiredIdentity) Fingerprint() string { return string(r.Key.Fingerprint()) }

type retiredMeta struct {
	Version   int          `json:"version"`
	HubURL    string       `json:"hub_url"`
	HubPub    identity.B64 `json:"hub_pub"`
	PinSeq    uint64       `json:"pin_seq"`
	WorkerID  string       `json:"worker_id,omitempty"`
	RetiredAt time.Time    `json:"retired_at"`
	// Version 2 carries the certificate ratchet of the wiped enrollment.
	MaxSerial  uint64       `json:"max_serial,omitempty"`
	CertSHA256 identity.B64 `json:"cert_sha256,omitempty"`
}

// RetiredStore holds the pending revocations: for each old identity a private
// key file and a private metadata file in the "retired" directory of the state
// directory.
type RetiredStore struct {
	dir string
	// write stores a private file durably (temporary file, sync, rename,
	// directory sync); tests replace it to inject a failure.
	write func(path string, data []byte) error
}

// NewRetiredStore returns the store of the state directory stateDir.
func NewRetiredStore(stateDir string) *RetiredStore {
	return &RetiredStore{dir: filepath.Join(stateDir, retiredDirName), write: identity.WritePrivate}
}

// RetiredStore returns the pending revocations of this Worker's state
// directory.
func (e *Enrollments) RetiredStore() *RetiredStore { return NewRetiredStore(e.stateDir) }

func (s *RetiredStore) paths(fp string) (keyPath, metaPath string) {
	return filepath.Join(s.dir, fp+".key"), filepath.Join(s.dir, fp+".json")
}

// retireLockWait bounds the wait for the lock of one pending revocation: its
// holder keeps it for one retirement connection (handshake, answer and at most
// retireCallTimeout).
const retireLockWait = 2 * time.Minute

// errRetiredGone is returned when the record of a pending revocation was
// removed (by another process) while the caller waited for its lock.
var errRetiredGone = errors.New("agent: the pending revocation was completed by another process")

// lock takes the cross-process lock of the pending revocation with fingerprint
// fp (the file <fp>.lock in the retired directory, created when missing and
// never removed, so that the lock is never lost to an unlink). Every process
// that reads a record in order to change it (Add, Advance, a retirement with
// its disposition and the removal) holds it from the reload to the last write.
// It is the innermost lock of the Worker (see enrollment_epoch.go).
func (s *RetiredStore) lock(ctx context.Context, fp string) (func(), error) {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: create %s: %w", s.dir, err)
	}
	ctx, cancel := context.WithTimeout(ctx, retireLockWait)
	defer cancel()
	unlock, err := lockFile(ctx, filepath.Join(s.dir, fp+".lock"))
	if err != nil {
		return nil, fmt.Errorf("agent: lock the pending revocation %s: %w", fp, err)
	}
	return unlock, nil
}

// Add stores r durably. The key goes first, the metadata last: a record exists
// only when both are complete. Adding the same identity again (a repeated
// replacement after a crash) replaces the record but never lowers a ratchet
// that a retirement connection advanced meanwhile under the same root.
func (s *RetiredStore) Add(r RetiredIdentity) error {
	if r.Key == nil || len(r.Pin.Pub) == 0 || r.HubURL == "" {
		return errors.New("agent: a retired identity needs its key, the Hub pin and the Hub URL")
	}
	fp := r.Fingerprint()
	unlock, err := s.lock(context.Background(), fp)
	if err != nil {
		return err
	}
	defer unlock()
	keyPath, metaPath := s.paths(fp)
	if cur, err := s.load(fp); err == nil {
		r.Pin, r.MaxSerial, r.CertSHA256 = mergeRatchet(cur, r.Pin, transport.Ratchet{MaxSerial: r.MaxSerial, CertSHA256: r.CertSHA256})
	}
	if err := identity.SaveKey(keyPath, r.Key); err != nil {
		return fmt.Errorf("agent: save the retired Worker key: %w", err)
	}
	if r.RetiredAt.IsZero() {
		r.RetiredAt = time.Now()
	}
	return s.writeMeta(metaPath, r)
}

// mergeRatchet returns the pin and ratchet to store when a record holding cur
// is to be updated with pin and rt: under the same root the serial never goes
// down (an equal serial keeps the stored hash, unless none is stored); a root
// further along the transition chain starts a new scope; anything else keeps
// cur.
func mergeRatchet(cur RetiredIdentity, pin identity.Pin, rt transport.Ratchet) (identity.Pin, uint64, []byte) {
	if bytes.Equal(cur.Pin.Pub, pin.Pub) {
		seq := max(cur.Pin.Seq, pin.Seq)
		if rt.MaxSerial > cur.MaxSerial || (rt.MaxSerial == cur.MaxSerial && len(cur.CertSHA256) == 0) {
			return identity.Pin{Pub: pin.Pub, Seq: seq}, rt.MaxSerial, rt.CertSHA256
		}
		return identity.Pin{Pub: cur.Pin.Pub, Seq: seq}, cur.MaxSerial, cur.CertSHA256
	}
	if pin.Seq > cur.Pin.Seq {
		return pin, rt.MaxSerial, rt.CertSHA256
	}
	return cur.Pin, cur.MaxSerial, cur.CertSHA256
}

// writeMeta stores r as version-2 metadata, durably (temporary file, sync,
// rename, directory sync).
func (s *RetiredStore) writeMeta(metaPath string, r RetiredIdentity) error {
	raw, err := json.Marshal(retiredMeta{
		Version: 2, HubURL: r.HubURL, HubPub: identity.B64(r.Pin.Pub), PinSeq: r.Pin.Seq,
		WorkerID: r.WorkerID, RetiredAt: r.RetiredAt.UTC(),
		MaxSerial: r.MaxSerial, CertSHA256: identity.B64(r.CertSHA256),
	})
	if err != nil {
		return err
	}
	if err := s.write(metaPath, raw); err != nil {
		return fmt.Errorf("agent: save the pending revocation: %w", err)
	}
	return nil
}

// Advance durably stores the ratchet (and the pin, when a chain transition moved
// it) that a validated retirement connection saw. It takes the record's lock,
// re-reads the record and never lowers it (see mergeRatchet): r may be a stale
// copy. live, when not nil, is called under the lock immediately before the
// write; when it reports false nothing is written (errGuardExpired). It
// rewrites the metadata as version 2 (the migration of a version-1 file happens
// here, at the seed). A failure leaves the previous file in place.
func (s *RetiredStore) Advance(r RetiredIdentity, pin identity.Pin, rt transport.Ratchet, live func() bool) error {
	fp := r.Fingerprint()
	unlock, err := s.lock(context.Background(), fp)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = s.advanceLocked(fp, pin, rt, live)
	return err
}

// advanceLocked is Advance for a caller that holds the record's lock. It
// returns the record as stored.
func (s *RetiredStore) advanceLocked(fp string, pin identity.Pin, rt transport.Ratchet, live func() bool) (RetiredIdentity, error) {
	cur, err := s.load(fp)
	if errors.Is(err, fs.ErrNotExist) {
		return RetiredIdentity{}, errRetiredGone
	}
	if err != nil {
		return RetiredIdentity{}, err
	}
	next := cur
	next.Pin, next.MaxSerial, next.CertSHA256 = mergeRatchet(cur, pin, rt)
	next.RatchetSeeded = true
	if live != nil && !live() {
		return RetiredIdentity{}, errGuardExpired
	}
	_, metaPath := s.paths(fp)
	if err := s.writeMeta(metaPath, next); err != nil {
		return RetiredIdentity{}, err
	}
	return next, nil
}

// List returns the pending revocations, oldest first. A record that cannot be
// read is reported in the error and skipped; the others are still returned.
func (s *RetiredStore) List() ([]RetiredIdentity, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent: read %s: %w", s.dir, err)
	}
	var out []RetiredIdentity
	var errs []error
	for _, ent := range entries {
		fp, ok := strings.CutSuffix(ent.Name(), ".json")
		if !ok || ent.IsDir() {
			continue
		}
		r, err := s.load(fp)
		if err != nil {
			errs = append(errs, fmt.Errorf("pending revocation %s: %w", fp, err))
			continue
		}
		out = append(out, r)
	}
	for i := 1; i < len(out); i++ { // insertion sort: a handful of records
		for j := i; j > 0 && out[j].RetiredAt.Before(out[j-1].RetiredAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, errors.Join(errs...)
}

func (s *RetiredStore) load(fp string) (RetiredIdentity, error) {
	keyPath, metaPath := s.paths(fp)
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return RetiredIdentity{}, err
	}
	var m retiredMeta
	if err := json.Unmarshal(raw, &m); err != nil || (m.Version != 1 && m.Version != 2) {
		return RetiredIdentity{}, errors.New("unreadable metadata")
	}
	if len(m.CertSHA256) != 0 && len(m.CertSHA256) != 32 {
		return RetiredIdentity{}, errors.New("unreadable metadata")
	}
	k, err := identity.LoadKey(keyPath)
	if err != nil {
		return RetiredIdentity{}, fmt.Errorf("load the key: %w", err)
	}
	return RetiredIdentity{
		Key: k, HubURL: m.HubURL, Pin: identity.Pin{Pub: ed25519.PublicKey(m.HubPub), Seq: m.PinSeq}, WorkerID: m.WorkerID, RetiredAt: m.RetiredAt,
		MaxSerial: m.MaxSerial, CertSHA256: []byte(m.CertSHA256), RatchetSeeded: m.Version >= 2,
	}, nil
}

// Remove deletes the record of the identity with fingerprint fp. The metadata
// goes first, so a crash never leaves a record without its key.
func (s *RetiredStore) Remove(fp string) error {
	keyPath, metaPath := s.paths(fp)
	for _, p := range []string{metaPath, keyPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// RetirerConfig configures a [Retirer]. Store and Dialer are required.
type RetirerConfig struct {
	Store *RetiredStore
	// Dialer is the connect transport (the same port as the connector's).
	Dialer Dialer
	// Name is the Worker's display name for the hello.
	Name string
	// Protocols is the supported protocol range; zero means proto.Current.
	Protocols proto.Range
	// Session tunes the yamux session.
	Session transport.SessionOptions
	// WallMono supplies the wall and monotonic clock of the certificate window;
	// nil means the system clocks.
	WallMono transport.WallMono
	// Clock and Backoff pace the retries; nil means the system clock and the
	// default sequence.
	Clock   transport.Clock
	Backoff *transport.Backoff
	// Poll is how often [Retirer.Run] looks for new pending revocations; zero
	// means [DefaultRetirePoll].
	Poll time.Duration
	// AdmittedRetry is the least wait after an attempt that was admitted but
	// not retired; zero means [DefaultAdmittedRetry].
	AdmittedRetry time.Duration
	// Logger receives operational messages, never key material. Nil discards
	// them.
	Logger *slog.Logger
}

// Retirer carries out the pending revocations of replaced Worker identities.
type Retirer struct {
	cfg RetirerConfig
	log *slog.Logger
	clk transport.Clock
	bo  *transport.Backoff

	mu       sync.Mutex
	lastFail map[string]string // fingerprint -> last failure that was logged at Warn
	admitted bool              // the last RunOnce had an attempt that was admitted but not retired
}

// admittedError marks an attempt that the Hub admitted (welcome) but that did
// not end in a retirement.
type admittedError struct{ err error }

func (e *admittedError) Error() string { return e.err.Error() }
func (e *admittedError) Unwrap() error { return e.err }

// NewRetirer validates cfg and returns a retirer.
func NewRetirer(cfg RetirerConfig) (*Retirer, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("agent: the retirer needs the store of pending revocations")
	case cfg.Dialer == nil:
		return nil, errors.New("agent: the retirer needs a dialer")
	}
	r := &Retirer{cfg: cfg, log: cfg.Logger, clk: cfg.Clock, bo: cfg.Backoff, lastFail: map[string]string{}}
	if r.log == nil {
		r.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if r.clk == nil {
		r.clk = transport.SystemClock
	}
	if r.bo == nil {
		r.bo = &transport.Backoff{}
	}
	if r.cfg.Poll <= 0 {
		r.cfg.Poll = DefaultRetirePoll
	}
	if r.cfg.AdmittedRetry <= 0 {
		r.cfg.AdmittedRetry = DefaultAdmittedRetry
	}
	return r, nil
}

// RunOnce makes one attempt for every pending revocation and returns how many
// are still pending (including records it could not read). A revocation that
// the Hub confirmed is deleted.
func (r *Retirer) RunOnce(ctx context.Context) (remaining int, err error) {
	r.setAdmitted(false)
	list, listErr := r.cfg.Store.List()
	if listErr != nil {
		r.log.Warn("some pending revocations cannot be read", "error", listErr.Error())
		remaining++
	}
	for _, listed := range list {
		if ctx.Err() != nil {
			return remaining + len(list), ctx.Err()
		}
		fp := listed.Fingerprint()
		id, done, err := r.retireLocked(ctx, fp)
		if errors.Is(err, errRetiredGone) {
			// Another process (the CLI or the service) completed it meanwhile.
			r.clearFail(fp)
			continue
		}
		if err != nil {
			remaining++
			var ae *admittedError
			if errors.As(err, &ae) {
				r.setAdmitted(true)
			}
			r.logFail(fp, listed, err)
			continue
		}
		r.clearFail(fp)
		r.log.Info("revoked the old Worker identity at the hub", "hub", hubLabel(id.HubURL), "old_fingerprint", fp, "result", done)
	}
	return remaining, nil
}

// retireLocked serializes the retirement of one identity across processes: it
// holds the record's lock from the reload through the connection, the
// durable ratchet advance and the disposition to the removal of the record. A
// listing taken before the lock is never acted on: the record is read again
// under the lock, and errRetiredGone reports one that another process removed.
func (r *Retirer) retireLocked(ctx context.Context, fp string) (RetiredIdentity, string, error) {
	unlock, err := r.cfg.Store.lock(ctx, fp)
	if err != nil {
		return RetiredIdentity{}, "", err
	}
	defer unlock()
	id, err := r.cfg.Store.load(fp)
	if errors.Is(err, fs.ErrNotExist) {
		return RetiredIdentity{}, "", errRetiredGone
	}
	if err != nil {
		return RetiredIdentity{}, "", fmt.Errorf("read the record: %w", err)
	}
	if id.Key == nil || len(id.Pin.Pub) == 0 || id.HubURL == "" {
		return id, "", errors.New("the pending revocation is incomplete")
	}
	done, err := r.retireOne(ctx, id)
	if err != nil {
		return id, "", err
	}
	if err := r.cfg.Store.Remove(fp); err != nil {
		return id, "", fmt.Errorf("delete the record: %w", err)
	}
	return id, done, nil
}

// Run carries out pending revocations until ctx ends: a failing attempt is
// retried with the AD-3 backoff without any time limit (and, when the Hub
// admitted the old key but did not retire it, after at least
// [RetirerConfig.AdmittedRetry]), and with nothing pending it looks again after
// the poll interval, because the CLI adds records from another process.
func (r *Retirer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		remaining, err := r.RunOnce(ctx)
		if err != nil {
			return
		}
		var werr error
		if remaining == 0 {
			r.bo.Reset()
			select {
			case <-r.clk.After(r.cfg.Poll):
			case <-ctx.Done():
				werr = ctx.Err()
			}
		} else {
			d := r.bo.Next()
			if r.wasAdmitted() {
				d = max(d, r.cfg.AdmittedRetry)
			}
			select {
			case <-r.clk.After(d):
			case <-ctx.Done():
				werr = ctx.Err()
			}
		}
		if werr != nil {
			return
		}
	}
}

func (r *Retirer) setAdmitted(v bool) {
	r.mu.Lock()
	r.admitted = v
	r.mu.Unlock()
}

func (r *Retirer) wasAdmitted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.admitted
}

func (r *Retirer) logFail(fp string, id RetiredIdentity, err error) {
	msg := err.Error()
	r.mu.Lock()
	repeat := r.lastFail[fp] == msg
	r.lastFail[fp] = msg
	r.mu.Unlock()
	if repeat {
		r.log.Debug("revoking the old Worker identity failed again", "old_fingerprint", fp, "error", msg)
		return
	}
	r.log.Warn("cannot revoke the old Worker identity at the hub yet; retrying with backoff",
		"hub", hubLabel(id.HubURL), "old_fingerprint", fp, "error", msg)
}

func (r *Retirer) clearFail(fp string) {
	r.mu.Lock()
	delete(r.lastFail, fp)
	r.mu.Unlock()
}

// errRetireExpired is the failure of a retirement session whose Hub certificate
// reached its not_after before the disposition was final. The entry stays
// pending.
var errRetireExpired = errors.New("the Hub certificate expired before the answer was accepted; the pending revocation is kept")

// retireOne connects with the old key and revokes its enrollment. It returns a
// short result word for the log, or an error when the revocation is still
// pending.
func (r *Retirer) retireOne(ctx context.Context, id RetiredIdentity) (string, error) {
	att, err := r.cfg.Dialer.Connect(ctx, ConnectParams{
		HubURL:    id.HubURL,
		Key:       id.Key,
		Name:      r.cfg.Name,
		Protocols: r.cfg.Protocols,
		Pin:       id.Pin,
		Ratchet:   transport.Ratchet{Root: id.Pin.Pub, MaxSerial: id.MaxSerial, CertSHA256: id.CertSHA256},
		Clock:     r.cfg.WallMono,
		Session:   r.cfg.Session,
	})
	if err != nil {
		return "", fmt.Errorf("connect with the old key: %w", err)
	}
	// Nothing is accepted after the certificate's not_after (section 11.3): the
	// session is guarded until the disposition is final, and both clocks are read
	// again before each one is accepted.
	var guard *expiryGuard
	if att.Expiry != nil {
		wm := r.cfg.WallMono
		if wm == nil {
			wm = transport.SystemWallMono
		}
		guard = &expiryGuard{exp: *att.Expiry, clk: wm, log: r.log, onExpire: func() {
			if att.Session != nil {
				_ = att.Session.Close()
			}
		}}
		wctx, stopWatch := context.WithCancel(context.WithoutCancel(ctx))
		defer stopWatch()
		go guard.watch(wctx, r.clk)
		if guard.check() {
			return "", errRetireExpired
		}
	}
	// The handshake passed every check (chain, root, certificate, window, serial
	// scope, sig_hub). A higher serial (or the first hash at an equal serial),
	// and the first validated connection of a version-1 entry, are stored
	// durably BEFORE the Worker acts on revoke, not_enrolled or welcome. A failed
	// write ends the connection and keeps the entry.
	// The caller holds the record's lock, and the record is re-read inside it
	// right before the write, with both clocks read again immediately before
	// it.
	first := !id.RatchetSeeded
	if first || att.PinChanged || att.RatchetChanged {
		live := func() bool { return !guard.check() }
		if _, err := r.cfg.Store.advanceLocked(id.Fingerprint(), att.Pin, att.Ratchet, live); err != nil {
			if att.Session != nil {
				_ = att.Session.Close()
			}
			if errors.Is(err, errGuardExpired) {
				return "", errRetireExpired
			}
			return "", fmt.Errorf("cannot store the certificate ratchet; the pending revocation is kept: %w", err)
		}
	}
	switch att.Reply.Type {
	case proto.TypeRevoke:
		if err := identity.Verify(att.OpPub, identity.PurposeRevoke, att.TranscriptHash, att.Reply.RevokeSig); err != nil {
			return "", errors.New("the hub answered with a revoke that the operational key of this connection did not sign for it")
		}
		if guard.check() {
			return "", errRetireExpired
		}
		return "already_revoked", nil
	case proto.TypeNotEnrolled:
		if first {
			// Seeded just now: a not_enrolled cannot retire an entry that had no
			// memory of the root's earlier serials. A later validated
			// connection with serial >= the seed may retire it.
			return "", errors.New("not_enrolled on the first validated connection after the upgrade; the entry stays pending until a later validated connection")
		}
		if guard.check() {
			return "", errRetireExpired
		}
		return "not_enrolled", nil
	case proto.TypeWelcome:
		if att.Session == nil {
			return "", errors.New("welcome without a session")
		}
		// Ending ctx aborts everything below at once: the session is closed,
		// which fails the open and the read, so a stop is never held up by a
		// Hub that does not answer.
		stop := context.AfterFunc(ctx, func() { _ = att.Session.Close() })
		defer func() {
			stop()
			_ = att.Session.Close()
		}()
		// Nothing is sent on the authenticated session after not_after.
		if guard.check() {
			return "", &admittedError{errRetireExpired}
		}
		if err := retireCall(ctx, att.Session); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", &admittedError{err}
		}
		if guard.check() {
			return "", &admittedError{errRetireExpired}
		}
		return "retired", nil
	}
	if att.Session != nil {
		_ = att.Session.Close()
	}
	return "", fmt.Errorf("unexpected control frame %q", att.Reply.Type)
}

// retireCall asks the Hub, on a call stream of the connection that the old key
// authenticated, to remove that key's enrollment. It returns when the Hub
// answered, [retireCallTimeout] passed or ctx ended (the stream is reset).
func retireCall(ctx context.Context, sess Session) error {
	st, err := sess.OpenCall(ctx)
	if err != nil {
		return fmt.Errorf("open the call stream: %w", err)
	}
	defer st.Reset()
	// ctx ending aborts a blocked write or read of the stream at once.
	stop := context.AfterFunc(ctx, func() {
		_ = st.SetDeadline(time.Now())
		st.Reset()
	})
	defer stop()
	_ = st.SetDeadline(time.Now().Add(retireCallTimeout))
	id := ops.NewUUIDv7()
	args := ops.WorkerRetireArgs{WriteArgs: ops.WriteArgs{RequestID: id}}
	if err := st.WriteFrame(map[string]any{"type": proto.TypeOpRequest, "op": RetireOp, "request_id": id, "args": args}); err != nil {
		return fmt.Errorf("send %s: %w", RetireOp, err)
	}
	body, err := st.ReadFrame()
	if err != nil {
		return fmt.Errorf("read the answer to %s: %w", RetireOp, err)
	}
	typ, err := proto.FrameType(body)
	if err != nil {
		return err
	}
	switch typ {
	case proto.TypeOpResult:
		var f struct {
			Result ops.WriteResult `json:"result"`
		}
		if err := proto.DecodeFrame(body, &f); err != nil {
			return err
		}
		if f.Result.Outcome != ops.OutcomeDelivered {
			return fmt.Errorf("the hub answered %s with outcome %q (%s)", RetireOp, f.Result.Outcome, f.Result.Reason)
		}
		return nil
	case proto.TypeError:
		var f proto.ErrorFrame
		if err := proto.DecodeFrame(body, &f); err != nil {
			return err
		}
		return f.Err()
	}
	return fmt.Errorf("the hub answered %s with a %q frame", RetireOp, typ)
}
