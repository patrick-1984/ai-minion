package transport

// Reason codes of the session host and of a failed session listing (v0.10.0).
// The hello carries a code and its fixed English text, never a path, a user
// name or the output of a program: the Worker sends its hello before it has
// checked the Hub's identity. The raw details stay in the Worker's own log
// and local status.

// Codes of [WorkerFacts.SessionHostReasonCode].
const (
	HostReasonTmuxDefault         = "tmux_default"
	HostReasonTmuxDefaultService  = "tmux_default_service"
	HostReasonSocketUnknown       = "socket_unknown"
	HostReasonPrivateSocket       = "private_socket"
	HostReasonRootService         = "root_service"
	HostReasonContainerTmux       = "container_tmux"
	HostReasonVisibleSetting      = "visible_setting"
	HostReasonContainerHostSocket = "container_host_socket"
	HostReasonHiddenSetting       = "hidden_setting"
	HostReasonPsmuxDefault        = "psmux_default"
	HostReasonConPTYNoPsmux       = "conpty_no_psmux"
	HostReasonConPTYService       = "conpty_service"
	HostReasonConPTYSetting       = "conpty_setting"
	HostReasonConPTYPsmuxFailed   = "conpty_psmux_failed"
)

// Codes of [WorkerFacts.SessionsListErrorCode].
const (
	ListFailedTimeout  = "list_failed_timeout"
	ListFailedNotFound = "list_failed_not_found"
	ListFailedExec     = "list_failed_exec"
	ListFailedFormat   = "list_failed_format"
)

var hostReasonTexts = map[string]string{
	HostReasonTmuxDefault:         "the default tmux server of the user the Worker runs as (tmux ls shows them)",
	HostReasonTmuxDefaultService:  "the default tmux server of the user the Worker runs as, as seen by the service (tmux ls shows them unless your shell sets another TMUX_TMPDIR or socket; then set sessions_visible); the Worker runs under a service manager",
	HostReasonSocketUnknown:       "the tmux socket of the Worker is not known",
	HostReasonPrivateSocket:       "a private tmux socket, not the default tmux server of the user the Worker runs as (tmux -S <socket> ls shows them)",
	HostReasonRootService:         "the Worker runs as root under a service manager (systemd or launchd); its sessions are in the tmux server of root",
	HostReasonContainerTmux:       "the tmux server inside the Minion image, not one on the host",
	HostReasonVisibleSetting:      "sessions_visible is yes: the Worker serves the person's own tmux server (for example the host user's tmux socket mounted into its container)",
	HostReasonContainerHostSocket: "the host user's tmux socket mounted into the Worker's container at /tmux/default (the documented Docker setup; tmux ls on the host shows them)",
	HostReasonHiddenSetting:       "sessions_visible is no: the Worker's tmux server is not the person's own (for example a tmux inside its container)",
	HostReasonPsmuxDefault:        "the default psmux namespace of the user the Worker runs as (psmux ls shows them)",
	HostReasonConPTYNoPsmux:       "ConPTY inside the Worker: psmux is not installed (winget install psmux)",
	HostReasonConPTYService:       "ConPTY inside the Worker: the Worker runs as a Windows service (LocalSystem); psmux sessions are served by a Worker that runs as you (minion worker up --foreground)",
	HostReasonConPTYSetting:       "ConPTY inside the Worker: session_host is set to conpty",
	HostReasonConPTYPsmuxFailed:   "ConPTY inside the Worker: psmux is installed but cannot be used (the Worker log says why)",
}

var listErrorTexts = map[string]string{
	ListFailedTimeout:  "listing the sessions timed out",
	ListFailedNotFound: "the session host program was not found",
	ListFailedExec:     "the session host program failed (the Worker log and minion worker status say why)",
	ListFailedFormat:   "the session host program answered in an unexpected format",
}

// HostReasonText is the fixed text of a session host reason code; "" for an
// unknown code.
func HostReasonText(code string) string { return hostReasonTexts[code] }

// ListErrorText is the fixed text of a listing error code; "" for an unknown
// code.
func ListErrorText(code string) string { return listErrorTexts[code] }
