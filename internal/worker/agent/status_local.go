package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
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
		RunMode:            live.RunMode,
		SessionHost:        live.Host.Kind,
		SessionsHidden:     live.Host.Hidden,
		SessionHostReason:  live.Host.Reason,
		SessionsListError:  live.ListError,
		Alarm:              c.Alarm,
		TLSPath:            c.TLSPath,
		TLSLine:            c.TLSLine,
		HubCheckAware:      true,
	}
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
	return LiveStatus{
		Connector:          c,
		HubPreviewOnly:     r.HubPreviewOnly,
		ApprovalKnown:      r.Pending != nil,
		PairingKnown:       r.PairingKnown,
		PendingPairingCode: r.PendingPairingCode,
		RunMode:            r.RunMode,
		Host:               SessionHostInfo{Kind: r.SessionHost, Hidden: r.SessionsHidden, Reason: r.SessionHostReason},
		ListError:          r.SessionsListError,
	}, nil
}

// statusOp serves [OpLocalStatus] from src.
func statusOp(ctx context.Context, stateDir string, src LiveSource) (json.RawMessage, error) {
	live, err := src.LiveStatus(ctx)
	if err != nil {
		return nil, internalErr("read the live status", err)
	}
	b, err := json.Marshal(NewLiveStatusReply(stateDir, live))
	if err != nil {
		return nil, internalErr("encode the live status", err)
	}
	return b, nil
}
