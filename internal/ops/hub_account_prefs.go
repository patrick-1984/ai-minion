package ops

import "reflect"

// Names of the per-user preference operations (v0.10.0). Each user reads and
// changes their own preferences; there is no user id.
const (
	OpAccountPrefs    = "account_prefs"
	OpAccountPrefsSet = "account_prefs_set"
)

// AccountPrefsArgs are the arguments of account_prefs (none).
type AccountPrefsArgs struct{}

// AccountPrefsResult are the caller's own preferences and what they mean
// under the Hub's settings.
type AccountPrefsResult struct {
	// AllowHiddenSessions is the caller's choice: create sessions that they
	// could not see or join from their own terminal (Windows ConPTY inside the
	// Worker, a tmux server that is not their default one). Off by default.
	AllowHiddenSessions bool `json:"allow_hidden_sessions"`
	// HiddenSessions is the Hub setting hidden_sessions: user_controlled (the
	// choice above decides), allowed or disabled (the choice is ignored).
	HiddenSessions string `json:"hidden_sessions"`
	// HiddenSessionsAllowed is the effective answer for the caller.
	HiddenSessionsAllowed bool `json:"hidden_sessions_allowed"`
}

// AccountPrefsSetArgs are the arguments of account_prefs_set. A preference
// that is not given stays as it is.
type AccountPrefsSetArgs struct {
	WriteArgs
	// AllowHiddenSessions allows (true) or refuses (false) new hidden sessions
	// for the caller while the Hub setting hidden_sessions is user_controlled.
	AllowHiddenSessions *bool `json:"allow_hidden_sessions,omitempty"`
}

// Validate checks the request id and that something is changed.
func (a AccountPrefsSetArgs) Validate() error {
	if err := a.WriteArgs.Validate(); err != nil {
		return err
	}
	if a.AllowHiddenSessions == nil {
		return invalid("allow_hidden_sessions", "is required")
	}
	return nil
}

// guardUserExists refuses a change for an account that no longer exists. It
// is KEEP: force cannot create preferences for a removed account.
var guardUserExists = Guard{Name: "user_exists", Class: GuardKeep, Reads: "users.id"}

func init() {
	read := MustActionSet(ActionRead)
	// The owner of an account holds operate on it, so every user may change
	// their own preferences.
	own := MustActionSet(ActionOperate)
	Register(Op{
		Name: OpAccountPrefs, Summary: "Show your own preferences, such as whether you allow new hidden sessions, and the Hub policy they depend on.",
		Scope: ScopeHub, Actions: read,
		Args: reflect.TypeFor[AccountPrefsArgs](), Result: reflect.TypeFor[AccountPrefsResult](),
	})
	Register(Op{
		Name: OpAccountPrefsSet, Summary: "Change your own preferences, such as allowing new sessions that would be hidden from your terminal.",
		Scope: ScopeHub, Actions: own, Write: true,
		Guards: []Guard{guardUserExists},
		Args:   reflect.TypeFor[AccountPrefsSetArgs](), Result: reflect.TypeFor[WriteResult](),
	})
}
