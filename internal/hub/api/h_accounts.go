package api

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/patrick-1984/ai-minion/internal/hub/auth/local"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/logx"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// AccountService is the port of the account administration handlers;
// *local.Admin implements it (Story 3.10).
type AccountService interface {
	ListUsers(ctx context.Context) ([]store.UserAdminRow, error)
	CreateUser(ctx context.Context, actor local.Actor, req local.CreateRequest) (local.CreatedUser, error)
	ResetPassword(ctx context.Context, actor local.Actor, userID string) (logx.Secret, error)
	SetRole(ctx context.Context, actor local.Actor, userID, role string) (string, error)
	Deactivate(ctx context.Context, actor local.Actor, userID string) (bool, error)
	ChangePassword(ctx context.Context, actor local.Actor, req local.PasswordChange) error
}

var _ AccountService = (*local.Admin)(nil)

// RegisterAccounts registers the handlers of users_list, user_create,
// user_reset_password, user_set_role, user_deactivate and password_change in
// r. It fails when the catalog of r lacks one of them or one is registered
// already.
//
// Authorisation is decided by the dispatcher before a handler runs:
//
//   - users_list and user_create need manage on the caller's own user
//     resource, which only admins hold;
//   - user_reset_password, user_set_role and user_deactivate need manage on
//     the target user (admins only; any other caller is refused);
//   - password_change needs operate on the caller's own user resource, which
//     the owner of an account holds.
//
// API keys whose scope does not reach the whole Hub cannot list or create
// users, and no API key can change a password: the old password is proof of
// a person at the keyboard, so the call is for browser sessions only.
func RegisterAccounts(r *Registry, svc AccountService) error {
	h := &accountHandlers{svc: svc}
	target := func(userID func(args any) (string, bool)) HandlerOption {
		return WithResource(func(_ authz.Principal, args any) (authz.Resource, error) {
			id, ok := userID(args)
			if !ok || id == "" {
				return nil, invalidArg("user_id", "is required")
			}
			return authz.UserResource{ID: id}, nil
		})
	}
	steps := []func() error{
		func() error { return HandleTyped(r, ops.OpUsersList, h.list) },
		// user_create and user_reset_password return a result that extends
		// WriteResult with the temporary password, so they are registered
		// without the type check of HandleTyped.
		func() error { return r.Handle(ops.OpUserCreate, h.create) },
		func() error {
			return r.Handle(ops.OpUserResetPassword, h.resetPassword, target(func(a any) (string, bool) {
				v, ok := a.(*ops.UserResetPasswordArgs)
				if !ok {
					return "", false
				}
				return v.UserID, true
			}))
		},
		func() error {
			return HandleTyped(r, ops.OpUserSetRole, h.setRole, target(func(a any) (string, bool) {
				v, ok := a.(*ops.UserSetRoleArgs)
				if !ok {
					return "", false
				}
				return v.UserID, true
			}))
		},
		func() error {
			return HandleTyped(r, ops.OpUserDeactivate, h.deactivate, target(func(a any) (string, bool) {
				v, ok := a.(*ops.UserDeactivateArgs)
				if !ok {
					return "", false
				}
				return v.UserID, true
			}))
		},
		func() error { return HandleTyped(r, ops.OpPasswordChange, h.changePassword) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return fmt.Errorf("api: register account operations: %w", err)
		}
	}
	return nil
}

type accountHandlers struct{ svc AccountService }

// requireHubWideScope refuses an API key whose scope does not reach the whole
// Hub. users_list and user_create are decided on the key owner's own user
// resource but act on every account, and an account is no resource a scope
// could be tested on. Only an everything scope grants manage on a user the key
// was never given. Browser sessions are not scoped.
func requireHubWideScope(p authz.Principal) error {
	if p.Kind != authz.KindAPIKey {
		return nil
	}
	if p.Scope != nil {
		id := ops.NewUUIDv7() // a user no chosen-users scope can list
		probe := authz.ResolvedResource{Kind: authz.ResourceUser, UserID: id, OwnerID: id}
		if slices.Contains(p.Scope.Actions(probe), ops.ActionManage) {
			return nil
		}
	}
	return NewError(proto.CodeForbidden, "this API key is limited to some resources; administering accounts takes an everything scope")
}

func actorOf(c *Call) local.Actor {
	return local.Actor{ID: c.Identity.Actor, ActingAs: c.Identity.ActingAs}
}

func delivered() ops.WriteResult {
	return ops.WriteResult{Outcome: ops.OutcomeDelivered, Warnings: []ops.Warning{}}
}

func userInfo(u store.UserRow, sources []string) ops.UserInfo {
	return ops.UserInfo{
		ID: u.ID, Username: u.Username, Email: u.Email, Role: u.Role,
		AdminSources: sources, Active: u.Active, HasPassword: u.PasswordHash != "",
		MustChangePassword: u.MustChangePassword, CreatedAt: ops.Timestamp(store.FromUnixMS(u.CreatedAt)),
	}
}

func (h *accountHandlers) list(ctx context.Context, c *Call, _ ops.UsersListArgs) (ops.UsersListResult, error) {
	if err := requireHubWideScope(c.Principal); err != nil {
		return ops.UsersListResult{}, err
	}
	rows, err := h.svc.ListUsers(ctx)
	if err != nil {
		return ops.UsersListResult{}, err
	}
	out := ops.UsersListResult{Users: make([]ops.UserInfo, 0, len(rows))}
	for _, r := range rows {
		out.Users = append(out.Users, userInfo(r.UserRow, r.AdminSources))
	}
	return out, nil
}

func (h *accountHandlers) create(ctx context.Context, c *Call) (any, error) {
	args, ok := c.Args.(*ops.UserCreateArgs)
	if !ok {
		return nil, fmt.Errorf("api: user_create was called with %T", c.Args)
	}
	if err := requireHubWideScope(c.Principal); err != nil {
		return nil, err
	}
	created, err := h.svc.CreateUser(ctx, actorOf(c), local.CreateRequest{
		Username: args.Username, Email: args.Email, Role: args.Role,
	})
	if err != nil {
		return nil, accountError(err)
	}
	var sources []string
	if created.User.Role == ops.RoleAdmin {
		sources = []string{ops.RoleSourceLocal}
	}
	return ops.UserCreateResult{
		WriteResult:       delivered(),
		User:              userInfo(created.User, sources),
		TemporaryPassword: created.TemporaryPassword.Reveal(),
	}, nil
}

func (h *accountHandlers) resetPassword(ctx context.Context, c *Call) (any, error) {
	args, ok := c.Args.(*ops.UserResetPasswordArgs)
	if !ok {
		return nil, fmt.Errorf("api: user_reset_password was called with %T", c.Args)
	}
	if c.Principal.Kind == authz.KindActAs {
		return nil, errActAsPassword
	}
	pw, err := h.svc.ResetPassword(ctx, actorOf(c), args.UserID)
	if err != nil {
		return nil, accountError(err)
	}
	return ops.UserResetPasswordResult{WriteResult: delivered(), UserID: args.UserID, TemporaryPassword: pw.Reveal()}, nil
}

func (h *accountHandlers) setRole(ctx context.Context, c *Call, args ops.UserSetRoleArgs) (ops.WriteResult, error) {
	if _, err := h.svc.SetRole(ctx, actorOf(c), args.UserID, args.Role); err != nil {
		return ops.WriteResult{}, accountError(err)
	}
	return delivered(), nil
}

func (h *accountHandlers) deactivate(ctx context.Context, c *Call, args ops.UserDeactivateArgs) (ops.WriteResult, error) {
	if _, err := h.svc.Deactivate(ctx, actorOf(c), args.UserID); err != nil {
		return ops.WriteResult{}, accountError(err)
	}
	return delivered(), nil
}

var errActAsPassword = NewError(proto.CodeForbidden, "an admin acting as a user cannot change that user's password")

func (h *accountHandlers) changePassword(ctx context.Context, c *Call, args ops.PasswordChangeArgs) (ops.WriteResult, error) {
	switch {
	case c.Principal.Kind == authz.KindActAs:
		return ops.WriteResult{}, errActAsPassword
	case c.Principal.Kind != authz.KindBrowserSession || c.Identity.Session == nil:
		return ops.WriteResult{}, NewError(proto.CodeForbidden, "a password is changed from a signed-in browser session, not with an API key")
	}
	err := h.svc.ChangePassword(ctx, actorOf(c), local.PasswordChange{
		UserID: c.Principal.UserID, SessionID: c.Identity.Session.ID,
		OldPassword: logx.Secret(args.OldPassword), NewPassword: logx.Secret(args.NewPassword),
	})
	if err != nil {
		return ops.WriteResult{}, accountError(err)
	}
	return delivered(), nil
}

// accountError maps the errors of the account service to API errors. Anything
// it does not know becomes internal through [ErrorOf].
func accountError(err error) error {
	var limited *local.RateLimitedError
	switch {
	case errors.As(err, &limited):
		e := NewError(proto.CodeRateLimited, "too many wrong passwords; retry later")
		e.RetryAfter = limited.RetryAfter
		return e
	case errors.Is(err, local.ErrUserNotFound):
		return NewError(proto.CodeNotFound, "not found")
	case errors.Is(err, local.ErrUsernameTaken):
		return NewError(proto.CodeConflict, "that user name is taken").WithDetails(map[string]any{"field": "username"})
	case errors.Is(err, local.ErrEmailTaken):
		return NewError(proto.CodeConflict, "that e-mail address is taken").WithDetails(map[string]any{"field": "email"})
	case errors.Is(err, local.ErrUserInactive):
		return NewError(proto.CodeConflict, "the account is deactivated")
	case errors.Is(err, local.ErrNoLocalPassword):
		return NewError(proto.CodeConflict, "the account has no local password")
	case errors.Is(err, local.ErrLastAdmin):
		return NewError(proto.CodeConflict, "this is the last active admin; make another admin first")
	case errors.Is(err, local.ErrConcurrentChange):
		return NewError(proto.CodeConflict, "the credentials changed in the meantime; try again")
	case errors.Is(err, local.ErrWrongPassword):
		return invalidArg("old_password", "is not the current password")
	case errors.Is(err, local.ErrActAsPasswordChange):
		return errActAsPassword
	case errors.Is(err, local.ErrInvalidRole):
		return invalidArg("role", "must be user or admin")
	}
	return err
}
