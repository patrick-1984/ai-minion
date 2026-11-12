package workers

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/ratelimit"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// InviteSecretFrame is the frame a Worker sends inside the secure channel in
// enroll_invite mode, after it verified the Hub against the fingerprint of the
// invitation key (AD-6, AD-7):
//
//	{"type":"invite.secret","secret":"<52 lowercase base32 characters>"}
//
// Secret is the part of mnk_<fingerprint>.<secret> after the dot. The whole
// key is accepted as well, as long as its fingerprint is the Hub's.
type InviteSecretFrame struct {
	Type   string `json:"type"`
	Secret string `json:"secret"`
}

// Limits of a Worker name derived from the hello.
const (
	// maxEnrollNameLen leaves room for the "-N" collision suffix inside the
	// 64-byte Worker name limit of the catalog.
	maxEnrollNameLen  = 56
	defaultWorkerName = "worker"
)

// EnrollStore is the part of the database the enrollment needs; *store.Store
// implements it.
type EnrollStore interface {
	EnrollWithInvitation(ctx context.Context, p store.EnrollParams) (store.EnrollResult, error)
	EnsureInfraKey(ctx context.Context, hash []byte, expiresAt, nowMS int64) (store.InvitationRow, error)
	InfraOwnerID(ctx context.Context, name string) (string, error)
}

var _ EnrollStore = (*store.Store)(nil)

// EnrollConfig configures [NewEnroller]. Store and HubFingerprint are
// required.
type EnrollConfig struct {
	Store EnrollStore
	// HubFingerprint is the Hub identity fingerprint named by every
	// invitation key.
	HubFingerprint identity.Fingerprint
	// FingerprintFunc, when set, supplies the ROOT fingerprint at the time of
	// each call and replaces HubFingerprint.
	FingerprintFunc func() identity.Fingerprint
	// Limiter applies the enroll policy (AD-19) by client IP and Hub
	// fingerprint before the secret is read; nil disables it. The Hub's
	// Service passes its limiter.
	Limiter Limiter
	// Auditor records refused enrollments; nil disables it. Successful
	// enrollments are audited in the enrollment transaction itself.
	Auditor Auditor
	// Infra is the Hub-wide infrastructure key (MINION_INFRA_INVITE_KEY_FILE),
	// or nil when none is configured.
	Infra *InfraKey
	// InfraExpiry is how long the key file is valid from its first load (the
	// default is [DefaultInfraKeyLifetime] when zero is not meant; use
	// [NoInfraExpiry] for none). The first load stores the expiry in the
	// key's row; a later load never changes it.
	InfraExpiry time.Duration
	// InfraOwner is infra_default_owner: the user name (or e-mail address)
	// that owns Workers enrolled with Infra. Empty means the seeded admin.
	InfraOwner string
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Enroller serves the enroll_invite handshake mode (AD-7). Register it with
// Handler.Handle(transport.ModeEnrollInvite, enroller).
type Enroller struct {
	cfg EnrollConfig
	now func() time.Time
	log *slog.Logger
}

var _ ModeHandler = (*Enroller)(nil)

// NewEnroller validates cfg and returns the enroll_invite handler.
func NewEnroller(cfg EnrollConfig) (*Enroller, error) {
	if cfg.Store == nil {
		return nil, errors.New("workers: the enroller needs a store")
	}
	if cfg.FingerprintFunc == nil {
		fpr, err := identity.ParseFingerprint(string(cfg.HubFingerprint))
		if err != nil {
			return nil, errors.New("workers: the enroller needs the Hub fingerprint")
		}
		cfg.FingerprintFunc = func() identity.Fingerprint { return fpr }
	}
	e := &Enroller{cfg: cfg, now: cfg.Now, log: cfg.Logger}
	if e.now == nil {
		e.now = time.Now
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	if cfg.Infra != nil {
		if err := e.loadInfra(); err != nil {
			return nil, err
		}
	}
	return e, nil
}

// Infrastructure key file lifetime (F-02). The key file is stored as a row keyed
// by the hash of its secret; the first load gives it an expiry, a reload never
// changes the row.
const (
	// DefaultInfraKeyLifetime is the lifetime of a new key file row when the
	// setting infra_invite_key_days is left at its default (30 days).
	DefaultInfraKeyLifetime = 30 * 24 * time.Hour
	// NoInfraExpiry as InfraExpiry stores a key file row without expiry (the
	// Hub warns at every start).
	NoInfraExpiry time.Duration = -1
)

// loadInfra records the key file in the database. An existing row is left as it
// is: its expiry, use counter and revocation survive every reload.
func (e *Enroller) loadInfra() error {
	now := e.now()
	var expires int64
	switch life := e.cfg.InfraExpiry; {
	case life == 0:
		expires = store.UnixMS(now.Add(DefaultInfraKeyLifetime))
	case life > 0:
		expires = store.UnixMS(now.Add(life))
	}
	row, err := e.cfg.Store.EnsureInfraKey(context.Background(), e.cfg.Infra.hash[:], expires, store.UnixMS(now))
	if err != nil {
		return fmt.Errorf("workers: record the infrastructure invitation key: %w", err)
	}
	switch {
	case row.Revoked:
		e.log.Warn("the infrastructure invitation key file holds a revoked key; it enrolls nothing until the file holds a new key",
			"invitation_id", row.ID)
	case row.ExpiresAt == 0:
		e.log.Warn("the infrastructure invitation key has no expiry: anyone who obtains the file can enroll Workers for as long as it is loaded; "+
			"set infra_invite_key_days AND rotate the key file secret (or revoke this key): "+
			"changing the setting alone does not change an existing key", "invitation_id", row.ID)
	case row.ExpiresAt <= store.UnixMS(now):
		e.log.Warn("the infrastructure invitation key has expired; it enrolls nothing until the file holds a new key",
			"invitation_id", row.ID)
	}
	return nil
}

// Handle runs one enrollment on an authenticated enroll_invite connection:
//
//  1. the enroll rate limit applies by client IP and Hub fingerprint;
//  2. the Worker's invite.secret frame is read from inside the channel;
//  3. the key is looked up by the SHA-256 of its secret and the enrollment
//     transaction runs (see [store.Store.EnrollWithInvitation]);
//  4. the Worker gets welcome, or an error frame: invite_invalid (unknown or
//     revoked), invite_expired, invite_exhausted.
//
// A Worker key that was removed is answered with the signed revoke of AD-9,
// consuming nothing.
func (e *Enroller) Handle(ctx context.Context, c *Conn) error {
	if e.cfg.Limiter != nil {
		if ok, _ := e.cfg.Limiter.Allow(ratelimit.PolicyEnroll, c.ClientIP(), string(e.cfg.FingerprintFunc())); !ok {
			e.refuse(ctx, c, "", proto.NewError(proto.CodeRateLimited, "too many enrollment attempts; retry later"))
			return nil
		}
	}
	body, err := c.ReadFrame()
	if err != nil {
		return fmt.Errorf("read invite.secret: %w", err)
	}
	hash, perr := e.secretHash(body)
	if perr != nil {
		e.refuse(ctx, c, "", perr)
		return nil
	}

	name := cleanWorkerName(c.Worker().Name)
	now := e.now()
	var res store.EnrollResult
	params := store.EnrollParams{
		SecretHash: hash[:], Pub: []byte(c.WorkerPub()), Name: name, WorkerID: store.NewID(), Now: store.UnixMS(now),
		Audit: e.auditRow(c, now), Addr: c.ClientIP().String(),
	}
	if e.cfg.Infra != nil && e.cfg.Infra.matches(hash) {
		params.Infra = true
		params.OwnerID, err = e.cfg.Store.InfraOwnerID(ctx, e.cfg.InfraOwner)
		if err != nil {
			err = fmt.Errorf("resolve the infrastructure owner (infra_default_owner): %w", err)
		}
	}
	if err == nil {
		res, err = e.cfg.Store.EnrollWithInvitation(ctx, params)
	}
	switch {
	case err == nil:
	case errors.Is(err, store.ErrWorkerTombstoned):
		return c.revokeKey(ctx)
	default:
		pe := mapEnrollError(err)
		e.refuse(ctx, c, res.InvitationID, pe)
		if pe.Code == proto.CodeInternal {
			return fmt.Errorf("enroll: %w", err)
		}
		return nil
	}
	if res.Existing {
		e.log.Info("worker enroll: known key enrolled again", "worker_id", res.Worker.ID, "client_ip", c.ClientIP().String())
	} else {
		e.log.Info("worker enrolled; it is pending until its owner or an admin approves it", "worker_id", res.Worker.ID, "name", res.Worker.Name, "kind", res.Worker.Kind,
			"owner", res.Worker.OwnerID, "client_ip", c.ClientIP().String())
	}
	return c.Welcome(ctx, res.Worker)
}

func (e *Enroller) auditRow(c *Conn, now time.Time) *store.AuditRow {
	return &store.AuditRow{
		ID: store.NewID(), TS: store.UnixMS(now), Actor: "worker:" + c.fingerprint(),
		Action: AuditWorkerEnroll, Outcome: auditPending,
	}
}

// secretHash decodes the invite.secret frame and returns SHA-256 of the
// secret. The errors it returns are proto errors safe to send to the Worker.
func (e *Enroller) secretHash(body json.RawMessage) ([sha256.Size]byte, *proto.Error) {
	var f InviteSecretFrame
	if err := json.Unmarshal(body, &f); err != nil || f.Type != proto.TypeInviteSecret {
		return [sha256.Size]byte{}, proto.NewError(proto.CodeInvalidArgument, "expected an invite.secret frame")
	}
	k, err := parseSecret(e.cfg.FingerprintFunc(), f.Secret)
	if err != nil {
		return [sha256.Size]byte{}, proto.NewError(proto.CodeInviteInvalid, "the invitation key is not valid")
	}
	return k.SecretHash(), nil
}

// parseSecret accepts the bare secret or the whole key of this Hub.
func parseSecret(hub identity.Fingerprint, s string) (identity.InviteKey, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "mnk_") {
		k, err := identity.ParseInviteKey(s)
		if err != nil {
			return identity.InviteKey{}, err
		}
		if k.Fingerprint != hub {
			return identity.InviteKey{}, errors.New("the invitation key names another Hub")
		}
		return k, nil
	}
	return identity.ParseInviteKey("mnk_" + string(hub) + "." + s)
}

// mapEnrollError maps a store error to the code the Worker sees.
func mapEnrollError(err error) *proto.Error {
	switch {
	case errors.Is(err, store.ErrInvitationNotFound), errors.Is(err, store.ErrInvitationRevoked):
		return proto.NewError(proto.CodeInviteInvalid, "the invitation key is not valid")
	case errors.Is(err, store.ErrInvitationExpired):
		return proto.NewError(proto.CodeInviteExpired, "the invitation key has expired")
	case errors.Is(err, store.ErrInvitationExhausted):
		return proto.NewError(proto.CodeInviteExhausted, "the invitation key has no uses left")
	}
	return proto.NewError(proto.CodeInternal, "")
}

// refuse answers the Worker with an error frame and audits the refusal. The
// audit row names the invitation id when it is known, never the secret.
func (e *Enroller) refuse(ctx context.Context, c *Conn, invitationID string, pe *proto.Error) {
	// The audit row is written before the answer, so that a Worker that sees
	// the refusal can rely on the row being there.
	if e.cfg.Auditor != nil {
		ev := audit.Event{Actor: "worker:" + c.fingerprint(), Action: AuditWorkerEnroll, Target: invitationID, Outcome: string(pe.Code)}
		if err := e.cfg.Auditor.Record(context.WithoutCancel(ctx), ev); err != nil {
			e.log.Warn("worker enroll: cannot audit the refusal", "error", err.Error())
		}
	}
	if err := c.Reject(pe); err != nil {
		e.log.Info("worker enroll: cannot send the refusal", "error", err.Error())
	}
	e.log.Info("worker enroll: refused", "code", string(pe.Code), "client_ip", c.ClientIP().String())
}

// revokeKey answers a removed Worker key with the signed revoke (AD-9).
func (c *Conn) revokeKey(ctx context.Context) error {
	t, err := c.h.cfg.Repository.TombstoneByPub(ctx, []byte(c.res.WorkerPub))
	if err != nil {
		_ = c.Reject(proto.NewError(proto.CodeInternal, ""))
		return fmt.Errorf("read tombstone: %w", err)
	}
	return c.revoke(ctx, t)
}

// cleanWorkerName turns the name of a hello into a Worker name: control
// characters, white space and ":" (the separator of "worker:session")
// become "-", the result is trimmed and cut at maxEnrollNameLen bytes on a
// character boundary, and an empty result becomes "worker".
func cleanWorkerName(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "-")
	}
	s = strings.Map(func(r rune) rune {
		if r == ':' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return '-'
		}
		return r
	}, s)
	s = strings.Trim(s, "-.")
	if len(s) > maxEnrollNameLen {
		cut := maxEnrollNameLen
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimRight(s[:cut], "-.")
	}
	if s == "" {
		return defaultWorkerName
	}
	return s
}

// InfraKey is the Hub-wide infrastructure invitation key read from
// MINION_INFRA_INVITE_KEY_FILE. It enrols infrastructure Workers owned by
// infra_default_owner. Its secret hash is also a row of the invitation table
// (see [Enroller] and [store.Store.EnsureInfraKey]) with an expiry, a use
// counter and a revocation that survive reloads; removing the file setting
// and restarting the Hub withdraws it. Only the hash of its secret is kept in
// memory.
type InfraKey struct {
	hash [sha256.Size]byte
}

// NewInfraKey parses the text of an infrastructure key: either a complete
// mnk_<fingerprint>.<secret> whose fingerprint must be the Hub's, or just the
// 52-character base32 secret, which saves the operator from knowing the
// fingerprint before the Hub has started. The errors never echo the text.
func NewInfraKey(text string, hub identity.Fingerprint) (*InfraKey, error) {
	fpr, err := identity.ParseFingerprint(string(hub))
	if err != nil {
		return nil, errors.New("workers: the Hub fingerprint is invalid")
	}
	k, err := parseSecret(fpr, text)
	if err != nil {
		return nil, errors.New("workers: the infrastructure invitation key is malformed or names another Hub: " +
			"expected mnk_<this Hub's fingerprint>.<secret>, or only the 52-character secret")
	}
	return &InfraKey{hash: k.SecretHash()}, nil
}

// LoadInfraKey reads the key from the file at path (one line, trailing newline
// removed) and parses it with [NewInfraKey].
func LoadInfraKey(path string, hub identity.Fingerprint) (*InfraKey, error) {
	text, err := config.ReadSecretFile(path)
	if err != nil {
		return nil, fmt.Errorf("workers: infrastructure invitation key file: %w", err)
	}
	k, err := NewInfraKey(text, hub)
	if err != nil {
		return nil, fmt.Errorf("%w (file %s)", err, path)
	}
	return k, nil
}

// matches compares secret hashes in constant time.
func (k *InfraKey) matches(hash [sha256.Size]byte) bool {
	return subtle.ConstantTimeCompare(k.hash[:], hash[:]) == 1
}

// RegisterEnroller registers e as the handler of the enroll_invite mode on h.
func RegisterEnroller(h *Handler, e *Enroller) error {
	return h.Handle(transport.ModeEnrollInvite, e)
}
