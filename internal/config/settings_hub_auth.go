package config

import (
	"fmt"
	"strings"
	"time"
)

// Names of the Hub authentication settings, for callers that look them up by
// key.
const (
	KeyAdminUsername = "admin_username"
	KeyAdminPassword = "admin_password"
	KeySessionTTL    = "session_ttl"
)

// Defaults and bounds of the Hub authentication settings.
const (
	// DefaultAdminUsername is the user name of the seeded admin when
	// MINION_ADMIN_USERNAME is not set.
	DefaultAdminUsername = "admin"
	// DefaultSessionTTL is how long a browser session lives.
	DefaultSessionTTL = 24 * time.Hour
	// MinSessionTTL and MaxSessionTTL bound session_ttl.
	MinSessionTTL = 5 * time.Minute
	MaxSessionTTL = 30 * 24 * time.Hour
)

func init() { declareHubAuth(Default) }

// declareHubAuth declares the settings of local accounts and browser
// sessions (AD-10).
func declareHubAuth(r *Registry) {
	r.Declare(Setting{
		Key:     KeyAdminUsername,
		Scope:   ScopeHub,
		Help:    "User name of the local admin created on the first start (only used while the user table is empty)",
		Default: DefaultAdminUsername,
	})
	r.Declare(Setting{
		Key:   KeyAdminPassword,
		Scope: ScopeHub,
		Help:  "Password of the local admin created on the first start; when unset a random password is generated, logged once and must be changed at first login",
		// The one named exception to "no secret in a plain variable": the
		// one-time seed (AD-10, AD-19). MINION_ADMIN_PASSWORD_FILE works too.
		Secret:          true,
		PlainEnvAllowed: true,
	})
	r.Declare(Setting{
		Key:     KeySessionTTL,
		Scope:   ScopeHub,
		Help:    "Lifetime of a browser session as a Go duration such as 24h (5m to 720h)",
		Default: DefaultSessionTTL.String(),
	})
}

// HubAuth holds the resolved authentication settings of the Hub.
type HubAuth struct {
	// AdminUsername is the seed admin's user name.
	AdminUsername string
	// AdminPassword is the seed password, "" when none is configured. It is a
	// secret: callers must never log it.
	AdminPassword string
	// SessionTTL is the lifetime of a browser session. It is
	// [DefaultSessionTTL] when SessionTTLFallback is set.
	SessionTTL time.Duration
	// SessionTTLFallback names the source of an invalid session_ttl value
	// that was replaced by the default; empty when the value was valid. Only a
	// panel (or default) value is replaced: an invalid environment value is an
	// error.
	SessionTTLFallback Source
}

// ResolvedSessionTTL is the result of [ResolveSessionTTL].
type ResolvedSessionTTL struct {
	// Value is the lifetime to use: the configured one, or [DefaultSessionTTL]
	// when Fallback is set.
	Value time.Duration
	// Source is where the configured value came from.
	Source Source
	// Fallback is true when the configured value is invalid and Value is the
	// default instead.
	Fallback bool
}

// ResolveSessionTTL resolves session_ttl. A value from the environment that is
// not a valid duration in range is an error, so that an operator's mistake
// stops the start. A value from the panel is only checked for the choices of
// the setting, so a bad one can be stored; it must never lock everybody out,
// so it resolves to [DefaultSessionTTL] with Fallback set and no error. The
// caller logs that (naming the key and the source, not the value).
func ResolveSessionTTL(r *Resolver) (ResolvedSessionTTL, error) {
	res, err := r.Resolve(KeySessionTTL)
	if err != nil {
		return ResolvedSessionTTL{}, err
	}
	d, perr := ParseSessionTTL(res.Value)
	switch {
	case perr == nil:
		return ResolvedSessionTTL{Value: d, Source: res.Source}, nil
	case res.Source == SourcePanel:
		return ResolvedSessionTTL{Value: DefaultSessionTTL, Source: res.Source, Fallback: true}, nil
	default:
		return ResolvedSessionTTL{}, fmt.Errorf("setting %q (%s): %w", KeySessionTTL, res.Source, perr)
	}
}

// ResolveHubAuth resolves the authentication settings through r (a Hub-role
// resolver). The password is read from MINION_ADMIN_PASSWORD_FILE, the plain
// MINION_ADMIN_PASSWORD (the one-time seed exception), the panel, in that
// order of precedence of the resolver.
func ResolveHubAuth(r *Resolver) (HubAuth, error) {
	if r.Role != ScopeHub {
		return HubAuth{}, fmt.Errorf("config: ResolveHubAuth needs a %q resolver, got %q", ScopeHub, r.Role)
	}
	var ha HubAuth
	user, err := r.Resolve(KeyAdminUsername)
	if err != nil {
		return HubAuth{}, err
	}
	ha.AdminUsername = strings.TrimSpace(user.Value)
	pw, err := r.Resolve(KeyAdminPassword)
	if err != nil {
		return HubAuth{}, err
	}
	ha.AdminPassword = pw.Value
	ttl, err := ResolveSessionTTL(r)
	if err != nil {
		return HubAuth{}, err
	}
	ha.SessionTTL = ttl.Value
	if ttl.Fallback {
		ha.SessionTTLFallback = ttl.Source
	}
	return ha, nil
}

// ParseSessionTTL parses and range-checks a session_ttl value.
func ParseSessionTTL(v string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 24h", v)
	}
	if d < MinSessionTTL || d > MaxSessionTTL {
		return 0, fmt.Errorf("%s is outside the allowed range %s to %s", d, MinSessionTTL, MaxSessionTTL)
	}
	return d, nil
}
