package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Audit vocabulary of account administration (AD-20). Every row names the
// affected account and never holds a password. The row is written in the
// transaction of the change it records, next to the operation row that the
// REST dispatcher writes for every write op.
const (
	AuditActionUserCreate         = "user.create"
	AuditActionUserResetPassword  = "user.reset_password"
	AuditActionUserSetRole        = "user.set_role"
	AuditActionUserDeactivate     = "user.deactivate"
	AuditActionUserPasswordChange = "user.password_change"
)

// Errors of the account administration. Callers match them with errors.Is.
var (
	// ErrUserNotFound: no account has this id.
	ErrUserNotFound = errors.New("local: user not found")
	// ErrUsernameTaken: another account has this user name.
	ErrUsernameTaken = errors.New("local: user name is taken")
	// ErrEmailTaken: another account has this e-mail address.
	ErrEmailTaken = errors.New("local: e-mail address is taken")
	// ErrUserInactive: the account is deactivated.
	ErrUserInactive = errors.New("local: user is deactivated")
	// ErrNoLocalPassword: the account has no local password, so there is no
	// password to reset or change.
	ErrNoLocalPassword = errors.New("local: the account has no local password")
	// ErrLastAdmin: the change would leave the Hub without an active admin.
	ErrLastAdmin = errors.New("local: this is the last active admin")
	// ErrWrongPassword: the old password of a password change is wrong.
	ErrWrongPassword = errors.New("local: the current password is wrong")
	// ErrConcurrentChange: another change of the same credentials won the
	// race; the caller may read the state again and retry.
	ErrConcurrentChange = errors.New("local: the credentials changed in the meantime")
	// ErrActAsPasswordChange: an admin acting as a user cannot change that
	// user's password (AD-10).
	ErrActAsPasswordChange = errors.New("local: an admin acting as a user cannot change the password")
	// ErrInvalidRole: the role is neither "user" nor "admin".
	ErrInvalidRole = errors.New("local: the role must be user or admin")
)

// AdminStore is the persistence port of the [Admin]; *store.Store implements
// it.
type AdminStore interface {
	ListUsers(ctx context.Context) ([]store.UserAdminRow, error)
	UserByID(ctx context.Context, id string) (store.UserRow, error)
	CreateUser(ctx context.Context, p store.CreateUserParams) error
	ResetPassword(ctx context.Context, p store.ResetPasswordParams) error
	ChangePassword(ctx context.Context, p store.ChangePasswordParams) (int64, error)
	SetLocalRole(ctx context.Context, p store.SetLocalRoleParams) (string, error)
	DeactivateUser(ctx context.Context, p store.DeactivateUserParams) (bool, error)
}

var _ AdminStore = (*store.Store)(nil)

// PasswordBackoff is the part of the shared rate limiter that guards the
// old-password check of a password change; *ratelimit.Limiter implements it.
// Without it a stolen session could guess the password at full speed.
type PasswordBackoff interface {
	Backoff(account string) time.Duration
	BackoffFail(account string) time.Duration
	BackoffReset(account string)
}

// Actor says who performs an administrative change, for the audit row.
type Actor struct {
	// ID is the audit actor: a user id, or "key:<id>" for an API key.
	ID string
	// ActingAs is the user an admin acts as (act-as); empty otherwise.
	ActingAs string
}

// Admin implements the account administration operations on top of the
// store: users_list, user_create, user_reset_password, user_set_role,
// user_deactivate and password_change (R37, R39, R57). It holds no state and
// is safe for concurrent use.
type Admin struct {
	Store  AdminStore
	Hasher *Hasher
	// Backoff limits wrong old passwords per account; nil disables it.
	Backoff PasswordBackoff
	// RevokeKeys, when set, revokes the API keys of a deactivated user in the
	// transaction of the deactivation (the API key store, Story 6.1, supplies
	// it). Without it the keys stay stored, but they are already useless: a
	// key is valid only while its owner is active.
	//
	// Wiring: the accounts module (internal/hub/server/mod_accounts.go)
	// provides the *Admin as a kernel service. The API key module, which
	// initialises after it, calls Require[*local.Admin] and sets RevokeKeys
	// during its init, before the Hub serves. The field is not synchronised:
	// it must not be set after startup.
	RevokeKeys store.TxHook
	// Now defaults to time.Now; Random to crypto/rand.
	Now    func() time.Time
	Random io.Reader

	// pwLocks serialises the password changes of one account (see
	// ChangePassword). The zero value is ready.
	pwLocks keyedLocks
}

// keyedLocks is a set of mutexes by key whose Lock honours a context. An
// entry exists only while somebody holds or waits for it.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*keyedLock
}

type keyedLock struct {
	ch   chan struct{} // capacity 1: a token in ch means locked
	refs int
}

// lock waits for the key's lock or for ctx; the returned function releases it.
func (k *keyedLocks) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyedLock{}
	}
	l := k.m[key]
	if l == nil {
		l = &keyedLock{ch: make(chan struct{}, 1)}
		k.m[key] = l
	}
	l.refs++
	k.mu.Unlock()
	drop := func() {
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.m, key)
		}
		k.mu.Unlock()
	}
	select {
	case l.ch <- struct{}{}:
		return func() { <-l.ch; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

func (a *Admin) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Admin) audit(actor Actor, action, target string, ts int64) *store.AuditRow {
	return &store.AuditRow{
		ID: store.NewID(), TS: ts, Actor: actor.ID, ActingAs: actor.ActingAs,
		Action: action, Target: sanitizeTarget(target), Outcome: OutcomeSuccess,
	}
}

// ListUsers returns every account with its role and state.
func (a *Admin) ListUsers(ctx context.Context) ([]store.UserAdminRow, error) {
	return a.Store.ListUsers(ctx)
}

// CreateRequest describes a new local account.
type CreateRequest struct {
	Username string
	// Email is optional.
	Email string
	// Role is "user" or "admin"; empty means "user".
	Role string
}

// CreatedUser is the result of [Admin.CreateUser].
type CreatedUser struct {
	User store.UserRow
	// TemporaryPassword is the generated password. It exists only here, in
	// the response that shows it once; the store keeps its hash.
	TemporaryPassword logx.Secret
}

// CreateUser creates a local account with a generated temporary password,
// flagged must-change, and the local role grant. It returns
// [ErrUsernameTaken], [ErrEmailTaken], [ErrInvalidRole] or an error that
// matches ops.ErrInvalidArgument for a bad user name or address.
func (a *Admin) CreateUser(ctx context.Context, actor Actor, req CreateRequest) (CreatedUser, error) {
	if err := ops.ValidateUsername("username", req.Username); err != nil {
		return CreatedUser{}, err
	}
	if err := ops.ValidateEmail("email", req.Email); err != nil {
		return CreatedUser{}, err
	}
	role := req.Role
	if role == "" {
		role = ops.RoleUser
	}
	if role != ops.RoleUser && role != ops.RoleAdmin {
		return CreatedUser{}, ErrInvalidRole
	}
	password, err := GeneratePassword(a.Random)
	if err != nil {
		return CreatedUser{}, err
	}
	hash, err := a.Hasher.Hash(ctx, password)
	if err != nil {
		return CreatedUser{}, fmt.Errorf("local: create user: %w", err)
	}
	ts := store.UnixMS(a.now())
	u := store.UserRow{
		ID: store.NewID(), Username: req.Username, Email: req.Email, PasswordHash: hash,
		MustChangePassword: true, CredentialVersion: 1, Active: true, Role: role,
		CreatedAt: ts, UpdatedAt: ts,
	}
	err = a.Store.CreateUser(ctx, store.CreateUserParams{User: u, Role: role, Audit: a.audit(actor, AuditActionUserCreate, u.ID, ts)})
	if err != nil {
		return CreatedUser{}, mapStoreErr(err)
	}
	return CreatedUser{User: u, TemporaryPassword: logx.Secret(password)}, nil
}

// ResetPassword sets a new generated temporary password, flags the account
// must-change and revokes all its sessions. It returns [ErrUserNotFound],
// [ErrUserInactive] or [ErrNoLocalPassword]. The password is returned once.
//
// An admin acting as a user must not change that user's password (AD-10): an
// actor that acts as somebody gets [ErrActAsPasswordChange].
func (a *Admin) ResetPassword(ctx context.Context, actor Actor, userID string) (logx.Secret, error) {
	if actor.ActingAs != "" {
		return "", ErrActAsPasswordChange
	}
	password, err := GeneratePassword(a.Random)
	if err != nil {
		return "", err
	}
	hash, err := a.Hasher.Hash(ctx, password)
	if err != nil {
		return "", fmt.Errorf("local: reset password: %w", err)
	}
	ts := store.UnixMS(a.now())
	err = a.Store.ResetPassword(ctx, store.ResetPasswordParams{
		UserID: userID, PasswordHash: hash, NowMS: ts,
		Audit: a.audit(actor, AuditActionUserResetPassword, userID, ts),
	})
	if err != nil {
		return "", mapStoreErr(err)
	}
	return logx.Secret(password), nil
}

// SetRole makes the local role grants of the user exactly {role} and returns
// the effective role. It returns [ErrUserNotFound], [ErrUserInactive],
// [ErrLastAdmin] or [ErrInvalidRole]. Grants that an IdP made are untouched.
func (a *Admin) SetRole(ctx context.Context, actor Actor, userID, role string) (string, error) {
	if role != ops.RoleUser && role != ops.RoleAdmin {
		return "", ErrInvalidRole
	}
	ts := store.UnixMS(a.now())
	eff, err := a.Store.SetLocalRole(ctx, store.SetLocalRoleParams{
		UserID: userID, Role: role, NowMS: ts, Audit: a.audit(actor, AuditActionUserSetRole, userID, ts),
	})
	if err != nil {
		return "", mapStoreErr(err)
	}
	return eff, nil
}

// Deactivate makes the account unable to sign in and ends its sessions and
// API keys, in one transaction. Its Workers stay with it. It returns
// [ErrUserNotFound] or [ErrLastAdmin]; deactivating a deactivated account
// succeeds with changed=false.
func (a *Admin) Deactivate(ctx context.Context, actor Actor, userID string) (changed bool, err error) {
	ts := store.UnixMS(a.now())
	p := store.DeactivateUserParams{
		UserID: userID, NowMS: ts, Audit: a.audit(actor, AuditActionUserDeactivate, userID, ts),
	}
	if a.RevokeKeys != nil {
		p.Hooks = []store.TxHook{a.RevokeKeys}
	}
	changed, err = a.Store.DeactivateUser(ctx, p)
	if err != nil {
		return false, mapStoreErr(err)
	}
	return changed, nil
}

// PasswordChange is a request of a user to change their own password.
type PasswordChange struct {
	// UserID is the account whose password changes.
	UserID string
	// SessionID is the session that asks. It stays signed in; every other
	// session of the user ends. Empty ends them all.
	SessionID   string
	OldPassword logx.Secret
	NewPassword logx.Secret
}

// ChangePassword verifies the old password, then stores the new one, bumps
// the credential version and revokes the user's other sessions.
//
// The version read before the old password was verified is compared in the
// update (compare-and-set): of two changes that raced, one wins and the other
// gets [ErrConcurrentChange]. A wrong old password returns [ErrWrongPassword]
// and counts toward the per-account back-off; a user in back-off gets a
// [*RateLimitedError] before the password is checked. An actor that acts as a
// user is refused with [ErrActAsPasswordChange] (AD-10).
//
// The verdicts on the old password of one account are given one at a time,
// and the back-off is checked again inside that section. Without it, a batch
// of concurrent guesses would all pass the back-off check before the first
// failure set it, queue on the hasher and then all be verified. The rest of
// the change (the new hash and the compare-and-set) runs outside the section.
func (a *Admin) ChangePassword(ctx context.Context, actor Actor, req PasswordChange) error {
	if actor.ActingAs != "" {
		return ErrActAsPasswordChange
	}
	key := "pwchange:" + req.UserID
	if err := a.checkBackoff(key); err != nil {
		return err
	}
	u, err := a.Store.UserByID(ctx, req.UserID)
	if err != nil {
		return mapStoreErr(err)
	}
	if !u.Active {
		return ErrUserInactive
	}
	if u.PasswordHash == "" {
		return ErrNoLocalPassword
	}
	if err := a.verifyOldPassword(ctx, key, u.PasswordHash, req.OldPassword); err != nil {
		return err
	}
	hash, err := a.Hasher.Hash(ctx, req.NewPassword.Reveal())
	if err != nil {
		return fmt.Errorf("local: change password: %w", err)
	}
	ts := store.UnixMS(a.now())
	_, err = a.Store.ChangePassword(ctx, store.ChangePasswordParams{
		UserID: req.UserID, ExpectedVersion: u.CredentialVersion, PasswordHash: hash,
		KeepSessionID: req.SessionID, NowMS: ts,
		Audit: a.audit(actor, AuditActionUserPasswordChange, req.UserID, ts),
	})
	if err != nil {
		return mapStoreErr(err)
	}
	if a.Backoff != nil {
		a.Backoff.BackoffReset(key)
	}
	return nil
}

func (a *Admin) checkBackoff(key string) error {
	if a.Backoff == nil {
		return nil
	}
	if wait := a.Backoff.Backoff(key); wait > 0 {
		return &RateLimitedError{RetryAfter: wait}
	}
	return nil
}

// verifyOldPassword checks the old password under the account's lock: it
// re-checks the back-off, verifies, and counts a wrong password.
func (a *Admin) verifyOldPassword(ctx context.Context, key, hash string, old logx.Secret) error {
	unlock, err := a.pwLocks.lock(ctx, key)
	if err != nil {
		return err
	}
	defer unlock()
	if err := a.checkBackoff(key); err != nil {
		return err
	}
	ok, _, err := a.Hasher.Verify(ctx, old.Reveal(), hash)
	if err != nil {
		return fmt.Errorf("local: verify password: %w", err)
	}
	if !ok {
		if a.Backoff != nil {
			a.Backoff.BackoffFail(key)
		}
		return ErrWrongPassword
	}
	return nil
}

// mapStoreErr translates the sentinel errors of the store into those of this
// package; other errors pass through.
func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrUserNotFound
	case errors.Is(err, store.ErrUsernameTaken):
		return ErrUsernameTaken
	case errors.Is(err, store.ErrEmailTaken):
		return ErrEmailTaken
	case errors.Is(err, store.ErrUserInactive):
		return ErrUserInactive
	case errors.Is(err, store.ErrNoLocalPassword):
		return ErrNoLocalPassword
	case errors.Is(err, store.ErrLastAdmin):
		return ErrLastAdmin
	case errors.Is(err, store.ErrVersionConflict):
		return ErrConcurrentChange
	}
	return err
}
