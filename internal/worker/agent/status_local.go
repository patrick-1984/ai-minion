package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// OpLocalStatus is the op of the local control socket that returns the live
// status of the running Worker (AD-22). It is served by the local socket
// module itself and is not part of the AD-2 op catalog: it is never announced
// to the Hub and never reaches the Executor, so a Hub cannot call it.
const OpLocalStatus = "worker_status"

// OpLocalStop is the op of the local control socket that stops the running
// Worker ("minion worker down", v0.9.3). Only the user who may open the
// socket can ask; the reply comes before the Worker stops.
const OpLocalStop = "worker_stop"

// LiveStatusReply is the JSON result of [OpLocalStatus]. It carries no secret:
// the pairing code is the one the Worker shows to its human anyway (AD-20), and
// it is only ever sent to the OS user that owns the socket.
type LiveStatusReply struct {
	// StateDir is the state directory of the answering Worker, so that a client
	// asking about another state directory can tell the answer is not its own.
	StateDir string `json:"state_dir"`
	// State is idle, connecting, connected or backoff.
	State          string `json:"state"`
	Reason         string `json:"reason,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	LastErrorAt    string `json:"last_error_at,omitempty"`
	ConnectedSince string `json:"connected_since,omitempty"`
	Owner          string `json:"owner,omitempty"`
	// WorkerID and Pending are from the welcome of the current connection:
	// the id the Hub gave this Worker and whether it waits for approval.
	// Pending is nil in the reply of an older Worker, which does not send it.
	WorkerID       string `json:"worker_id,omitempty"`
	Pending        *bool  `json:"pending,omitempty"`
	HubPreviewOnly bool   `json:"hub_preview_only"`
	// PairingKnown is false when the Worker has no source for a pairing code,
	// so that "no code" is not mistaken for "no code pending".
	PairingKnown       bool   `json:"pairing_known"`
	PendingPairingCode string `json:"pending_pairing_code,omitempty"`
	// Pairing is the private pairing presentation of the managed Android mode
	// (code, expiry, Worker words and the Hub words as presented).
	Pairing *PairingPresentation `json:"pairing,omitempty"`
	// RunMode, the session host and the latest listing error (v0.10.0); an
	// older Worker omits them.
	RunMode           string `json:"run_mode,omitempty"`
	SessionHost       string `json:"session_host,omitempty"`
	SessionsHidden    bool   `json:"sessions_hidden,omitempty"`
	SessionHostReason string `json:"session_host_reason,omitempty"`
	SessionsListError string `json:"sessions_list_error,omitempty"`
	// Alarm and the TLS path of the latest connection (v0.11.0); an older
	// Worker omits them.
	Alarm   string `json:"alarm,omitempty"`
	TLSPath string `json:"tls_path,omitempty"`
	TLSLine string `json:"tls_line,omitempty"`
	// HubCheckAware is true for a Worker that applies a stored hub_check
	// mismatch (v0.11.0 or later): it stops connecting when `verify-hub`
	// stores one. An older Worker omits it and would reconnect.
	HubCheckAware bool `json:"hub_check_aware,omitempty"`
	// Instance identifies this control socket server across status and writes.
	Instance string `json:"instance,omitempty"`
	// LogSinks is the state of the remote log sinks (v0.14.0); an older
	// Worker omits it.
	LogSinks []ops.LogSinkStatus `json:"log_sinks,omitempty"`
	// Paused is the pause the Worker applies; an older Worker omits it.
	Paused *bool `json:"paused,omitempty"`
	// AllowNewSessions is on or off as this Worker enforces it now; an older
	// Worker omits it.
	AllowNewSessions string `json:"allow_new_sessions,omitempty"`
	// Daemon identifies the running daemon (instance, version, start time,
	// optional PID); an older Worker omits it.
	Daemon *DaemonInfo `json:"daemon,omitempty"`
}

// DaemonInfo is the wire contract that identifies one running Worker daemon
// in the reply of [OpLocalStatus] (docs/schemas/local-status-daemon.schema.json).
// Instance is one random id per daemon process; it equals the top-level
// instance of the reply. Version is the daemon's own build version, which can
// differ from the version of the CLI that asks. Every field but Instance may
// be absent: absent means not known, never zero.
type DaemonInfo struct {
	Instance string `json:"instance"`
	Version  string `json:"version,omitempty"`
	// StartedAt is when the daemon started, RFC 3339 UTC.
	StartedAt string `json:"started_at,omitempty"`
	// PID is the operating system process id; informative only.
	PID int `json:"pid,omitempty"`
}

// NewLiveStatusReply builds the reply of [OpLocalStatus] from live.
func NewLiveStatusReply(stateDir string, live LiveStatus) LiveStatusReply {
	c := live.Connector
	r := LiveStatusReply{
		StateDir:           stateDir,
		State:              string(c.State),
		Reason:             c.Reason,
		LastError:          c.LastError,
		Owner:              c.Welcome.Owner,
		WorkerID:           c.Welcome.WorkerID,
		Pending:            &c.Welcome.Pending,
		HubPreviewOnly:     live.HubPreviewOnly,
		PairingKnown:       live.PairingKnown || live.PendingPairingCode != "",
		PendingPairingCode: live.PendingPairingCode,
		Pairing:            live.Pairing,
		RunMode:            live.RunMode,
		SessionHost:        live.Host.Kind,
		SessionsHidden:     live.Host.Hidden,
		SessionHostReason:  live.Host.Reason,
		SessionsListError:  live.ListError,
		Alarm:              c.Alarm,
		TLSPath:            c.TLSPath,
		TLSLine:            c.TLSLine,
		HubCheckAware:      true,
		LogSinks:           live.LogSinks,
	}
	if d := live.Daemon; d != nil {
		w := DaemonInfo{Instance: d.Instance, Version: d.Version, PID: d.PID}
		if !d.StartedAt.IsZero() {
			w.StartedAt = d.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		r.Daemon = &w
	}
	if live.PauseKnown {
		r.Paused = &live.Paused
	}
	r.AllowNewSessions = live.NewSessions
	if !c.LastErrorAt.IsZero() {
		r.LastErrorAt = c.LastErrorAt.UTC().Format(time.RFC3339Nano)
	}
	if !c.ConnectedSince.IsZero() {
		r.ConnectedSince = c.ConnectedSince.UTC().Format(time.RFC3339Nano)
	}
	return r
}

// LiveStatus converts the reply back to a [LiveStatus].
func (r LiveStatusReply) LiveStatus() (LiveStatus, error) {
	c := Status{State: ConnState(r.State), Reason: r.Reason, LastError: r.LastError, Alarm: r.Alarm, TLSPath: r.TLSPath, TLSLine: r.TLSLine}
	c.Welcome.Owner, c.Welcome.WorkerID = r.Owner, r.WorkerID
	if r.Pending != nil {
		c.Welcome.Pending = *r.Pending
	}
	for _, f := range []struct {
		name string
		in   string
		out  *time.Time
	}{
		{"last_error_at", r.LastErrorAt, &c.LastErrorAt},
		{"connected_since", r.ConnectedSince, &c.ConnectedSince},
	} {
		if f.in == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, f.in)
		if err != nil {
			return LiveStatus{}, fmt.Errorf("the Worker sent an invalid %s: %w", f.name, err)
		}
		*f.out = t
	}
	var daemon *DaemonInfo
	switch {
	case r.Daemon != nil:
		d := *r.Daemon
		daemon = &d
	case r.Instance != "":
		// A daemon older than this contract knows its instance only.
		daemon = &DaemonInfo{Instance: r.Instance}
	}
	var daemonStart time.Time
	if daemon != nil && daemon.StartedAt != "" {
		t, err := time.Parse(time.RFC3339Nano, daemon.StartedAt)
		if err != nil {
			return LiveStatus{}, fmt.Errorf("the Worker sent an invalid daemon.started_at: %w", err)
		}
		daemonStart = t
	}
	var dl *LiveDaemon
	if daemon != nil {
		dl = &LiveDaemon{Instance: daemon.Instance, Version: daemon.Version, StartedAt: daemonStart, PID: daemon.PID}
	}
	return LiveStatus{
		Daemon:             dl,
		Connector:          c,
		HubPreviewOnly:     r.HubPreviewOnly,
		ApprovalKnown:      r.Pending != nil,
		PairingKnown:       r.PairingKnown,
		PendingPairingCode: r.PendingPairingCode,
		Pairing:            r.Pairing,
		RunMode:            r.RunMode,
		Host:               SessionHostInfo{Kind: r.SessionHost, Hidden: r.SessionsHidden, Reason: r.SessionHostReason},
		ListError:          r.SessionsListError,
		LogSinks:           r.LogSinks,
		Paused:             r.Paused != nil && *r.Paused,
		PauseKnown:         r.Paused != nil,
		NewSessions:        r.AllowNewSessions,
	}, nil
}

// statusOp serves [OpLocalStatus] from src.
func statusOp(ctx context.Context, stateDir, instance string, src LiveSource) (json.RawMessage, error) {
	live, err := src.LiveStatus(ctx)
	if err != nil {
		return nil, internalErr("read the live status", err)
	}
	reply := NewLiveStatusReply(stateDir, live)
	reply.Instance = instance
	if reply.Daemon != nil {
		reply.Daemon.Instance = instance
	}
	b, err := json.Marshal(reply)
	if err != nil {
		return nil, internalErr("encode the live status", err)
	}
	return b, nil
}
