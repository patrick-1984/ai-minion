package ops

import (
	"strings"
	"unicode/utf8"
)

// Limits of target components. tmux itself rewrites ":" and "." in session
// names, so neither can occur in a name that was resolved.
const (
	maxSessionNameLen = 128
	maxWorkerNameLen  = 64
)

// Target addresses one session of one Worker. The identity is the session
// name, not the pane: a recreated session with the same name is the same
// target (R88). Each call resolves it to exactly one pane: the active pane, or
// the explicit Pane.
type Target struct {
	// WorkerID is the Worker's UUIDv7. Hub-routed calls carry it; a call on
	// the local socket of a Worker (Solo) may leave it empty.
	WorkerID    string `json:"worker_id"`
	SessionName string `json:"session_name"`
	// Pane is an optional "window.pane" index pair such as "0.1"; empty
	// means the active pane.
	Pane string `json:"pane,omitempty"`
}

// Validate checks the session name, the optional pane and the optional worker
// id. Every error matches ErrInvalidArgument.
func (t Target) Validate() error {
	if t.WorkerID != "" && !ValidUUIDv7(t.WorkerID) {
		return invalid("worker_id", "must be a UUIDv7")
	}
	if !validSessionName(t.SessionName) {
		return invalid("session_name", "invalid session name")
	}
	if t.Pane != "" && !validPane(t.Pane) {
		return invalid("pane", "must have the form window.pane, for example 0.1")
	}
	return nil
}

// String returns a log-safe form "worker_id:session[:pane]". It never contains
// terminal content.
func (t Target) String() string {
	s := t.WorkerID + ":" + t.SessionName
	if t.Pane != "" {
		s += ":" + t.Pane
	}
	return s
}

// HumanTarget is the parsed human form of a target, where the Worker is named
// by its name instead of its id. The Hub resolves WorkerName inside the
// caller's space (or the act-as space) to a Target (AD-18).
type HumanTarget struct {
	WorkerName  string
	SessionName string
	Pane        string // "window.pane" or empty
}

// ParseHumanTarget parses "worker-name:session-name" or
// "worker-name:session-name:window.pane", for example "pi-1:minion-3" or
// "pi-1:minion-3:0.1". Any other form returns an error that matches
// ErrInvalidArgument.
func ParseHumanTarget(s string) (HumanTarget, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return HumanTarget{}, invalid("target", "expected worker-name:session-name[:window.pane]")
	}
	h := HumanTarget{WorkerName: parts[0], SessionName: parts[1]}
	if len(parts) == 3 {
		h.Pane = parts[2]
		if !validPane(h.Pane) {
			return HumanTarget{}, invalid("target", "pane must have the form window.pane, for example 0.1")
		}
	}
	if !validWorkerName(h.WorkerName) {
		return HumanTarget{}, invalid("target", "invalid worker name")
	}
	if !validSessionName(h.SessionName) {
		return HumanTarget{}, invalid("target", "invalid session name")
	}
	return h, nil
}

// String returns the human form that ParseHumanTarget accepts.
func (h HumanTarget) String() string {
	s := h.WorkerName + ":" + h.SessionName
	if h.Pane != "" {
		s += ":" + h.Pane
	}
	return s
}

// Resolve combines the parsed name with the worker id the Hub looked up.
func (h HumanTarget) Resolve(workerID string) Target {
	return Target{WorkerID: workerID, SessionName: h.SessionName, Pane: h.Pane}
}

// validSessionName accepts adopted tmux names: non-empty valid UTF-8, no
// control characters, no ":" or ".", no leading or trailing space.
func validSessionName(s string) bool {
	return validName(s, maxSessionNameLen, ":.", true)
}

// validWorkerName accepts non-empty names without ":", whitespace or control
// characters.
func validWorkerName(s string) bool {
	return validName(s, maxWorkerNameLen, ":", false)
}

func validName(s string, maxLen int, forbidden string, allowInnerSpace bool) bool {
	if s == "" || len(s) > maxLen || !utf8.ValidString(s) {
		return false
	}
	if strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if isControl(r) || strings.ContainsRune(forbidden, r) {
			return false
		}
		if !allowInnerSpace && (r == ' ' || r == 0x85 || r == 0xa0 || r == 0x2028 || r == 0x2029) {
			return false
		}
	}
	return true
}

// validPane reports whether s is "window.pane" with two decimal indexes.
func validPane(s string) bool {
	win, pane, ok := strings.Cut(s, ".")
	return ok && decimal(win) && decimal(pane)
}

func decimal(s string) bool {
	if s == "" || len(s) > 6 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
