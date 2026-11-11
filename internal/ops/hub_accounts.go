package ops

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/mail"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Names of the account administration operations (AD-2, AD-10, R37, R39,
// R57).
const (
	OpUsersList         = "users_list"
	OpUserCreate        = "user_create"
	OpUserResetPassword = "user_reset_password"
	OpUserSetRole       = "user_set_role"
	OpUserDeactivate    = "user_deactivate"
	OpPasswordChange    = "password_change"
)

// Roles of a user (AD-10).
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// Role sources (AD-10): a grant made by an admin in the panel is local; one
// made by an IdP role rule is idp.
const (
	RoleSourceLocal = "local"
	RoleSourceIdP   = "idp"
)

// Limits of the account operations.
const (
	// MaxUsernameLen is the longest user name in characters.
	MaxUsernameLen = 64
	// MaxEmailLen is the longest e-mail address in bytes (RFC 5321).
	MaxEmailLen = 254
	// MinPasswordLen is the shortest password a user can choose, in
	// characters. Generated temporary passwords are longer.
	MinPasswordLen = 10
	// MaxPasswordLen is the longest accepted password in bytes. It equals
	// local.MaxPasswordLen, which the kernel cannot import.
	MaxPasswordLen = 1024
)

// Guards of the account operations. They are all KEEP: an account operation
// has nothing a caller may override with force.
var (
	guardUsernameFree      = Guard{Name: "username_free", Class: GuardKeep, Reads: "users.username"}
	guardEmailFree         = Guard{Name: "email_free", Class: GuardKeep, Reads: "users.email"}
	guardUserActive        = Guard{Name: "user_active", Class: GuardKeep, Reads: "users.active"}
	guardLocalPassword     = Guard{Name: "local_password", Class: GuardKeep, Reads: "users.password_hash"}
	guardLastAdmin         = Guard{Name: "last_admin", Class: GuardKeep, Reads: "users.roles"}
	guardNotActAs          = Guard{Name: "not_act_as", Class: GuardKeep, Reads: "principal.kind"}
	guardCurrentPassword   = Guard{Name: "current_password", Class: GuardKeep, Reads: "users.password_hash"}
	guardCredentialVersion = Guard{Name: "credential_version", Class: GuardKeep, Reads: "users.credential_version"}
	guardInteractiveLogin  = Guard{Name: "interactive_session", Class: GuardKeep, Reads: "principal.kind"}
)

// ValidateUsername checks a user name: 1 to [MaxUsernameLen] characters of
// valid UTF-8, no control characters and no leading or trailing space.
func ValidateUsername(field, name string) error {
	switch {
	case name == "" || utf8.RuneCountInString(name) > MaxUsernameLen:
		return invalid(field, "must have 1 to %d characters", MaxUsernameLen)
	case !utf8.ValidString(name):
		return invalid(field, "must be valid UTF-8")
	case name != strings.TrimSpace(name):
		return invalid(field, "must not start or end with space")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return invalid(field, "must not contain control characters")
		}
	}
	return nil
}

// ValidateEmail accepts an empty address (an account may have none) or a
// plain address of the form local@domain without a display name.
func ValidateEmail(field, s string) error {
	if s == "" {
		return nil
	}
	if len(s) > MaxEmailLen || !utf8.ValidString(s) {
		return invalid(field, "must be a valid e-mail address of at most %d bytes", MaxEmailLen)
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || addr.Name != "" {
		return invalid(field, "must be a plain e-mail address such as name@example.com")
	}
	return nil
}

// validateUserID checks the id of a target user.
func validateUserID(a string) error {
	if !ValidUUIDv7(a) {
		return invalid("user_id", "must be a UUIDv7")
	}
	return nil
}

// validateRole accepts the two roles.
func validateRole(role string) error {
	if role != RoleUser && role != RoleAdmin {
		return invalid("role", "must be %q or %q", RoleUser, RoleAdmin)
	}
	return nil
}

// UserInfo describes one account for the admin panel. It never holds a
// password or its hash.
type UserInfo struct {
	ID       string `json:"id"` // UUIDv7
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
	// Role is the effective role: admin when any admin grant exists.
	Role string `json:"role"`
	// AdminSources lists where an admin grant comes from ("local", "idp");
	// empty for a plain user. Demoting a local admin does not remove an idp
	// grant, so the effective role can stay admin.
	AdminSources []string `json:"admin_sources"`
	Active       bool     `json:"active"`
	// HasPassword is true for an account with a local password.
	HasPassword bool `json:"has_password"`
	// MustChangePassword is true while the account still has a temporary
	// password.
	MustChangePassword bool      `json:"must_change_password"`
	CreatedAt          Timestamp `json:"created_at"`
}

// UsersListArgs are the arguments of users_list (none).
type UsersListArgs struct{}

// UsersListResult is the result of users_list, ordered by user name.
type UsersListResult struct {
	Users []UserInfo `json:"users"`
}

// MarshalJSON writes users as an empty array instead of null.
func (r UsersListResult) MarshalJSON() ([]byte, error) {
	type plain UsersListResult
	if r.Users == nil {
		r.Users = []UserInfo{}
	}
	for i := range r.Users {
		if r.Users[i].AdminSources == nil {
			r.Users[i].AdminSources = []string{}
		}
	}
	return json.Marshal(plain(r))
}

// UserCreateArgs are the arguments of user_create.
type UserCreateArgs struct {
	WriteArgs
	// Username is the sign-in name; unique, compared case-insensitively.
	Username string `json:"username"`
	// Email is optional. When set it is unique and also works as sign-in
	// name.
	Email string `json:"email,omitempty"`
	// Role is "user" (the default) or "admin"; the grant has source local.
	Role string `json:"role,omitempty"`
}

// Validate checks the request id, the user name, the e-mail address and the
// role.
func (a UserCreateArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if err := ValidateUsername("username", a.Username); err != nil {
		return err
	}
	if err := ValidateEmail("email", a.Email); err != nil {
		return err
	}
	if a.Role != "" {
		return validateRole(a.Role)
	}
	return nil
}

// EffectiveRole returns Role, or "user" when it is empty.
func (a UserCreateArgs) EffectiveRole() string {
	if a.Role == "" {
		return RoleUser
	}
	return a.Role
}

// UserCreateResult is what the Hub returns for user_create: the fields of a
// [WriteResult] plus the new account and its temporary password, which is
// shown this once and never stored in clear (R57).
//
// The catalog declares WriteResult as the result type of every write op, and
// a JSON consumer reads this value as a WriteResult with extra members.
type UserCreateResult struct {
	WriteResult
	User UserInfo `json:"user"`
	// TemporaryPassword is shown once. The account is flagged
	// must-change.
	TemporaryPassword string `json:"temporary_password"`
}

// MarshalJSON writes the WriteResult members and the extra members flat. The
// method of the embedded WriteResult would otherwise hide the extras.
func (r UserCreateResult) MarshalJSON() ([]byte, error) {
	if r.Warnings == nil {
		r.Warnings = []Warning{}
	}
	if r.User.AdminSources == nil {
		r.User.AdminSources = []string{}
	}
	type plain WriteResult // no methods, so the members are promoted
	return json.Marshal(struct {
		plain
		User              UserInfo `json:"user"`
		TemporaryPassword string   `json:"temporary_password"`
	}{plain(r.WriteResult), r.User, r.TemporaryPassword})
}

// LogValue keeps the temporary password out of structured logs.
func (r UserCreateResult) LogValue() slog.Value {
	return slog.GroupValue(slog.String("user_id", r.User.ID), slog.String("temporary_password", "[redacted]"))
}

// String keeps the temporary password out of formatted output.
func (r UserCreateResult) String() string {
	return fmt.Sprintf("UserCreateResult{user_id: %s, temporary_password: [redacted]}", r.User.ID)
}

// GoString keeps the temporary password out of %#v output.
func (r UserCreateResult) GoString() string { return r.String() }

// UserResetPasswordArgs are the arguments of user_reset_password.
type UserResetPasswordArgs struct {
	WriteArgs
	// UserID is the account whose password is reset.
	UserID string `json:"user_id"`
	// KeepAPIKeys keeps the user's API keys. By default a reset revokes all of
	// them in the same transaction (F-07).
	KeepAPIKeys bool `json:"keep_api_keys,omitempty"`
}

// Validate checks the request id and the user id.
func (a UserResetPasswordArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	return validateUserID(a.UserID)
}

// UserResetPasswordResult is the result of user_reset_password: a
// WriteResult plus the new temporary password, shown once.
type UserResetPasswordResult struct {
	WriteResult
	UserID            string `json:"user_id"`
	TemporaryPassword string `json:"temporary_password"`
}

// MarshalJSON writes the WriteResult members and the extra members flat.
func (r UserResetPasswordResult) MarshalJSON() ([]byte, error) {
	if r.Warnings == nil {
		r.Warnings = []Warning{}
	}
	type plain WriteResult
	return json.Marshal(struct {
		plain
		UserID            string `json:"user_id"`
		TemporaryPassword string `json:"temporary_password"`
	}{plain(r.WriteResult), r.UserID, r.TemporaryPassword})
}

// LogValue keeps the temporary password out of structured logs.
func (r UserResetPasswordResult) LogValue() slog.Value {
	return slog.GroupValue(slog.String("user_id", r.UserID), slog.String("temporary_password", "[redacted]"))
}

// String keeps the temporary password out of formatted output.
func (r UserResetPasswordResult) String() string {
	return fmt.Sprintf("UserResetPasswordResult{user_id: %s, temporary_password: [redacted]}", r.UserID)
}

// GoString keeps the temporary password out of %#v output.
func (r UserResetPasswordResult) GoString() string { return r.String() }

// UserSetRoleArgs are the arguments of user_set_role.
type UserSetRoleArgs struct {
	WriteArgs
	UserID string `json:"user_id"`
	// Role is the local role to grant: "admin" adds the local admin grant,
	// "user" removes it. Grants that come from an IdP are never touched.
	Role string `json:"role"`
}

// Validate checks the request id, the user id and the role.
func (a UserSetRoleArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if err := validateUserID(a.UserID); err != nil {
		return err
	}
	return validateRole(a.Role)
}

// UserDeactivateArgs are the arguments of user_deactivate.
type UserDeactivateArgs struct {
	WriteArgs
	UserID string `json:"user_id"`
}

// Validate checks the request id and the user id.
func (a UserDeactivateArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	return validateUserID(a.UserID)
}

// PasswordChangeArgs are the arguments of password_change. The caller changes
// their own password; there is no user id.
type PasswordChangeArgs struct {
	WriteArgs
	// OldPassword is the current password, verified before anything changes.
	OldPassword string `json:"old_password"`
	// NewPassword is the new password, at least [MinPasswordLen] characters
	// and different from the old one.
	NewPassword string `json:"new_password"`
}

// Validate checks the request id and the shape of both passwords. Whether the
// old password is right is decided on the Hub.
func (a PasswordChangeArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if a.OldPassword == "" || len(a.OldPassword) > MaxPasswordLen {
		return invalid("old_password", "must be the current password")
	}
	if !utf8.ValidString(a.NewPassword) || utf8.RuneCountInString(a.NewPassword) < MinPasswordLen {
		return invalid("new_password", "must be valid UTF-8 of at least %d characters", MinPasswordLen)
	}
	if len(a.NewPassword) > MaxPasswordLen {
		return invalid("new_password", "must be at most %d bytes", MaxPasswordLen)
	}
	if a.NewPassword == a.OldPassword {
		return invalid("new_password", "must differ from the old password")
	}
	return nil
}

// LogValue keeps both passwords out of structured logs (AD-20).
func (a PasswordChangeArgs) LogValue() slog.Value {
	return slog.GroupValue(slog.String("request_id", a.RequestID), slog.String("old_password", "[redacted]"), slog.String("new_password", "[redacted]"))
}

// String keeps both passwords out of formatted output.
func (a PasswordChangeArgs) String() string {
	return "PasswordChangeArgs{request_id: " + a.RequestID + ", old_password: [redacted], new_password: [redacted]}"
}

// GoString keeps both passwords out of %#v output.
func (a PasswordChangeArgs) GoString() string { return a.String() }

func init() {
	manage := MustActionSet(ActionManage)
	// A user changes their own password: the owner of an account holds
	// operate on it, and so does an admin on their own.
	own := MustActionSet(ActionOperate)

	Register(Op{
		Name: OpUsersList, Summary: "List the accounts with their role, state and whether the password is temporary.",
		Scope: ScopeHub, Actions: manage,
		Args: reflect.TypeFor[UsersListArgs](), Result: reflect.TypeFor[UsersListResult](),
	})
	Register(Op{
		Name: OpUserCreate, Summary: "Create a local account with a generated temporary password that is shown once.",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardUsernameFree, guardEmailFree},
		Args:   reflect.TypeFor[UserCreateArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpUserResetPassword, Summary: "Set a new temporary password for a local account and sign the user out everywhere.",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardUserActive, guardLocalPassword, guardNotActAs},
		Args:   reflect.TypeFor[UserResetPasswordArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpUserSetRole, Summary: "Grant or remove the local admin role of an account.",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardUserActive, guardLastAdmin},
		Args:   reflect.TypeFor[UserSetRoleArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpUserDeactivate, Summary: "Deactivate an account: it cannot sign in, its sessions and API keys end and its Workers stay with it.",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardLastAdmin},
		Args:   reflect.TypeFor[UserDeactivateArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpPasswordChange, Summary: "Change your own password: the old one is verified and your other sessions end.",
		Scope: ScopeHub, Actions: own, Write: true,
		Guards: []Guard{guardInteractiveLogin, guardNotActAs, guardCurrentPassword, guardCredentialVersion},
		Args:   reflect.TypeFor[PasswordChangeArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
