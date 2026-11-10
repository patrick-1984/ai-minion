package ops

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strings"
)

// Names of the linked-account operations (AD-2, AD-10, R67-R70).
const (
	OpIdentityList       = "identity_list"
	OpIdentityDetach     = "identity_detach"
	OpUserSetPassword    = "user_set_password"
	OpAdminLinkConsent   = "admin_link_consent"
	OpOIDCLinkPolicyList = "oidc_link_policy_list"
	OpOIDCLinkPolicySet  = "oidc_link_policy_set"
)

// Limits of the linking operations.
const (
	// MaxLinkDomains caps the e-mail domains of one provider's allow-list.
	MaxLinkDomains = 100
	// MaxLinkDomainLen is the longest domain in bytes (RFC 1035).
	MaxLinkDomainLen = 253
)

// Guards of the linking operations. They are all KEEP: nothing here can be
// overridden with force.
var (
	guardIdentityExists = Guard{Name: "identity_exists", Class: GuardKeep, Reads: "identities.id"}
	// guardKeepsSignInMethod refuses to detach the only way an account can
	// sign in.
	guardKeepsSignInMethod = Guard{Name: "keeps_sign_in_method", Class: GuardKeep, Reads: "users.password_hash"}
	// guardNoLocalPassword refuses a first password for an account that has
	// one (user_reset_password applies then).
	guardNoLocalPassword = Guard{Name: "no_local_password", Class: GuardKeep, Reads: "users.password_hash"}
	// guardAdminAccount refuses consent for an account that is not an admin.
	guardAdminAccount = Guard{Name: "admin_account", Class: GuardKeep, Reads: "users.roles"}
)

// linkDomain matches a host name in lower case: dot separated labels of
// letters, digits and hyphens that neither start nor end with a hyphen.
var linkDomain = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// NormalizeLinkDomain returns the canonical form of an e-mail domain: trimmed,
// lower case, without a leading "@".
func NormalizeLinkDomain(d string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
}

// ValidateLinkDomain checks one allow-list entry after normalisation.
func ValidateLinkDomain(field, d string) error {
	n := NormalizeLinkDomain(d)
	if n == "" || len(n) > MaxLinkDomainLen || !linkDomain.MatchString(n) {
		return invalid(field, "each entry must be a domain name such as example.com (no wildcard, no user part)")
	}
	return nil
}

// IdentityInfo describes one external identity of an account for the admin
// panel. It never holds a token.
type IdentityInfo struct {
	ID          string    `json:"id"` // UUIDv7
	UserID      string    `json:"user_id"`
	ProviderID  string    `json:"provider_id"`
	Issuer      string    `json:"issuer"`
	Subject     string    `json:"subject"`
	CreatedAt   Timestamp `json:"created_at"`
	LastLoginAt Timestamp `json:"last_login_at"`
}

// IdentityListArgs are the arguments of identity_list.
type IdentityListArgs struct {
	// UserID limits the list to one account; empty lists every identity.
	UserID string `json:"user_id,omitempty"`
}

// Validate checks the user id when it is given.
func (a IdentityListArgs) Validate() error {
	if a.UserID == "" {
		return nil
	}
	return validateUserID(a.UserID)
}

// IdentityListResult is the result of identity_list, oldest first.
type IdentityListResult struct {
	Identities []IdentityInfo `json:"identities"`
}

// MarshalJSON writes identities as an empty array instead of null.
func (r IdentityListResult) MarshalJSON() ([]byte, error) {
	type plain IdentityListResult
	if r.Identities == nil {
		r.Identities = []IdentityInfo{}
	}
	return json.Marshal(plain(r))
}

// IdentityDetachArgs are the arguments of identity_detach.
type IdentityDetachArgs struct {
	WriteArgs
	// IdentityID is the id of the identity, from identity_list.
	IdentityID string `json:"identity_id"`
}

// Validate checks the request id and the identity id.
func (a IdentityDetachArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !ValidUUIDv7(a.IdentityID) {
		return invalid("identity_id", "must be a UUIDv7")
	}
	return nil
}

// UserSetPasswordArgs are the arguments of user_set_password: the admin gives
// an account that an IdP created its first local password. The password is
// generated and returned once; it is never an argument.
type UserSetPasswordArgs struct {
	WriteArgs
	UserID string `json:"user_id"`
	// Email is optional. The user signs in locally by user name or e-mail;
	// an account an IdP created has no e-mail address, so set one here to let
	// the user sign in by it. It must be free.
	Email string `json:"email,omitempty"`
}

// Validate checks the request id, the user id and the e-mail address.
func (a UserSetPasswordArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if err := validateUserID(a.UserID); err != nil {
		return err
	}
	return ValidateEmail("email", a.Email)
}

// UserSetPasswordResult is what the Hub returns for user_set_password: the
// fields of a [WriteResult] plus the temporary password, shown this once. The
// account is flagged must-change.
type UserSetPasswordResult struct {
	WriteResult
	UserID            string `json:"user_id"`
	TemporaryPassword string `json:"temporary_password"`
}

// MarshalJSON writes the WriteResult members and the extra members flat.
func (r UserSetPasswordResult) MarshalJSON() ([]byte, error) {
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
func (r UserSetPasswordResult) LogValue() slog.Value {
	return slog.GroupValue(slog.String("user_id", r.UserID), slog.String("temporary_password", "[redacted]"))
}

// String keeps the temporary password out of formatted output.
func (r UserSetPasswordResult) String() string {
	return fmt.Sprintf("UserSetPasswordResult{user_id: %s, temporary_password: [redacted]}", r.UserID)
}

// GoString keeps the temporary password out of %#v output.
func (r UserSetPasswordResult) GoString() string { return r.String() }

// AdminLinkConsentArgs are the arguments of admin_link_consent.
type AdminLinkConsentArgs struct {
	WriteArgs
	// UserID is the admin account.
	UserID string `json:"user_id"`
	// Granted turns the consent on or off. Off is the default: an e-mail link
	// into an admin account needs it (R68).
	Granted bool `json:"granted"`
}

// Validate checks the request id and the user id.
func (a AdminLinkConsentArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	return validateUserID(a.UserID)
}

// OIDCLinkPolicyInfo is the linking policy of one provider.
type OIDCLinkPolicyInfo struct {
	ProviderID string `json:"provider_id"`
	// LinkingAllowed lets a first sign-in link to the local account with the
	// same e-mail address, when the IdP verified it and its domain is allowed.
	LinkingAllowed bool `json:"linking_allowed"`
	// AutoCreate creates a separate account when linking is refused, instead
	// of refusing the sign-in.
	AutoCreate bool     `json:"auto_create"`
	Domains    []string `json:"domains"`
}

// OIDCLinkPolicyListArgs are the arguments of oidc_link_policy_list (none).
type OIDCLinkPolicyListArgs struct{}

// OIDCLinkPolicyListResult is the result of oidc_link_policy_list. A provider
// without an entry has everything off.
type OIDCLinkPolicyListResult struct {
	Policies []OIDCLinkPolicyInfo `json:"policies"`
}

// MarshalJSON writes policies and domains as empty arrays instead of null.
func (r OIDCLinkPolicyListResult) MarshalJSON() ([]byte, error) {
	type plain OIDCLinkPolicyListResult
	if r.Policies == nil {
		r.Policies = []OIDCLinkPolicyInfo{}
	}
	for i := range r.Policies {
		if r.Policies[i].Domains == nil {
			r.Policies[i].Domains = []string{}
		}
	}
	return json.Marshal(plain(r))
}

// OIDCLinkPolicySetArgs are the arguments of oidc_link_policy_set. It replaces
// the whole policy of the provider.
type OIDCLinkPolicySetArgs struct {
	WriteArgs
	ProviderID     string   `json:"provider_id"`
	LinkingAllowed bool     `json:"linking_allowed"`
	AutoCreate     bool     `json:"auto_create"`
	Domains        []string `json:"domains"`
}

// Validate checks the request id, the provider id and the domains.
func (a OIDCLinkPolicySetArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !oidcProviderID.MatchString(a.ProviderID) {
		return invalid("provider_id", "must be a provider id")
	}
	if len(a.Domains) > MaxLinkDomains {
		return invalid("domains", "must have at most %d entries", MaxLinkDomains)
	}
	for _, d := range a.Domains {
		if err := ValidateLinkDomain("domains", d); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	manage := MustActionSet(ActionManage)

	Register(Op{
		Name: OpIdentityList, Summary: "List the IdP identities linked to accounts, for one account or for all.",
		Scope: ScopeHub, Actions: manage,
		Args: reflect.TypeFor[IdentityListArgs](), Result: reflect.TypeFor[IdentityListResult](),
	})
	Register(Op{
		Name: OpIdentityDetach, Summary: "Detach an IdP identity from an account: its IdP roles and sessions end, the account stays.",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardIdentityExists, guardKeepsSignInMethod, guardLastAdmin},
		Args:   reflect.TypeFor[IdentityDetachArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpUserSetPassword, Summary: "Give an account that signs in through an IdP a local temporary password, shown once, and optionally an e-mail address.",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardUserActive, guardNoLocalPassword, guardEmailFree, guardNotActAs},
		Args:   reflect.TypeFor[UserSetPasswordArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpAdminLinkConsent, Summary: "Allow or forbid an e-mail link of an IdP identity into an admin account (off by default).",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardAdminAccount, guardNotActAs},
		Args:   reflect.TypeFor[AdminLinkConsentArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpOIDCLinkPolicyList, Summary: "List the e-mail linking policy of the OIDC providers.",
		Scope: ScopeHub, Actions: manage,
		Args: reflect.TypeFor[OIDCLinkPolicyListArgs](), Result: reflect.TypeFor[OIDCLinkPolicyListResult](),
	})
	Register(Op{
		Name: OpOIDCLinkPolicySet, Summary: "Set whether a provider may link a first sign-in to a local account by e-mail, for which domains, and whether a separate account is created instead of refusing.",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardOIDCProviderExists},
		Args:   reflect.TypeFor[OIDCLinkPolicySetArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
