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
	EnrollInfrastructure(ctx context.Context, p store.InfraEnrollParams) (store.EnrollResult, error)
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
	fpr, err := identity.ParseFingerprint(string(cfg.HubFingerprint))
	if err != nil {
		return nil, errors.New("workers: the enroller needs the Hub fingerprint")
	}
	cfg.HubFingerprint = fpr
	e := &Enroller{cfg: cfg, now: cfg.Now, log: cfg.Logger}
	if e.now == nil {
		e.now = time.Now
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	return e, nil
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
		if ok, _ := e.cfg.Limiter.Allow(ratelimit.PolicyEnroll, c.ClientIP(), string(e.cfg.HubFingerprint)); !ok {
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
	if e.cfg.Infra != nil && e.cfg.Infra.matches(hash) {
		res, err = e.enrollInfra(ctx, c, name, now)
	} else {
		res, err = e.cfg.Store.EnrollWithInvitation(ctx, store.EnrollParams{
			SecretHash: hash[:], Pub: []byte(c.WorkerPub()), Name: name, WorkerID: store.NewID(), Now: store.UnixMS(now),
			Audit: e.auditRow(c, now),
		})
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
		e.log.Info("worker enrolled", "worker_id", res.Worker.ID, "name", res.Worker.Name, "kind", res.Worker.Kind,
			"owner", res.Worker.OwnerID, "client_ip", c.ClientIP().String())
	}
	return c.Welcome(ctx, res.Worker)
}

// enrollInfra enrolls with the Hub-wide key: kind infrastructure, owner
// infra_default_owner.
func (e *Enroller) enrollInfra(ctx context.Context, c *Conn, name string, now time.Time) (store.EnrollResult, error) {
	owner, err := e.cfg.Store.InfraOwnerID(ctx, e.cfg.InfraOwner)
	if err != nil {
		return store.EnrollResult{}, fmt.Errorf("resolve the infrastructure owner (infra_default_owner): %w", err)
	}
	return e.cfg.Store.EnrollInfrastructure(ctx, store.InfraEnrollParams{
		OwnerID: owner, Pub: []byte(c.WorkerPub()), Name: name, WorkerID: store.NewID(), Now: store.UnixMS(now),
		Audit: e.auditRow(c, now),
	})
}

func (e *Enroller) auditRow(c *Conn, now time.Time) *store.AuditRow {
	return &store.AuditRow{
		ID: store.NewID(), TS: store.UnixMS(now), Actor: "worker:" + c.fingerprint(),
		Action: AuditWorkerEnroll, Outcome: auditSuccess,
	}
}

// secretHash decodes the invite.secret frame and returns SHA-256 of the
// secret. The errors it returns are proto errors safe to send to the Worker.
func (e *Enroller) secretHash(body json.RawMessage) ([sha256.Size]byte, *proto.Error) {
	var f InviteSecretFrame
	if err := json.Unmarshal(body, &f); err != nil || f.Type != proto.TypeInviteSecret {
		return [sha256.Size]byte{}, proto.NewError(proto.CodeInvalidArgument, "expected an invite.secret frame")
	}
	k, err := parseSecret(e.cfg.HubFingerprint, f.Secret)
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
// MINION_INFRA_INVITE_KEY_FILE. It enrolls infrastructure Workers owned by
// infra_default_owner, is reusable, never expires and cannot be revoked in
// the panel: removing the file setting and restarting the Hub withdraws it.
// Only the hash of its secret is kept in memory.
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
