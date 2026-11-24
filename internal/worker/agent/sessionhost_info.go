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

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/transport"
)

// SessionHostInfo is what the Worker reports about the host of its sessions.
// The core module provides it (as a pointer) to the connector.
type SessionHostInfo struct {
	// Kind is transport.SessionHostTmux, SessionHostPsmux or SessionHostConPTY.
	Kind string
	// Hidden is true when a new session would be hidden from the person.
	Hidden bool
	// Code is why (transport.HostReason*) and Reason its fixed text: the only
	// reason the hello carries.
	Code   string
	Reason string
	// Detail is what only the Worker's own log and local status show: a
	// socket path, a user name, the error of psmux. Never sent to the Hub.
	Detail string
	// The effective tmux target is exposed only on the local control socket.
	tmuxSocket string
	tmuxBinary string
}

// newHostInfo is the info of a host with a reason code and a local detail.
func newHostInfo(kind string, hidden bool, code, detail string) SessionHostInfo {
	return SessionHostInfo{Kind: kind, Hidden: hidden, Code: code, Reason: transport.HostReasonText(code), Detail: detail}
}

// applyTo copies the info into the facts of the hello: the host, whether
// its sessions are hidden and the reason code with its fixed text, never the
// detail. A zero info (unknown) leaves them alone.
func (i *SessionHostInfo) applyTo(f *transport.WorkerFacts) {
	if i == nil || i.Kind == "" {
		return
	}
	f.SessionHost, f.SessionsHidden = i.Kind, i.Hidden
	f.SessionHostReasonCode, f.SessionHostReason = i.Code, i.Reason
}

// localReason is the reason with its detail, for the Worker's own log and
// local status.
func (i SessionHostInfo) localReason() string {
	if i.Detail == "" {
		return i.Reason
	}
	if i.Reason == "" {
		return i.Detail
	}
	return i.Reason + " (" + i.Detail + ")"
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
	// Container is true when the Worker runs in a container: its default
	// tmux server is the container's own, not one of the host.
	Container bool
	// MinionImage is true when the Worker runs from the Minion image
	// (runsFromMinionImage). Only then is the default tmux server the
	// image's own: a binary installed by hand in any other container (a Coder
	// workspace, a dev container) runs where the person works, so its default
	// tmux server is theirs, as on a host (v0.10.0).
	MinionImage bool
	// Visible is the setting sessions_visible: auto (decide from the facts
	// above), yes or no (the operator says).
	Visible string
}

// containerHostTmuxSocket is where the documented Docker setup
// (deploy/compose/worker.yaml, docs/install/worker-docker.md) mounts the host
// user's tmux socket inside the Worker's container.
const containerHostTmuxSocket = "/tmux/default"

// decideTmuxHost decides whether the sessions of a tmux Worker are hidden.
// The setting sessions_visible yes or no decides by itself (a container that
// mounts the host user's tmux socket is visible although the socket path is
// not the container's default). auto: in a container, the documented host
// socket mount (/tmux/default) is visible; another socket than the default one is
// hidden, since a plain `tmux ls` of the person does not show it; the default
// server of the Minion image is the image's own, hidden from the host (a
// binary installed by hand in another container serves the person's tmux); the
// default server of root under a service manager is hidden too: that Worker
// is a system service and the person who uses it is not signed in as root.
func decideTmuxHost(f tmuxHostFacts) SessionHostInfo {
	tm := transport.SessionHostTmux
	socket := "socket " + f.Socket
	switch f.Visible {
	case config.SessionsVisibleYes:
		return newHostInfo(tm, false, transport.HostReasonVisibleSetting, socket)
	case config.SessionsVisibleNo:
		return newHostInfo(tm, true, transport.HostReasonHiddenSetting, socket)
	}
	who := "user " + f.User
	if f.User == "" {
		who = "user unknown"
	}
	switch {
	case f.Socket == "":
		return newHostInfo(tm, true, transport.HostReasonSocketUnknown, "")
	case f.Container && filepath.Clean(f.Socket) == containerHostTmuxSocket:
		// The documented Docker setup (deploy/compose/worker.yaml) mounts the
		// host user's tmux socket directory at /tmux: the v0.9.3 Compose file
		// did so without sessions_visible, and its sessions are the host
		// user's own.
		return newHostInfo(tm, false, transport.HostReasonContainerHostSocket, socket)
	case f.DefaultSocket == "" || filepath.Clean(f.Socket) != filepath.Clean(f.DefaultSocket):
		return newHostInfo(tm, true, transport.HostReasonPrivateSocket, socket+", "+who)
	case f.MinionImage:
		return newHostInfo(tm, true, transport.HostReasonContainerTmux, socket)
	case f.Root && f.ServiceManager:
		return newHostInfo(tm, true, transport.HostReasonRootService, socket)
	case f.ServiceManager:
		return newHostInfo(tm, false, transport.HostReasonTmuxDefaultService, socket+", "+who)
	}
	return newHostInfo(tm, false, transport.HostReasonTmuxDefault, socket+", "+who)
}

// windowsHostFacts are the facts of the hidden decision on Windows.
type windowsHostFacts struct {
	// Psmux is true when psmux hosts the sessions; ConPTY otherwise.
	Psmux bool
	// User is the name of the user the Worker runs as (may be empty).
	User string
	// ConPTYCode is why ConPTY hosts the sessions (transport.HostReasonConPTY*);
	// ConPTYDetail the local detail (the error of psmux).
	ConPTYCode   string
	ConPTYDetail string
}

// decideWindowsHost decides whether the sessions of a Windows Worker are
// hidden: psmux serves the default namespace of the user the Worker runs as
// (sessionHostSpec uses it only then), so its sessions are visible; ConPTY
// sessions live inside the Worker process and are always hidden.
func decideWindowsHost(f windowsHostFacts) SessionHostInfo {
	if f.Psmux {
		detail := ""
		if f.User != "" {
			detail = "user " + f.User
		}
		return newHostInfo(transport.SessionHostPsmux, false, transport.HostReasonPsmuxDefault, detail)
	}
	code := f.ConPTYCode
	if code == "" {
		code = transport.HostReasonConPTYSetting
	}
	return newHostInfo(transport.SessionHostConPTY, true, code, f.ConPTYDetail)
}

// maxListErrorText bounds the local listing error, in bytes.
const maxListErrorText = 200

// SessionListHealth is the outcome of the Worker's latest look at its
// sessions (the startup probe and every poll of the event publisher). The
// hello carries its code and fixed text, so the Hub can tell "0 sessions"
// apart from "the listing failed"; the local status shows the error itself.
// The core module provides it.
type SessionListHealth struct {
	last atomic.Pointer[listHealth]
}

type listHealth struct{ code, detail string }

// Set records the outcome of one look: nil clears the error.
func (h *SessionListHealth) Set(err error) {
	if h == nil {
		return
	}
	if err == nil {
		h.last.Store(nil)
		return
	}
	h.last.Store(&listHealth{code: listErrorCode(err), detail: listErrorDetail(err)})
}

func (h *SessionListHealth) load() listHealth {
	if h == nil {
		return listHealth{}
	}
	if p := h.last.Load(); p != nil {
		return *p
	}
	return listHealth{}
}

// Code is the reason code of the latest failed look (transport.ListFailed*);
// empty when the latest look succeeded or none was made.
func (h *SessionListHealth) Code() string { return h.load().code }

// LocalText is the error of the latest failed look (its first line), for the
// Worker's own status; empty when it succeeded.
func (h *SessionListHealth) LocalText() string {
	l := h.load()
	if l.code == "" || l.detail != "" {
		return l.detail
	}
	return transport.ListErrorText(l.code)
}

// applyListHealth puts the latest listing error into the facts of the hello:
// its code and fixed text only. The hello is sent before the Hub's identity
// is checked, so no program output goes in it.
func applyListHealth(f *transport.WorkerFacts, h *SessionListHealth) {
	if code := h.Code(); code != "" {
		f.SessionsListErrorCode, f.SessionsListError = code, transport.ListErrorText(code)
	}
}

// listErrorCode classifies a listing error into a fixed reason code.
func listErrorCode(err error) string {
	var exit *exec.ExitError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return transport.ListFailedTimeout
	case errors.Is(err, exec.ErrNotFound):
		return transport.ListFailedNotFound
	case errors.As(err, &exit):
		return transport.ListFailedExec
	}
	msg := err.Error()
	for _, w := range []string{"fields", "parse", "unexpected", "not a number"} {
		if strings.Contains(msg, w) {
			return transport.ListFailedFormat
		}
	}
	return transport.ListFailedExec
}

// listErrorDetail is the first line of a listing error, bounded, for the
// local status only.
func listErrorDetail(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	line = strings.TrimSpace(line)
	if len(line) > maxListErrorText {
		cut := maxListErrorText
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "..."
	}
	return line
}
