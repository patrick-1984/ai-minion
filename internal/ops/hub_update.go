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
	// OpHubUpdateRefresh asks GitHub now for the newest release in the
	// update channel (at most once a minute) and reports like
	// hub_update_check.
	OpHubUpdateRefresh = "hub_update_refresh"
	// OpHubUpdatePolicy arms the owner-installed host updater from an admin browser.
	OpHubUpdatePolicy = "hub_update_policy"
)

// HubUpdateCheckArgs are the arguments of hub_update_check (none).
type HubUpdateCheckArgs struct{}

// HubUpdateRefreshArgs are the arguments of hub_update_refresh (none).
type HubUpdateRefreshArgs struct{}

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
	// Refreshed is true when hub_update_refresh started a check (or one was
	// already running): not within a minute of the last call to GitHub, nor
	// with update_check off.
	Refreshed bool `json:"refreshed,omitempty"`
	// Checking is true while a check started by hub_update_refresh still runs
	// (it answers at once; the result shows up here when it is done).
	Checking bool `json:"checking,omitempty"`
	// SelfUpdate is true when the Hub can replace itself (Docker, opt-in);
	// Reason says why not otherwise, and Command is what to run instead.
	SelfUpdate  bool                  `json:"self_update"`
	Reason      string                `json:"reason,omitempty"`
	Command     string                `json:"command,omitempty"`
	HostUpdater *HubHostUpdaterStatus `json:"host_updater,omitempty"`
}

// HubHostUpdaterStatus is the sanitized state of the owner-installed executor.
type HubHostUpdaterStatus struct {
	App            string `json:"app,omitempty"`
	Available      bool   `json:"available"`
	Stale          bool   `json:"stale"`
	AllowHubPolicy bool   `json:"allow_hub_policy"`
	Enabled        bool   `json:"enabled"`
	Window         string `json:"window,omitempty"`
	Channel        string `json:"channel,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
	Tick           string `json:"tick,omitempty"`
	Detail         string `json:"detail,omitempty"`
	Attempt        string `json:"attempt,omitempty"`
	Outcome        string `json:"outcome,omitempty"`
	RequestError   string `json:"request_error,omitempty"`
	PendingRequest string `json:"pending_request,omitempty"`
	PolicyRevision int64  `json:"policy_revision"`
}

// HubUpdatePolicyArgs writes only policy, never host paths or credentials.
type HubUpdatePolicyArgs struct {
	Enabled  bool   `json:"enabled"`
	Window   string `json:"window"`
	Channel  string `json:"channel"`
	Revision int64  `json:"revision"`
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
	// Queued means the host executor will pick up an expiring request; no
	// restart has happened yet.
	Queued bool `json:"queued,omitempty"`
}

func init() {
	manage := MustActionSet(ActionManage)
	Register(Op{
		Name:    OpHubUpdateCheck,
		Summary: "Show the Hub's release, the newest one in its update channel, and whether the Hub can update itself (admin only).",
		Scope:   ScopeHub, Actions: manage,
		Args: reflect.TypeFor[HubUpdateCheckArgs](), Result: reflect.TypeFor[HubUpdateCheckResult](),
	})
	// hub_update_refresh is Mutates (POST only, CSRF-checked in the browser):
	// it spends the shared GitHub quota and replaces the cached answer.
	Register(Op{
		Name:    OpHubUpdateRefresh,
		Summary: "Ask GitHub now for the newest release in the Hub's update channel (at most once a minute) and report like hub_update_check (admin only).",
		Scope:   ScopeHub, Actions: manage, Mutates: true,
		Args: reflect.TypeFor[HubUpdateRefreshArgs](), Result: reflect.TypeFor[HubUpdateCheckResult](),
	})
	// hub_update is Mutates: its result is not a WriteResult, and a repeat is
	// refused while an update runs.
	Register(Op{
		Name: OpHubUpdatePolicy, Summary: "Set the owner-installed Hub host updater policy (administrator browser session only).",
		Scope: ScopeHub, Actions: manage, Mutates: true,
		Args: reflect.TypeFor[HubUpdatePolicyArgs](), Result: reflect.TypeFor[HubUpdateCheckResult](),
	})
	Register(Op{
		Name:    OpHubUpdate,
		Summary: "Replace the Hub container with the newest signed release in its update channel (Docker, opt-in; admin only).",
		Scope:   ScopeHub, Actions: manage, Mutates: true,
		Args: reflect.TypeFor[HubUpdateArgs](), Result: reflect.TypeFor[HubUpdateResult](),
	})
}
