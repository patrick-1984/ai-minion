package config

import "time"

// KeyOIDCRefreshInterval names the setting of the OIDC background role refresh
// (AD-10, Story 6.3). Its environment name is MINION_OIDC_REFRESH_INTERVAL.
const KeyOIDCRefreshInterval = "oidc_refresh_interval"

// Defaults and bounds of oidc_refresh_interval. The Hub parses the value with
// [time.ParseDuration] and applies the bounds when the refresher schedules its
// next pass.
const (
	// DefaultOIDCRefreshInterval is how often every OIDC account's IdP roles are
	// re-checked at the identity provider. It is the value of the declared
	// default "24h".
	DefaultOIDCRefreshInterval = 24 * time.Hour
	// MinOIDCRefreshInterval keeps a typo from hammering the identity provider.
	MinOIDCRefreshInterval = time.Minute
	// MaxOIDCRefreshInterval is the longest wait between two passes.
	MaxOIDCRefreshInterval = 30 * 24 * time.Hour
)

func init() { declareHubOIDCRefresh(Default) }

// declareHubOIDCRefresh declares the refresh interval of the OIDC background
// role refresh.
func declareHubOIDCRefresh(r *Registry) {
	r.Declare(Setting{Key: KeyOIDCRefreshInterval, Scope: ScopeHub, Default: "24h",
		Help: "How often the Hub re-checks the roles of OIDC accounts at the identity provider, as a Go duration such as 24h (1m to 720h)"})
}
