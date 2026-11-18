package ops

import "reflect"

// Names of the Hub self-update operations (v0.9.0, A10).
const (
	// OpHubUpdateCheck reports what the Hub can update to and whether it can
	// do it itself (Docker opt-in), or why not.
	OpHubUpdateCheck = "hub_update_check"
	// OpHubUpdate replaces the Hub container with the newest release in the
	// update channel. The Hub restarts moments after the answer.
	OpHubUpdate = "hub_update"
)

// HubUpdateCheckArgs are the arguments of hub_update_check.
type HubUpdateCheckArgs struct {
	// Refresh asks GitHub now instead of reporting the last daily answer
	// (at most once a minute; a sooner call reports the last answer).
	Refresh bool `json:"refresh,omitempty"`
}

// HubUpdateCheckResult describes the Hub's own update.
type HubUpdateCheckResult struct {
	Current string `json:"current"`
	Channel string `json:"channel"`
	// Latest is the newest release in the channel the daily check found;
	// empty before the first check or with update_check off.
	Latest     string `json:"latest,omitempty"`
	Newer      bool   `json:"newer"`
	ReleaseURL string `json:"release_url,omitempty"`
	// CheckedAt is when the release check last ran (RFC 3339, UTC), empty
	// before the first one; CheckedChannel is the channel it ran on, which
	// differs from Channel for a moment after the setting changed.
	CheckedAt      string `json:"checked_at,omitempty"`
	CheckedChannel string `json:"checked_channel,omitempty"`
	// CheckError is the error of the last check; the last good answer stays.
	CheckError string `json:"check_error,omitempty"`
	// Refreshed is true when this call asked GitHub (refresh, and the last
	// check was not less than a minute ago).
	Refreshed bool `json:"refreshed,omitempty"`
	// SelfUpdate is true when the Hub can replace itself (Docker, opt-in);
	// Reason says why not otherwise, and Command is what to run instead.
	SelfUpdate bool   `json:"self_update"`
	Reason     string `json:"reason,omitempty"`
	Command    string `json:"command,omitempty"`
}

// HubUpdateArgs are the arguments of hub_update.
type HubUpdateArgs struct {
	// Force updates although connected Workers speak a protocol the new
	// release does not (they then need an update by hand).
	Force bool `json:"force,omitempty"`
}

// HubUpdateResult says whether the update started.
type HubUpdateResult struct {
	Started bool   `json:"started"`
	From    string `json:"from"`
	To      string `json:"to,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Stranded lists the connected Workers whose protocol the new release
	// does not support (the update is refused unless force).
	Stranded []string `json:"stranded,omitempty"`
}

func init() {
	manage := MustActionSet(ActionManage)
	Register(Op{
		Name:    OpHubUpdateCheck,
		Summary: "Show the Hub's release, the newest one in its update channel (refresh: ask GitHub now, at most once a minute), and whether the Hub can update itself (admin only).",
		Scope:   ScopeHub, Actions: manage,
		Args: reflect.TypeFor[HubUpdateCheckArgs](), Result: reflect.TypeFor[HubUpdateCheckResult](),
	})
	// hub_update is Mutates: its result is not a WriteResult, and a repeat is
	// refused while an update runs.
	Register(Op{
		Name:    OpHubUpdate,
		Summary: "Replace the Hub container with the newest signed release in its update channel (Docker, opt-in; admin only).",
		Scope:   ScopeHub, Actions: manage, Mutates: true,
		Args: reflect.TypeFor[HubUpdateArgs](), Result: reflect.TypeFor[HubUpdateResult](),
	})
}
