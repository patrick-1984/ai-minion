package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Names of the Hub OIDC settings, for callers that look them up by key.
const (
	KeyOIDCName         = "oidc_name"
	KeyOIDCIssuer       = "oidc_issuer"
	KeyOIDCClientID     = "oidc_client_id"
	KeyOIDCClientSecret = "oidc_client_secret"
	KeyOIDCRoleClaim    = "oidc_role_claim"
	KeyOIDCAdminValues  = "oidc_admin_values"
	KeyOIDCUserValues   = "oidc_user_values"
	KeyOIDCScopes       = "oidc_scopes"
	KeyOIDCTimeout      = "oidc_timeout"
	KeyOIDCTLSInsecure  = "oidc_tls_insecure_skip_verify"

	// KeyOIDCCAFile names the panel key of the private CA file. Its
	// environment name is MINION_OIDC_CA_FILE ([EnvOIDCCAFile]). Like the TLS
	// certificate paths it is not declared in the registry: the registry
	// reserves the "_FILE" suffix for the path variable of a secret, and this
	// variable holds a file path itself. [ResolveHubOIDC] reads it directly
	// (environment, then the panel key).
	KeyOIDCCAFile = "oidc_ca_file"
	EnvOIDCCAFile = "MINION_OIDC_CA_FILE"
)

// Defaults and bounds of the OIDC settings.
const (
	// DefaultOIDCName is the label of the provider on the login page.
	DefaultOIDCName = "SSO"
	// DefaultOIDCScopes are the scopes requested at the IdP.
	DefaultOIDCScopes = "openid profile email"
	// DefaultOIDCTimeout is the deadline of every OIDC network call (AD-10).
	DefaultOIDCTimeout = 10 * time.Second
	// MinOIDCTimeout and MaxOIDCTimeout bound oidc_timeout.
	MinOIDCTimeout = time.Second
	MaxOIDCTimeout = 2 * time.Minute
)

func init() { declareHubOIDC(Default) }

// declareHubOIDC declares the settings of the OIDC provider (AD-10, AD-19).
func declareHubOIDC(r *Registry) {
	r.Declare(Setting{Key: KeyOIDCName, Scope: ScopeHub, Default: DefaultOIDCName,
		Help: `Name of the OIDC provider on the login page ("Sign in with <name>")`})
	r.Declare(Setting{Key: KeyOIDCIssuer, Scope: ScopeHub,
		Help: "Issuer URL of the OIDC provider; setting it turns OIDC login on"})
	r.Declare(Setting{Key: KeyOIDCClientID, Scope: ScopeHub, Help: "OIDC client id"})
	r.Declare(Setting{Key: KeyOIDCClientSecret, Scope: ScopeHub, Secret: true,
		Help: "OIDC client secret (MINION_OIDC_CLIENT_SECRET_FILE names a file with it)"})
	r.Declare(Setting{Key: KeyOIDCRoleClaim, Scope: ScopeHub,
		Help: `ID token claim that carries the roles, for example "groups"`})
	r.Declare(Setting{Key: KeyOIDCAdminValues, Scope: ScopeHub,
		Help: "Comma-separated claim values that grant the admin role"})
	r.Declare(Setting{Key: KeyOIDCUserValues, Scope: ScopeHub,
		Help: "Comma-separated claim values that grant the user role"})
	r.Declare(Setting{Key: KeyOIDCScopes, Scope: ScopeHub, Default: DefaultOIDCScopes,
		Help: "Space-separated scopes requested at the OIDC provider"})
	r.Declare(Setting{Key: KeyOIDCTimeout, Scope: ScopeHub, Default: DefaultOIDCTimeout.String(),
		Help: "Deadline of every call to the OIDC provider as a Go duration such as 10s (1s to 2m)"})
	r.Declare(Setting{Key: KeyOIDCTLSInsecure, Scope: ScopeHub, Default: "false", Choices: []string{"true", "false"},
		Help: "Do not verify the TLS certificate of the OIDC provider; insecure, a permanent warning is shown (prefer a CA file)"})
}

// HubOIDC holds the resolved OIDC settings of the Hub: the provider that the
// environment or the panel settings describe.
type HubOIDC struct {
	Name     string
	Issuer   string
	ClientID string
	// ClientSecret is a secret: callers must never log it.
	ClientSecret string
	RoleClaim    string
	AdminValues  []string
	UserValues   []string
	Scopes       []string
	CAFile       string
	// InsecureSkipVerify turns TLS verification of the IdP off (R96).
	InsecureSkipVerify bool
	Timeout            time.Duration
}

// Configured reports whether an issuer is set, which turns OIDC login on.
func (h HubOIDC) Configured() bool { return h.Issuer != "" }

// ErrOIDCClientIDMissing is wrapped by the error of [ResolveHubOIDC] when an
// issuer is set without a client id. The cross-field rule depends on every
// source, so a caller that checks one source alone can tell it apart.
var ErrOIDCClientIDMissing = errors.New("oidc_client_id is missing")

// ResolveHubOIDC resolves the OIDC settings through r (a Hub-role resolver).
// When an issuer is set, the client id is required, and oidc_timeout must be a
// duration in range; an invalid environment or panel value is an error.
func ResolveHubOIDC(r *Resolver) (HubOIDC, error) {
	if r.Role != ScopeHub {
		return HubOIDC{}, fmt.Errorf("config: ResolveHubOIDC needs a %q resolver, got %q", ScopeHub, r.Role)
	}
	var h HubOIDC
	get := func(key string) (string, error) {
		res, err := r.Resolve(key)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(res.Value), nil
	}
	for _, f := range []struct {
		key string
		dst *string
	}{
		{KeyOIDCName, &h.Name}, {KeyOIDCIssuer, &h.Issuer}, {KeyOIDCClientID, &h.ClientID},
		{KeyOIDCRoleClaim, &h.RoleClaim},
	} {
		v, err := get(f.key)
		if err != nil {
			return HubOIDC{}, err
		}
		*f.dst = v
	}
	secret, err := r.Resolve(KeyOIDCClientSecret)
	if err != nil {
		return HubOIDC{}, err
	}
	h.ClientSecret = secret.Value
	for _, f := range []struct {
		key string
		dst *[]string
	}{
		{KeyOIDCAdminValues, &h.AdminValues}, {KeyOIDCUserValues, &h.UserValues},
	} {
		v, err := get(f.key)
		if err != nil {
			return HubOIDC{}, err
		}
		*f.dst = SplitList(v, ",")
	}
	scopes, err := get(KeyOIDCScopes)
	if err != nil {
		return HubOIDC{}, err
	}
	h.Scopes = strings.Fields(scopes)
	if h.CAFile, err = r.pathSetting(EnvOIDCCAFile, KeyOIDCCAFile); err != nil {
		return HubOIDC{}, err
	}
	insecure, err := get(KeyOIDCTLSInsecure)
	if err != nil {
		return HubOIDC{}, err
	}
	h.InsecureSkipVerify = insecure == "true"
	to, err := get(KeyOIDCTimeout)
	if err != nil {
		return HubOIDC{}, err
	}
	if h.Timeout, err = ParseOIDCTimeout(to); err != nil {
		return HubOIDC{}, fmt.Errorf("setting %q: %w", KeyOIDCTimeout, err)
	}
	if h.Issuer != "" && h.ClientID == "" {
		return HubOIDC{}, fmt.Errorf("setting %q is set, so %q is required too: %w", KeyOIDCIssuer, KeyOIDCClientID, ErrOIDCClientIDMissing)
	}
	return h, nil
}

// ParseOIDCTimeout parses and range-checks an oidc_timeout value.
func ParseOIDCTimeout(v string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 10s", v)
	}
	if d < MinOIDCTimeout || d > MaxOIDCTimeout {
		return 0, fmt.Errorf("%s is outside the allowed range %s to %s", d, MinOIDCTimeout, MaxOIDCTimeout)
	}
	return d, nil
}

// SplitList splits v at sep, trims every element and drops the empty ones.
func SplitList(v, sep string) []string {
	var out []string
	for _, p := range strings.Split(v, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
