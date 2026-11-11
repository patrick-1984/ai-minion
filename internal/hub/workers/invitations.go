package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Audit vocabulary of invitation keys and enrollment (AD-20). No event holds a
// key, a secret or any part of one; the target is an invitation or Worker id.
const (
	AuditInvitationCreate = "invitation.create"
	AuditInvitationRevoke = "invitation.revoke"
	AuditWorkerEnroll     = "worker.enroll"

	auditSuccess = "success"
	// auditPending is the outcome of an enrollment: the Worker exists but
	// waits for approval.
	auditPending = "pending"
)

// InvitationCaller is who creates, lists or revokes invitation keys. The
// handler fills it from the authenticated request.
type InvitationCaller struct {
	// UserID is the user whose space the call is in: the session user, the
	// key owner or, for act-as, the target user. It becomes the creator and
	// so the owner of the Workers the key enrolls.
	UserID string
	// Actor and ActingAs are the audit identities (the admin who acts as
	// UserID, when ActingAs is set).
	Actor    string
	ActingAs string
	// Admin tells whether the caller holds the admin permission over every
	// user. The handler decides it with the authorizer; the service never
	// reads roles itself.
	Admin bool
	// Manages, when set and Admin is false, tells whether the caller may
	// manage the keys of the given creator. It serves callers whose reach is
	// limited (an admin's API key scoped to some users): they see and revoke
	// only the keys of creators they can manage. Nil means no reach beyond
	// the caller's own keys.
	Manages func(ctx context.Context, creatorID string) (bool, error)
}

// may reports whether the caller may act on a key made by creator: their own,
// or one of a creator they manage.
func (c InvitationCaller) may(ctx context.Context, creator string) (bool, error) {
	switch {
	case creator == c.UserID || c.Admin:
		return true, nil
	case c.Manages != nil:
		return c.Manages(ctx, creator)
	}
	return false, nil
}

func (c InvitationCaller) validate() error {
	if c.UserID == "" || c.Actor == "" {
		return proto.NewError(proto.CodeForbidden, "not allowed")
	}
	return nil
}

// InvitationStore is the part of the invitation tables the service needs;
// *store.Store implements it.
type InvitationStore interface {
	InsertInvitation(ctx context.Context, r store.InvitationRow) error
	InvitationByID(ctx context.Context, id string) (store.InvitationRow, error)
	ListInvitationsWithInfra(ctx context.Context, creatorID string, withInfra bool) ([]store.InvitationRow, error)
	RevokeInvitation(ctx context.Context, id string, nowMS int64) error
}

var _ InvitationStore = (*store.Store)(nil)

// InvitationsConfig configures [NewInvitations]. Store and HubFingerprint are
// required.
type InvitationsConfig struct {
	Store InvitationStore
	// HubFingerprint is the Hub identity fingerprint that every key carries
	// (AD-5, AD-7), so that a Worker verifies the Hub before sending the
	// secret.
	HubFingerprint identity.Fingerprint
	// Auditor records creation and revocation; nil disables it.
	Auditor Auditor
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Invitations creates, lists and revokes invitation keys (AD-7, R91). It
// holds no secret: a key leaves [Invitations.Create] once and only its
// SHA-256 is kept.
type Invitations struct {
	st    InvitationStore
	fpr   identity.Fingerprint
	audit Auditor
	now   func() time.Time
	log   *slog.Logger
}

// NewInvitations validates cfg and returns the service.
func NewInvitations(cfg InvitationsConfig) (*Invitations, error) {
	if cfg.Store == nil {
		return nil, errors.New("workers: the invitation service needs a store")
	}
	fpr, err := identity.ParseFingerprint(string(cfg.HubFingerprint))
	if err != nil {
		return nil, errors.New("workers: the invitation service needs the Hub fingerprint")
	}
	s := &Invitations{st: cfg.Store, fpr: fpr, audit: cfg.Auditor, now: cfg.Now, log: cfg.Logger}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

// Create makes a new invitation key owned by the caller. kind=infrastructure
// needs an admin (forbidden otherwise). The returned Key is the only time the
// secret is available.
func (s *Invitations) Create(ctx context.Context, caller InvitationCaller, a ops.InvitationCreateArgs) (ops.InvitationCreateResult, error) {
	if err := caller.validate(); err != nil {
		return ops.InvitationCreateResult{}, err
	}
	if err := a.Validate(); err != nil {
		return ops.InvitationCreateResult{}, proto.NewError(proto.CodeInvalidArgument, err.Error())
	}
	kind := a.EffectiveKind()
	if kind == ops.KindInfrastructure && !caller.Admin {
		return ops.InvitationCreateResult{}, proto.NewError(proto.CodeForbidden, "only an admin can create an infrastructure invitation key")
	}
	maxUses, err := invitationCaps(caller, a)
	if err != nil {
		return ops.InvitationCreateResult{}, err
	}
	key, err := identity.NewInviteKey(s.fpr)
	if err != nil {
		return ops.InvitationCreateResult{}, fmt.Errorf("workers: create invitation key: %w", err)
	}
	hash := key.SecretHash()
	now := s.now()
	row := store.InvitationRow{
		ID: store.NewID(), SecretHash: hash[:], Name: a.Name, Mode: a.Mode, Kind: kind, TTL: a.TTL,
		CreatorID: caller.UserID, CreatedAt: store.UnixMS(now),
	}
	if d, _ := ops.InvitationTTLDuration(a.TTL); d > 0 {
		row.ExpiresAt = store.UnixMS(now.Add(d))
	}
	row.MaxUses = maxUses
	if err := s.st.InsertInvitation(ctx, row); err != nil {
		return ops.InvitationCreateResult{}, err
	}
	s.record(ctx, caller, AuditInvitationCreate, row.ID, auditSuccess)
	res := ops.InvitationCreateResult{
		ID: row.ID, Key: key.String(), Name: row.Name, Mode: row.Mode, Kind: row.Kind, TTL: row.TTL, MaxUses: maxUsesPtr(row.MaxUses),
		CreatedAt: ops.Timestamp(store.FromUnixMS(row.CreatedAt)),
	}
	res.ExpiresAt = optTimestamp(row.ExpiresAt)
	return res, nil
}

// List returns the caller's invitation keys, newest first. all=true lists the
// keys of every user and needs an admin (forbidden otherwise).
func (s *Invitations) List(ctx context.Context, caller InvitationCaller, all bool) (ops.InvitationListResult, error) {
	if err := caller.validate(); err != nil {
		return ops.InvitationListResult{}, err
	}
	creator := caller.UserID
	if all {
		if !caller.Admin && caller.Manages == nil {
			return ops.InvitationListResult{}, proto.NewError(proto.CodeForbidden, "only an admin can list every user's invitation keys")
		}
		creator = ""
	}
	// The row of the Hub-wide key file belongs to no user; an admin sees it in
	// both views, so that it can be checked and revoked like any other key.
	rows, err := s.st.ListInvitationsWithInfra(ctx, creator, caller.Admin)
	if err != nil {
		return ops.InvitationListResult{}, err
	}
	nowMS := store.UnixMS(s.now())
	out := ops.InvitationListResult{Invitations: make([]ops.InvitationInfo, 0, len(rows))}
	reach := map[string]bool{}
	for _, r := range rows {
		if all && !caller.Admin {
			ok, seen := reach[r.CreatorID]
			if !seen {
				if ok, err = caller.may(ctx, r.CreatorID); err != nil {
					return ops.InvitationListResult{}, err
				}
				reach[r.CreatorID] = ok
			}
			if !ok {
				continue
			}
		}
		out.Invitations = append(out.Invitations, invitationInfo(r, nowMS))
	}
	return out, nil
}

// Revoke revokes an invitation key of the caller (or any key, for an admin).
// A key that does not exist or belongs to someone else is not_found, so that
// ids leak nothing. Revoking twice succeeds. Workers that enrolled with the
// key are unaffected.
func (s *Invitations) Revoke(ctx context.Context, caller InvitationCaller, id string) error {
	if err := caller.validate(); err != nil {
		return err
	}
	notFound := proto.NewError(proto.CodeNotFound, "invitation key not found")
	row, err := s.st.InvitationByID(ctx, id)
	switch {
	case errors.Is(err, store.ErrInvitationNotFound):
		return notFound
	case err != nil:
		return err
	}
	// Authorise against the creator of the key, not the caller's own space:
	// a scoped admin key must not reach keys of users outside its scope.
	if ok, err := caller.may(ctx, row.CreatorID); err != nil {
		return err
	} else if !ok {
		return notFound
	}
	if err := s.st.RevokeInvitation(ctx, id, store.UnixMS(s.now())); err != nil {
		if errors.Is(err, store.ErrInvitationNotFound) {
			return notFound
		}
		return err
	}
	s.record(ctx, caller, AuditInvitationRevoke, id, auditSuccess)
	return nil
}

func (s *Invitations) record(ctx context.Context, c InvitationCaller, action, target, outcome string) {
	if s.audit == nil {
		return
	}
	ev := audit.Event{Actor: c.Actor, ActingAs: c.ActingAs, Action: action, Target: target, Outcome: outcome}
	if err := s.audit.Record(context.WithoutCancel(ctx), ev); err != nil {
		s.log.Error("invitation audit write failed", "action", action, "error", err.Error())
	}
}

func invitationInfo(r store.InvitationRow, nowMS int64) ops.InvitationInfo {
	info := ops.InvitationInfo{
		ID: r.ID, Name: r.Name, Mode: r.Mode, Kind: r.Kind, TTL: r.TTL, CreatorID: r.CreatorID,
		CreatedAt: ops.Timestamp(store.FromUnixMS(r.CreatedAt)), ExpiresAt: optTimestamp(r.ExpiresAt),
		Uses: r.Uses, Revoked: r.Revoked, RevokedAt: optTimestamp(r.RevokedAt), Notice: r.Notice,
	}
	if r.Source == store.InvitationSourceInfraFile {
		info.Source = r.Source
	}
	if r.MaxUses != 0 {
		m := r.MaxUses
		info.MaxUses = &m
	}
	switch {
	case r.Revoked:
		info.Status = ops.InvitationRevoked
	case r.ExpiresAt != 0 && r.ExpiresAt <= nowMS:
		info.Status = ops.InvitationExpired
	case r.MaxUses != 0 && r.Uses >= r.MaxUses:
		info.Status = ops.InvitationExhausted
	default:
		info.Status = ops.InvitationActive
	}
	return info
}

// optTimestamp converts unix milliseconds to a timestamp, nil for 0.
func optTimestamp(ms int64) *ops.Timestamp {
	if ms == 0 {
		return nil
	}
	t := ops.Timestamp(store.FromUnixMS(ms))
	return &t
}

func maxUsesPtr(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// invitationCaps applies the creation caps of F-02 and returns the max_uses of
// the new key (0 for none). An admin is not capped. A non-admin cannot create a
// key that is valid longer than a week or a reusable key without a use limit
// (the default is ten uses, the most is one hundred). The messages say what is
// allowed, so the web page, the API and the CLI show the same text.
func invitationCaps(caller InvitationCaller, a ops.InvitationCreateArgs) (int, error) {
	bad := func(msg string) error { return proto.NewError(proto.CodeInvalidArgument, msg) }
	if !caller.Admin {
		if d, _ := ops.InvitationTTLDuration(a.TTL); d == 0 || d > mustTTL(ops.NonAdminMaxInvitationTTL) {
			return 0, bad("ttl: only an admin can create a key that is valid longer than 1 week; choose 1h, 4h, 8h, 24h or 1w")
		}
	}
	switch {
	case a.Mode == ops.InvitationOneTime:
		return 1, nil
	case caller.Admin:
		return a.MaxUses, nil
	case a.MaxUses == 0:
		return ops.DefaultReusableMaxUses, nil
	case a.MaxUses > ops.MaxReusableUsesNonAdmin:
		return 0, bad(fmt.Sprintf("max_uses: a reusable key allows at most %d enrollments unless you are an admin", ops.MaxReusableUsesNonAdmin))
	}
	return a.MaxUses, nil
}

func mustTTL(label string) time.Duration {
	d, _ := ops.InvitationTTLDuration(label)
	return d
}
