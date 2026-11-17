package agent

// Visible and hidden sessions (v0.10.0). A session is hidden when the person
// cannot see or join it from their own terminal:
//
//   - a Windows ConPTY session lives inside the Worker process;
//   - a tmux or psmux session on a server that is not the default server of the
//     user the Worker runs as (a private socket, or a Worker that a service
//     manager runs as root or LocalSystem).
//
// Sessions on the default tmux server of the running user (Linux, macOS) or in
// the default psmux namespace (Windows) are visible: `tmux ls` / `psmux ls`
// lists them and the person can attach. The Worker decides it here, where the
// facts are, and reports it in its hello; the Hub's hidden_sessions policy
// refuses a session_create that would make a hidden session.

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/patrick-1984/ai-minion/internal/transport"
)

// SessionHostInfo is what the Worker reports about the host of its sessions.
// The core module provides it (as a pointer) to the connector.
type SessionHostInfo struct {
	// Kind is transport.SessionHostTmux, SessionHostPsmux or SessionHostConPTY.
	Kind string
	// Hidden is true when a new session would be hidden from the person.
	Hidden bool
	// Reason is a short sentence on why: the host, and why it is visible or
	// hidden.
	Reason string
}

// applyTo copies the info into the facts of the hello. A zero info (unknown)
// leaves them alone.
func (i *SessionHostInfo) applyTo(f *transport.WorkerFacts) {
	if i == nil || i.Kind == "" {
		return
	}
	f.SessionHost, f.SessionsHidden, f.SessionHostReason = i.Kind, i.Hidden, i.Reason
}

// tmuxHostFacts are the facts of the hidden decision on Linux and macOS.
type tmuxHostFacts struct {
	// Socket is the tmux socket the Worker uses; DefaultSocket the one a tmux
	// client of the same user uses without -S and -L.
	Socket        string
	DefaultSocket string
	// User is the name of the user the Worker runs as (may be empty); Root is
	// true when that user is root.
	User string
	Root bool
	// ServiceManager is true when systemd or launchd runs the Worker.
	ServiceManager bool
}

// decideTmuxHost decides whether the sessions of a tmux Worker are hidden.
// Another socket than the default one is hidden: a plain `tmux ls` of the
// person does not show it. The default server of root under a service manager
// is hidden too: that Worker is a system service and the person who uses it
// is not signed in as root.
func decideTmuxHost(f tmuxHostFacts) SessionHostInfo {
	info := SessionHostInfo{Kind: transport.SessionHostTmux}
	who := "the user the Worker runs as"
	if f.User != "" {
		who = f.User
	}
	switch {
	case f.Socket == "":
		info.Hidden = true
		info.Reason = "the tmux socket of the Worker is not known"
	case f.DefaultSocket == "" || filepath.Clean(f.Socket) != filepath.Clean(f.DefaultSocket):
		info.Hidden = true
		info.Reason = "private tmux socket " + f.Socket + ", not the default tmux server of " + who + " (tmux -S " + f.Socket + " ls shows them)"
	case f.Root && f.ServiceManager:
		info.Hidden = true
		info.Reason = "the Worker runs as root under a service manager (systemd or launchd); its sessions are in the tmux server of root"
	default:
		info.Reason = "the default tmux server of " + who + " (tmux ls shows them)"
		if f.ServiceManager {
			info.Reason += "; the Worker runs under a service manager"
		}
	}
	return info
}

// windowsHostFacts are the facts of the hidden decision on Windows.
type windowsHostFacts struct {
	// Psmux is true when psmux hosts the sessions; ConPTY otherwise.
	Psmux bool
	// User is the name of the user the Worker runs as (may be empty).
	User string
	// ConPTYReason is why ConPTY hosts the sessions ("psmux is not installed
	// (winget install psmux)", "the Worker runs as a Windows service ...").
	ConPTYReason string
}

// decideWindowsHost decides whether the sessions of a Windows Worker are
// hidden: psmux serves the default namespace of the user the Worker runs as
// (sessionHostSpec uses it only then), so its sessions are visible; ConPTY
// sessions live inside the Worker process and are always hidden.
func decideWindowsHost(f windowsHostFacts) SessionHostInfo {
	if f.Psmux {
		who := "the user the Worker runs as"
		if f.User != "" {
			who = f.User
		}
		return SessionHostInfo{Kind: transport.SessionHostPsmux,
			Reason: "the default psmux namespace of " + who + " (psmux ls shows them)"}
	}
	reason := "ConPTY inside the Worker"
	if f.ConPTYReason != "" {
		reason += ": " + f.ConPTYReason
	}
	return SessionHostInfo{Kind: transport.SessionHostConPTY, Hidden: true, Reason: reason}
}

// maxListErrorText bounds the listing error the Worker reports, in bytes.
const maxListErrorText = 120

// SessionListHealth is the outcome of the Worker's latest look at its
// sessions (the startup probe and every poll of the event publisher). The
// Hub shows it when a Worker lists no session, so "0 sessions" can be told
// apart from "the listing failed". The core module provides it.
type SessionListHealth struct {
	text atomic.Pointer[string]
}

// Set records the outcome of one look: nil clears the error.
func (h *SessionListHealth) Set(err error) {
	if h == nil {
		return
	}
	if err == nil {
		h.text.Store(nil)
		return
	}
	t := listErrorText(err)
	h.text.Store(&t)
}

// Text is the short, path-free reason of the latest failed look; empty when
// the latest look succeeded or none was made.
func (h *SessionListHealth) Text() string {
	if h == nil {
		return ""
	}
	if p := h.text.Load(); p != nil {
		return *p
	}
	return ""
}

// listErrorText turns a listing error into a short sentence for the Hub: the
// first line, with every word that looks like a file path replaced by
// "<path>" (a path names the user and the machine), bounded.
func listErrorText(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "listing the sessions timed out"
	case errors.Is(err, exec.ErrNotFound):
		return "the session host program was not found"
	}
	line, _, _ := strings.Cut(err.Error(), "\n")
	words := strings.Fields(line)
	for i, w := range words {
		trimmed := strings.Trim(w, `"'():,;`)
		if strings.ContainsAny(trimmed, `/\`) {
			words[i] = strings.Replace(w, trimmed, "<path>", 1)
		}
	}
	out := strings.Join(words, " ")
	if len(out) > maxListErrorText {
		cut := maxListErrorText
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + "..."
	}
	if out == "" {
		out = "listing the sessions failed"
	}
	return out
}
