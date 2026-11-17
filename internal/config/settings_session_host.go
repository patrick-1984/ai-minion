package config

// Names of the settings of the Windows session host (v0.9.2, A21, A23).
const (
	// KeySessionHost chooses how a Windows Worker hosts sessions: psmux (the
	// user's psmux sessions, which outlive the Worker), conpty (sessions inside
	// the Worker process) or auto (psmux when it is installed and the Worker
	// runs as the user, else conpty). Other platforms always use tmux.
	KeySessionHost = "session_host"
	// KeyPsmuxOffer records the answer to "install psmux with winget?" so the
	// first-run question is asked once: ask, declined or installed.
	KeyPsmuxOffer = "psmux_offer"
	// KeySessionsVisible says whether the tmux sessions of a Linux or macOS
	// Worker are visible to the person (v0.10.0): yes when the Worker serves
	// the person's own tmux server (a container that mounts the host user's
	// tmux socket), no when the tmux server is the Worker's own (a tmux
	// inside a container), auto to decide from the socket, the user and the
	// service manager.
	KeySessionsVisible = "sessions_visible"
)

// Values of sessions_visible.
const (
	SessionsVisibleAuto = "auto"
	SessionsVisibleYes  = "yes"
	SessionsVisibleNo   = "no"
)

// Values of session_host.
const (
	SessionHostAuto   = "auto"
	SessionHostPsmux  = "psmux"
	SessionHostConPTY = "conpty"
)

func init() { declareSessionHost(Default) }

func declareSessionHost(r *Registry) {
	r.Declare(Setting{
		Key:     KeySessionHost,
		Scope:   ScopeWorker,
		Help:    "Windows: host sessions with psmux (your psmux sessions, kept across Worker restarts), conpty (inside the Worker) or auto (psmux when installed and the Worker runs as you)",
		Default: SessionHostAuto,
		Choices: []string{SessionHostAuto, SessionHostPsmux, SessionHostConPTY},
	})
	r.Declare(Setting{
		Key:     KeyPsmuxOffer,
		Scope:   ScopeWorker,
		Help:    "Windows: whether the first run still offers to install psmux with winget (ask, declined, installed)",
		Default: "ask",
		Choices: []string{"ask", "declined", "installed"},
	})
	r.Declare(Setting{
		Key:     KeySessionsVisible,
		Scope:   ScopeWorker,
		Help:    "Linux and macOS: whether new tmux sessions are visible to you: yes (the Worker serves your own tmux server, e.g. your tmux socket mounted into its container), no (its own tmux server, e.g. inside a container) or auto (decide from the socket)",
		Default: SessionsVisibleAuto,
		Choices: []string{SessionsVisibleAuto, SessionsVisibleYes, SessionsVisibleNo},
	})
}
