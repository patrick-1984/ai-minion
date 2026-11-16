package config

// Hub update settings (v0.9.0, A10): the daily release check and the channel
// the Hub follows.
const (
	// KeyUpdateCheck turns the daily release check on or off.
	KeyUpdateCheck = "update_check"
	// KeyUpdateChannel is the release channel the Hub follows: minor (patches
	// of the running X.Y), major (releases of the running X) or latest.
	KeyUpdateChannel = "update_channel"
	// KeySelfUpdate allows the Hub to replace its own Docker container
	// (auto: when the Docker socket is mounted) or forbids it (off).
	KeySelfUpdate = "self_update"
)

// Values of update_channel.
const (
	UpdateChannelMinor  = "minor"
	UpdateChannelMajor  = "major"
	UpdateChannelLatest = "latest"
)

func init() { declareHubUpdates(Default) }

func declareHubUpdates(r *Registry) {
	r.Declare(Setting{Key: KeyUpdateCheck, Scope: ScopeHub, Default: "on", Choices: []string{"on", "off"},
		Help: "Check once a day for a newer Minion release on GitHub (one unauthenticated API call); off contacts nothing"})
	r.Declare(Setting{Key: KeyUpdateChannel, Scope: ScopeHub, Default: UpdateChannelMinor,
		Choices: []string{UpdateChannelMinor, UpdateChannelMajor, UpdateChannelLatest},
		Help:    "Release channel the Hub follows: minor (patches of the running X.Y), major (releases of the running X) or latest"})
	r.Declare(Setting{Key: KeySelfUpdate, Scope: ScopeHub, Default: "auto", Choices: []string{"auto", "off"},
		Help: "Let an administrator update the Hub from the web UI when it runs in Docker with the socket mounted (auto) or never (off)"})
}
