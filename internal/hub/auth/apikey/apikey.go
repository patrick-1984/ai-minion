package apikey

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Errors of the authentication path. Callers match them with errors.Is.
var (
	// ErrInvalidKey is returned for a token that is malformed, unknown,
	// expired or revoked, or whose owner is unknown or deactivated. It is the
	// only error that counts as a failed attempt (apikey_fail, AD-19);
	// callers answer it as unauthenticated.
	ErrInvalidKey = errors.New("apikey: invalid API key")
	// ErrWrongWorker is returned by [CheckCallBinding] for a key that is not
	// bound to the Worker whose call stream it arrived on.
	ErrWrongWorker = errors.New("apikey: the API key is not bound to this Worker")
)

// Audit vocabulary of the key lifecycle (AD-20). An event names the key by its
// id and holds neither the key nor any part of it.
const (
	AuditActionCreate = "apikey.create"
	AuditActionRevoke = "apikey.revoke"
	auditSuccess      = "success"
	auditDenied       = "denied"
	auditFailed       = "failed"
)

// Identity is the result of a successful authentication: the stored id of the
// key (the audit actor, never the token or its hash) and the principal to hand
// to authz.
type Identity struct {
	KeyID     string
	Principal authz.Principal
}

// Repository is the persistence port of the [Service]; *store.Store implements
// it. All SQL lives in the store package (AD-16).
type Repository interface {
	CreateAPIKey(ctx context.Context, k store.APIKeyRow, audit *store.AuditRow) error
	APIKeyByHash(ctx context.Context, hash []byte) (store.APIKeyRow, error)
	APIKeyByID(ctx context.Context, id string) (store.APIKeyRow, error)
	ListAPIKeys(ctx context.Context, ownerID string) ([]store.APIKeyRow, error)
	RevokeAPIKey(ctx context.Context, id string, atMS int64, audit *store.AuditRow) (bool, error)
	// TouchAPIKey records the last use of a key (throttled by the store).
	TouchAPIKey(ctx context.Context, id string, atMS int64) (bool, error)
	// RecordAudit writes one audit row on its own, for a refused or failed
	// attempt that has no transaction to join.
	RecordAudit(ctx context.Context, row store.AuditRow) error
}

var _ Repository = (*store.Store)(nil)

// Authorizer is the part of *authz.Authorizer the service uses. Every decision
// about who may grant or manage what goes through it (AD-10: one path).
type Authorizer interface {
	Decide(ctx context.Context, p authz.Principal, accepted ops.ActionSet, res authz.Resource) error
	EffectiveActions(ctx context.Context, p authz.Principal, res authz.Resource) (ops.ActionSet, error)
}

var _ Authorizer = (*authz.Authorizer)(nil)

// Config builds a [Service].
type Config struct {
	// Repo stores the keys. Required.
	Repo Repository
	// Authz decides every grant. Required.
	Authz Authorizer
	// Users reads the current state of a key's owner on every
	// authentication; it must not cache (see authz.UserLookup). Required.
	Users authz.UserLookup
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Log receives the failures of recording a key's last use; nil discards
	// them. Such a failure never fails an authentication.
	Log *slog.Logger
}

// Service creates, lists, revokes and authenticates API keys (AD-11). It holds
// no state besides its ports, so it is safe for concurrent use when they are.
type Service struct {
	repo  Repository
	authz Authorizer
	users authz.UserLookup
	now   func() time.Time
	log   *slog.Logger
}

// New returns a Service. It fails when a required port is missing.
func New(cfg Config) (*Service, error) {
	if cfg.Repo == nil || cfg.Authz == nil || cfg.Users == nil {
		return nil, errors.New("apikey: New needs a Repo, an Authz and a Users port")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{repo: cfg.Repo, authz: cfg.Authz, users: cfg.Users, now: now, log: log}, nil
}

func forbidden(msg string) error { return proto.NewError(proto.CodeForbidden, msg) }

// requireHuman refuses principals that must not manage keys: an API key (a
// leaked key must not mint stronger ones) and a Worker identity. Act-as is
// allowed where the caller says so.
func requireHuman(p authz.Principal, allowActAs bool) error {
	switch p.Kind {
	case authz.KindBrowserSession:
		return nil
	case authz.KindActAs:
		if allowActAs {
			return nil
		}
		return forbidden("an admin acting as a user cannot create API keys")
	default:
		return forbidden("API keys are managed by signing in as the user, not with an API key")
	}
}

// audit builds the audit row of an action of p on the key id. It commits with
// the change it describes (AD-16).
func (s *Service) audit(p authz.Principal, action, keyID string) *store.AuditRow {
	row := &store.AuditRow{
		ID: store.NewID(), TS: store.UnixMS(s.now()),
		Actor: p.UserID, Action: action, Target: keyID, Outcome: auditSuccess,
	}
	if p.Kind == authz.KindActAs {
		row.Actor, row.ActingAs = p.ActorID, p.UserID
	}
	return row
}

// auditAttempt records a creation attempt that did not produce a key (AD-20:
// key actions are audited with their outcome). apikey_create is not a write
// operation of the dispatcher, so the service does it. The row names the
// caller as target because no key id exists, and holds no key text. The row
// is best effort: a failing audit store must not change the answer to the
// caller. A principal without any identity has no actor to record.
func (s *Service) auditAttempt(ctx context.Context, p authz.Principal, outcome string) {
	row := s.audit(p, AuditActionCreate, p.UserID)
	row.Outcome = outcome
	if row.Actor == "" {
		row.Actor = p.WorkerID // a Worker identity has no user
	}
	if row.Actor == "" {
		return
	}
	_ = s.repo.RecordAudit(ctx, *row)
}

// refuse audits a failed creation attempt and returns err: "denied" for a
// forbidden refusal, "failed" for anything else. Argument errors are the
// caller's typing mistakes and are not audited.
func (s *Service) refuse(ctx context.Context, p authz.Principal, err error) error {
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code == proto.CodeForbidden {
		s.auditAttempt(ctx, p, auditDenied)
	} else {
		s.auditAttempt(ctx, p, auditFailed)
	}
	return err
}

// Create makes a key owned by p and returns the key text, which is shown
// once, with the stored description. The key and its grants, and the audit row,
// are written in one transaction.
//
// A user can grant only what they hold: for every resource and every action
// the caller's current effective actions on that resource must include the
// action. That makes "everything" and "manage" admin only, and keeps a user
// within their own Workers and sessions, without a second rule table.
func (s *Service) Create(ctx context.Context, p authz.Principal, args ops.APIKeyCreateArgs) (ops.APIKeyCreateResult, error) {
	if err := requireHuman(p, false); err != nil {
		return ops.APIKeyCreateResult{}, s.refuse(ctx, p, err)
	}
	if err := args.Validate(); err != nil {
		return ops.APIKeyCreateResult{}, err
	}
	now := s.now()
	if args.ExpiresAt != nil && !args.ExpiresAt.Time().After(now) {
		return ops.APIKeyCreateResult{}, &ops.ArgumentError{Field: "expires_at", Reason: "must lie in the future"}
	}
	grants, actions := canonicalGrants(args.Resources), canonicalActions(args.Actions)
	if err := s.authorizeGrants(ctx, p, grants, actions, args.BoundWorkerID); err != nil {
		return ops.APIKeyCreateResult{}, s.refuse(ctx, p, err)
	}

	key := identity.NewAPIKey()
	hash := key.Hash()
	row := store.APIKeyRow{
		ID: store.NewID(), OwnerID: p.UserID, Name: args.Name,
		Hash: hash[:], Hint: key.Hint(),
		Actions:       actionStrings(actions),
		BoundWorkerID: args.BoundWorkerID,
		CreatedAt:     store.UnixMS(now),
		Scopes:        scopeRows(grants),
	}
	if args.ExpiresAt != nil {
		row.ExpiresAt = store.UnixMS(args.ExpiresAt.Time())
	}
	if err := s.repo.CreateAPIKey(ctx, row, s.audit(p, AuditActionCreate, row.ID)); err != nil {
		s.auditAttempt(ctx, p, auditFailed)
		return ops.APIKeyCreateResult{}, fmt.Errorf("apikey: create: %w", err)
	}
	return ops.APIKeyCreateResult{Key: key.String(), Info: info(row)}, nil
}

// authorizeGrants checks that p may hand out every action on every grant and
// may bind a key to the Worker.
func (s *Service) authorizeGrants(ctx context.Context, p authz.Principal, grants []Grant, actions []ops.Action, boundWorker string) error {
	deny := forbidden("you can grant only resources and actions you hold yourself")
	for _, g := range grants {
		if g.Kind == ops.KeyResourceEverything {
			// Only an admin holds manage on a user resource; the decision
			// stays in authz (AD-11).
			err := s.authz.Decide(ctx, p, ops.MustActionSet(ops.ActionManage), authz.UserResource{ID: p.UserID})
			var pe *proto.Error
			switch {
			case err == nil:
			case errors.As(err, &pe) && (pe.Code == proto.CodeForbidden || pe.Code == proto.CodeNotFound):
				return forbidden("only an admin can create a key for everything")
			default:
				return fmt.Errorf("apikey: check the grant for everything: %w", err)
			}
			continue
		}
		held, err := s.authz.EffectiveActions(ctx, p, resourceOf(g))
		if err != nil {
			return fmt.Errorf("apikey: check the grant: %w", err)
		}
		for _, a := range actions {
			if !slices.Contains(held, a) {
				return deny
			}
		}
	}
	if boundWorker != "" {
		held, err := s.authz.EffectiveActions(ctx, p, authz.WorkerResource{ID: boundWorker})
		if err != nil {
			return fmt.Errorf("apikey: check the bound Worker: %w", err)
		}
		if len(held) == 0 {
			return deny
		}
	}
	return nil
}

func resourceOf(g Grant) authz.Resource {
	switch g.Kind {
	case ops.KeyResourceUser:
		return authz.UserResource{ID: g.UserID}
	case ops.KeyResourceWorker:
		return authz.WorkerResource{ID: g.WorkerID}
	default:
		return authz.SessionResource{WorkerID: g.WorkerID, Name: g.SessionName}
	}
}

// List returns keys without their secret. By default these are p's own keys;
// owner_id and all are for admins, decided through authz like every other
// access to another user's data.
func (s *Service) List(ctx context.Context, p authz.Principal, args ops.APIKeyListArgs) (ops.APIKeyListResult, error) {
	if err := requireHuman(p, true); err != nil {
		return ops.APIKeyListResult{}, err
	}
	if err := args.Validate(); err != nil {
		return ops.APIKeyListResult{}, err
	}
	owner := p.UserID
	switch {
	case args.All:
		if err := s.authz.Decide(ctx, p, ops.MustActionSet(ops.ActionManage), authz.UserResource{ID: p.UserID}); err != nil {
			return ops.APIKeyListResult{}, err
		}
		owner = ""
	case args.OwnerID != "" && args.OwnerID != p.UserID:
		if err := s.authz.Decide(ctx, p, ops.MustActionSet(ops.ActionRead), authz.UserResource{ID: args.OwnerID}); err != nil {
			return ops.APIKeyListResult{}, err
		}
		owner = args.OwnerID
	}
	rows, err := s.repo.ListAPIKeys(ctx, owner)
	if err != nil {
		return ops.APIKeyListResult{}, fmt.Errorf("apikey: list: %w", err)
	}
	out := ops.APIKeyListResult{Keys: make([]ops.APIKeyInfo, 0, len(rows))}
	for _, r := range rows {
		out.Keys = append(out.Keys, info(r))
	}
	return out, nil
}

// Revoke revokes the key at once. The owner and admins may revoke it; for
// anyone else the key does not exist (not_found). Revoking a revoked key
// succeeds and keeps the first revocation time.
func (s *Service) Revoke(ctx context.Context, p authz.Principal, id string) error {
	if err := requireHuman(p, true); err != nil {
		return err
	}
	row, err := s.repo.APIKeyByID(ctx, id)
	if errors.Is(err, store.ErrAPIKeyNotFound) {
		return proto.NewError(proto.CodeNotFound, "not found")
	}
	if err != nil {
		return fmt.Errorf("apikey: revoke: %w", err)
	}
	if err := s.authz.Decide(ctx, p, ops.MustActionSet(ops.ActionOperate), authz.UserResource{ID: row.OwnerID}); err != nil {
		return err
	}
	if _, err := s.repo.RevokeAPIKey(ctx, id, store.UnixMS(s.now()), s.audit(p, AuditActionRevoke, id)); err != nil {
		if errors.Is(err, store.ErrAPIKeyNotFound) {
			return proto.NewError(proto.CodeNotFound, "not found")
		}
		return fmt.Errorf("apikey: revoke: %w", err)
	}
	return nil
}

// AuthenticateKey turns a bearer token into the principal of its key. The
// store is read on every call, nothing is cached: a revocation, an expiry and
// the deactivation of the owner apply to the next request. The principal
// carries the key's scope; authz then intersects it with the owner's current
// permissions on every decision, so a demoted owner shrinks the key at once.
//
// Every failure of the key itself is [ErrInvalidKey]; a failing store is a
// different error and does not count as a failed attempt. The token is never
// logged or included in an error. Expiry applies to keys only and never to
// enrolled Workers.
func (s *Service) AuthenticateKey(ctx context.Context, token string) (Identity, error) {
	key, err := identity.ParseAPIKey(token)
	if err != nil {
		return Identity{}, ErrInvalidKey
	}
	hash := key.Hash()
	row, err := s.repo.APIKeyByHash(ctx, hash[:])
	if errors.Is(err, store.ErrAPIKeyNotFound) {
		return Identity{}, ErrInvalidKey
	}
	if err != nil {
		return Identity{}, fmt.Errorf("apikey: look up the key: %w", err)
	}
	if subtle.ConstantTimeCompare(row.Hash, hash[:]) != 1 {
		return Identity{}, ErrInvalidKey
	}
	now := s.now()
	if row.RevokedAt != 0 || (row.ExpiresAt != 0 && !now.Before(store.FromUnixMS(row.ExpiresAt))) {
		return Identity{}, ErrInvalidKey
	}
	owner, err := s.users.User(ctx, row.OwnerID)
	if errors.Is(err, authz.ErrNotFound) {
		return Identity{}, ErrInvalidKey
	}
	if err != nil {
		return Identity{}, fmt.Errorf("apikey: look up the owner: %w", err)
	}
	if !owner.Active {
		return Identity{}, ErrInvalidKey
	}
	// The key is valid: remember the use. It is bookkeeping for the key list, so
	// a failing write is logged and never refuses a good key.
	if _, err := s.repo.TouchAPIKey(ctx, row.ID, now.UnixMilli()); err != nil {
		s.log.Warn("could not record the last use of an API key", "key_id", row.ID, "err", err)
	}
	scope := NewScope(grantsOf(row.Scopes), actionsOf(row.Actions))
	return Identity{
		KeyID:     row.ID,
		Principal: authz.NewAPIKeyPrincipal(row.OwnerID, scope, row.BoundWorkerID),
	}, nil
}

// AuthenticateCall is [Service.AuthenticateKey] for a Worker call stream: the
// key must also be bound to workerID, the identity of the Worker that opened
// the stream. Story 7.4 uses it. A key bound to another Worker, and a key that
// is not bound at all, is refused with [ErrWrongWorker]; a key that does not
// authenticate is [ErrInvalidKey].
func (s *Service) AuthenticateCall(ctx context.Context, token, workerID string) (Identity, error) {
	id, err := s.AuthenticateKey(ctx, token)
	if err != nil {
		return Identity{}, err
	}
	if err := CheckCallBinding(id, workerID); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// CheckCallBinding reports whether an authenticated key may be used on the
// call stream of the Worker workerID: only when it is bound to exactly that
// Worker. A key that is bound elsewhere, or not at all, gets [ErrWrongWorker].
func CheckCallBinding(id Identity, workerID string) error {
	if workerID == "" || id.Principal.WorkerID != workerID {
		return ErrWrongWorker
	}
	return nil
}

// canonicalGrants returns the grants without duplicates, in a stable order.
func canonicalGrants(in []Grant) []Grant {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b Grant) int {
		return strings.Compare(grantSortKey(a), grantSortKey(b))
	})
	return slices.Compact(out)
}

func grantSortKey(g Grant) string {
	return g.Kind + "\x00" + g.UserID + "\x00" + g.WorkerID + "\x00" + g.SessionName
}

// canonicalActions returns the actions sorted without duplicates.
func canonicalActions(in []ops.Action) []ops.Action {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

func actionStrings(in []ops.Action) []string {
	out := make([]string, len(in))
	for i, a := range in {
		out[i] = string(a)
	}
	return out
}

func actionsOf(in []string) []ops.Action {
	out := make([]ops.Action, 0, len(in))
	for _, s := range in {
		out = append(out, ops.Action(s))
	}
	return out
}

func scopeRows(grants []Grant) []store.APIKeyScopeRow {
	out := make([]store.APIKeyScopeRow, len(grants))
	for i, g := range grants {
		out[i] = store.APIKeyScopeRow{Kind: g.Kind, UserID: g.UserID, WorkerID: g.WorkerID, SessionName: g.SessionName}
	}
	return out
}

func grantsOf(rows []store.APIKeyScopeRow) []Grant {
	out := make([]Grant, len(rows))
	for i, r := range rows {
		out[i] = Grant{Kind: r.Kind, UserID: r.UserID, WorkerID: r.WorkerID, SessionName: r.SessionName}
	}
	return out
}

// info converts a stored key to its description. Text read from the database
// is passed through ToValidUTF8, because the wire form must be valid UTF-8.
func info(r store.APIKeyRow) ops.APIKeyInfo {
	out := ops.APIKeyInfo{
		ID: r.ID, Name: strings.ToValidUTF8(r.Name, "�"), Hint: r.Hint, OwnerID: r.OwnerID,
		Resources:     grantsOf(r.Scopes),
		Actions:       actionsOf(r.Actions),
		BoundWorkerID: r.BoundWorkerID,
		CreatedAt:     ops.Timestamp(store.FromUnixMS(r.CreatedAt)),
	}
	if r.ExpiresAt != 0 {
		t := ops.Timestamp(store.FromUnixMS(r.ExpiresAt))
		out.ExpiresAt = &t
	}
	if r.RevokedAt != 0 {
		t := ops.Timestamp(store.FromUnixMS(r.RevokedAt))
		out.RevokedAt = &t
	}
	if r.LastUsedAt != 0 {
		t := ops.Timestamp(store.FromUnixMS(r.LastUsedAt))
		out.LastUsedAt = &t
	}
	return out
}
