package ops

import (
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Names of the OIDC administration operations (AD-2, AD-10).
const (
	OpOIDCProviderList = "oidc_provider_list"
	OpOIDCProviderSet  = "oidc_provider_set"
	OpRoleRuleSet      = "role_rule_set"
)

// Limits of the OIDC operations.
const (
	// MaxOIDCFieldLen caps the text fields of a provider in bytes.
	MaxOIDCFieldLen = 512
	// MaxRoleRuleValues caps the claim values of one role rule.
	MaxRoleRuleValues = 100
	// MaxRoleRuleValueLen caps one claim value in bytes.
	MaxRoleRuleValueLen = 256
)

// OIDCDefaultProviderID is the id of the provider that the environment or the
// settings describe (MINION_OIDC_*). It is read-only in oidc_provider_set.
const OIDCDefaultProviderID = "default"

// Sources of a provider, reported by oidc_provider_list.
const (
	// OIDCSourceSettings marks the provider of the MINION_OIDC_* settings.
	OIDCSourceSettings = "settings"
	// OIDCSourcePanel marks a provider created with oidc_provider_set.
	OIDCSourcePanel = "panel"
)

// Warning codes of the OIDC write operations.
const (
	// WarnOIDCDiscoveryFailed says the provider was stored but its discovery
	// document could not be fetched now.
	WarnOIDCDiscoveryFailed = "oidc_discovery_failed"
	// WarnTLSVerificationOff says the provider skips TLS verification.
	WarnTLSVerificationOff = "tls_verification_off"
	// WarnLoginButtonAfterRestart says the login page lists the provider
	// after the next Hub restart.
	WarnLoginButtonAfterRestart = "login_button_after_restart"
)

var (
	// guardOIDCNotSettings refuses to change the provider that the settings
	// own (R72). It is KEEP: force does not override it.
	guardOIDCNotSettings = Guard{Name: "oidc_provider_not_settings", Class: GuardKeep, Reads: "oidc.source"}
	// guardOIDCDiscovery reports that the IdP did not answer. WARN: the
	// provider is stored anyway, the IdP may be down only for now.
	guardOIDCDiscovery = Guard{Name: "oidc_discovery", Class: GuardWarn, Reads: "oidc.discovery"}
	// guardOIDCInsecure reports a provider that skips TLS verification.
	guardOIDCInsecure = Guard{Name: "oidc_tls_insecure", Class: GuardWarn, Reads: "oidc.tls_insecure_skip_verify"}
	// guardOIDCProviderExists refuses a rule for an unknown provider.
	guardOIDCProviderExists = Guard{Name: "oidc_provider_exists", Class: GuardKeep, Reads: "oidc.provider"}
)

var oidcProviderID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// OIDCProviderListArgs are the arguments of oidc_provider_list (none).
type OIDCProviderListArgs struct{}

// OIDCProviderInfo describes one provider for the panel. It never carries the
// client secret.
type OIDCProviderInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	// ClientSecretSet tells whether a client secret is stored.
	ClientSecretSet bool   `json:"client_secret_set"`
	RoleClaim       string `json:"role_claim,omitempty"`
	// AdminValues and UserValues are the claim values of the role rules.
	AdminValues []string `json:"admin_values"`
	UserValues  []string `json:"user_values"`
	Scopes      string   `json:"scopes,omitempty"`
	CAFile      string   `json:"ca_file,omitempty"`
	// TLSInsecureSkipVerify is true when TLS verification of the IdP is off.
	// The panel shows a permanent warning then (R96, UX-DR8).
	TLSInsecureSkipVerify bool `json:"tls_insecure_skip_verify"`
	Enabled               bool `json:"enabled"`
	// Source is "settings" or "panel".
	Source string `json:"source"`
	// ReadOnly is true for the provider of the settings: change it with the
	// settings or the environment.
	ReadOnly bool `json:"read_only"`
}

// OIDCProviderListResult is the result of oidc_provider_list.
type OIDCProviderListResult struct {
	Providers []OIDCProviderInfo `json:"providers"`
	// InsecureTLS is true when any enabled provider skips TLS verification.
	InsecureTLS bool `json:"insecure_tls"`
}

// OIDCProviderSetArgs are the arguments of oidc_provider_set. It creates the
// provider or replaces all its fields; an empty ClientSecret keeps the stored
// one.
type OIDCProviderSetArgs struct {
	WriteArgs
	// ID is the provider's short id, used in sign-in URLs: lower-case letters,
	// digits, "-" and "_".
	ID string `json:"id"`
	// Delete removes the provider and its role rules; the other fields are
	// ignored.
	Delete   bool   `json:"delete,omitempty"`
	Name     string `json:"name,omitempty"`
	Issuer   string `json:"issuer,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	// ClientSecret is encrypted at rest and never echoed back.
	ClientSecret string `json:"client_secret,omitempty"`
	RoleClaim    string `json:"role_claim,omitempty"`
	// Scopes is space separated; empty means "openid profile email".
	Scopes string `json:"scopes,omitempty"`
	// CAFile is the path of a PEM file with the private CA of the IdP.
	CAFile                string `json:"ca_file,omitempty"`
	TLSInsecureSkipVerify bool   `json:"tls_insecure_skip_verify,omitempty"`
	// Enabled turns the provider on; omitted means true.
	Enabled *bool `json:"enabled,omitempty"`
}

// Validate checks the request id and the shape of every field. Whether the
// provider may be changed is decided on the Hub.
func (a OIDCProviderSetArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !oidcProviderID.MatchString(a.ID) {
		return invalid("id", "must be 1 to 32 characters: lower-case letters, digits, - and _")
	}
	if a.Delete {
		return nil
	}
	for _, f := range []struct {
		name, v string
		req     bool
	}{
		{"name", a.Name, true}, {"issuer", a.Issuer, true}, {"client_id", a.ClientID, true},
		{"client_secret", a.ClientSecret, false}, {"role_claim", a.RoleClaim, false},
		{"scopes", a.Scopes, false}, {"ca_file", a.CAFile, false},
	} {
		if f.req && strings.TrimSpace(f.v) == "" {
			return invalid(f.name, "is required")
		}
		if len(f.v) > MaxOIDCFieldLen || !utf8.ValidString(f.v) || strings.ContainsFunc(f.v, isControl) {
			return invalid(f.name, "must be valid text of at most %d bytes without control characters", MaxOIDCFieldLen)
		}
	}
	u, err := url.Parse(a.Issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return invalid("issuer", "must be an http(s) URL without credentials or fragment")
	}
	return nil
}

// RoleRuleSetArgs are the arguments of role_rule_set. It replaces the claim
// values that give Role for one panel provider; no value removes the rule.
type RoleRuleSetArgs struct {
	WriteArgs
	ProviderID string `json:"provider_id"`
	// Role is "admin" or "user".
	Role   string   `json:"role"`
	Values []string `json:"values"`
}

// Validate checks the request id, the role and the values.
func (a RoleRuleSetArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if !oidcProviderID.MatchString(a.ProviderID) {
		return invalid("provider_id", "must be a provider id")
	}
	if a.Role != "admin" && a.Role != "user" {
		return invalid("role", "must be admin or user")
	}
	if len(a.Values) > MaxRoleRuleValues {
		return invalid("values", "must have at most %d entries", MaxRoleRuleValues)
	}
	for _, v := range a.Values {
		if strings.TrimSpace(v) == "" || len(v) > MaxRoleRuleValueLen || !utf8.ValidString(v) || strings.ContainsFunc(v, isControl) {
			return invalid("values", "each value must be non-empty text of at most %d bytes", MaxRoleRuleValueLen)
		}
	}
	return nil
}

func init() {
	manage := MustActionSet(ActionManage)

	Register(Op{
		Name: OpOIDCProviderList, Summary: "List the OIDC providers with their role rules and whether TLS verification is off.",
		Scope: ScopeHub, Actions: manage,
		Args: reflect.TypeFor[OIDCProviderListArgs](), Result: reflect.TypeFor[OIDCProviderListResult](),
	})
	Register(Op{
		Name: OpOIDCProviderSet, Summary: "Create, replace or delete an OIDC provider set in the panel.",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardOIDCNotSettings, guardOIDCDiscovery, guardOIDCInsecure},
		Args:   reflect.TypeFor[OIDCProviderSetArgs](), Result: reflect.TypeFor[WriteResult](),
	})
	Register(Op{
		Name: OpRoleRuleSet, Summary: "Set the claim values that give the admin or user role for a panel OIDC provider.",
		Scope: ScopeHub, Actions: manage, Write: true,
		Guards: []Guard{guardOIDCProviderExists},
		Args:   reflect.TypeFor[RoleRuleSetArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
