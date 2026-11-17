package config

// KeyHiddenSessions is the Hub-wide policy on hidden sessions (v0.10.0): new
// sessions that the person could not see or join from their own terminal
// (Windows ConPTY sessions inside the Worker, or a tmux/psmux server that is
// not the person's default one). Its environment name is
// MINION_HIDDEN_SESSIONS. The Hub resolves it for every session_create, so a
// value stored in the panel takes effect at once.
const KeyHiddenSessions = "hidden_sessions"

// Values of hidden_sessions.
const (
	// HiddenSessionsUserControlled lets every user decide in their own
	// account settings; a user who never chose is refused (the default).
	HiddenSessionsUserControlled = "user_controlled"
	// HiddenSessionsAllowed allows hidden sessions for everybody.
	HiddenSessionsAllowed = "allowed"
	// HiddenSessionsDisabled refuses hidden sessions for everybody.
	HiddenSessionsDisabled = "disabled"
)

func init() { declareHubSessions(Default) }

func declareHubSessions(r *Registry) {
	r.Declare(Setting{Key: KeyHiddenSessions, Scope: ScopeHub, Default: HiddenSessionsUserControlled,
		Choices: []string{HiddenSessionsUserControlled, HiddenSessionsAllowed, HiddenSessionsDisabled},
		Help: "New sessions that you could not see or join from your own terminal (Windows ConPTY inside the Worker, " +
			"or a tmux server that is not your default one): user_controlled (each user allows them in Account > Sessions; " +
			"off until they do), allowed (for everybody) or disabled (refused for everybody)"})
}
