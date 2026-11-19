package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/version"
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

// FeatureStopGuard is the local capability of a worker_stop and a
// worker_lifecycle that check LocalStopArgs.Instance and EnrollmentEpoch: a
// client that sends them requires it, so that an older Worker, which would
// ignore them and stop, refuses.
const FeatureStopGuard = "stop_guard.v1"

// LocalStopArgs are the optional arguments of [OpLocalStop]: the Worker stops
// only when it is the instance the caller read and, when the caller sends
// the enrollment generation, only while that enrollment is still the one in
// the state directory.
type LocalStopArgs struct {
	Instance        string  `json:"instance,omitempty"`
	EnrollmentEpoch *uint64 `json:"enrollment_epoch,omitempty"`
}

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
	// PairingAttempt and PairingExpiresAt (RFC 3339) belong to the pending
	// code (tray W8a); an older Worker omits them.
	PairingAttempt   string `json:"pairing_attempt,omitempty"`
	PairingExpiresAt string `json:"pairing_expires_at,omitempty"`
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
	// Paused is whether the Worker enforces a pause of its Hub traffic
	// (pause.v1); an older Worker omits it.
	Paused *bool `json:"paused,omitempty"`
	// NewSessionsBlocked is whether the Worker refuses new sessions
	// (allow_new_sessions); an older Worker omits it.
	NewSessionsBlocked *bool `json:"new_sessions_blocked,omitempty"`
	// NewSessionsSources are the local sources (flag, env, file) that block
	// them; an older Worker omits it.
	NewSessionsSources []string `json:"new_sessions_sources,omitempty"`
	// Version is the release of the running Worker and Capabilities its
	// local features (tray W10a); an older Worker omits them.
	Version      string   `json:"version,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	// SessionsLostOnStop is how many sessions a stop would end, omitted when
	// they cannot be counted; CountReported says the Worker reports it.
	SessionsLostOnStop *int `json:"sessions_lost_on_stop,omitempty"`
	CountReported      bool `json:"count_reported,omitempty"`
	// LogSinks is the state of the remote log sinks (v0.14.0); an older
	// Worker omits it.
	LogSinks []ops.LogSinkStatus `json:"log_sinks,omitempty"`
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
		PairingAttempt:     live.PairingAttempt,
		RunMode:            live.RunMode,
		SessionHost:        live.Host.Kind,
		SessionsHidden:     live.Host.Hidden,
		SessionHostReason:  live.Host.Reason,
		SessionsListError:  live.ListError,
		Alarm:              c.Alarm,
		TLSPath:            c.TLSPath,
		TLSLine:            c.TLSLine,
		HubCheckAware:      true,
		Paused:             live.Paused,
		NewSessionsBlocked: live.NewSessionsBlocked,
		NewSessionsSources: live.NewSessionsSources,
		SessionsLostOnStop: live.SessionsLostOnStop,
		CountReported:      live.CountReported,
		LogSinks:           live.LogSinks,
	}
	if !c.LastErrorAt.IsZero() {
		r.LastErrorAt = c.LastErrorAt.UTC().Format(time.RFC3339Nano)
	}
	if !live.PairingExpiresAt.IsZero() {
		r.PairingExpiresAt = live.PairingExpiresAt.UTC().Format(time.RFC3339Nano)
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
	var pairingExpires time.Time
	for _, f := range []struct {
		name string
		in   string
		out  *time.Time
	}{
		{"last_error_at", r.LastErrorAt, &c.LastErrorAt},
		{"connected_since", r.ConnectedSince, &c.ConnectedSince},
		{"pairing_expires_at", r.PairingExpiresAt, &pairingExpires},
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
		PairingAttempt:     r.PairingAttempt,
		PairingExpiresAt:   pairingExpires,
		RunMode:            r.RunMode,
		Host:               SessionHostInfo{Kind: r.SessionHost, Hidden: r.SessionsHidden, Reason: r.SessionHostReason},
		ListError:          r.SessionsListError,
		Paused:             r.Paused,
		NewSessionsBlocked: r.NewSessionsBlocked,
		NewSessionsSources: r.NewSessionsSources,
		Instance:           r.Instance,
		StateDir:           r.StateDir,
		Version:            r.Version,
		Capabilities:       r.Capabilities,
		SessionsLostOnStop: r.SessionsLostOnStop,
		CountReported:      r.CountReported,
		LogSinks:           r.LogSinks,
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
	reply.Version = version.Version
	reply.Capabilities = slices.Sorted(slices.Values(localFeatures))
	b, err := json.Marshal(reply)
	if err != nil {
		return nil, internalErr("encode the live status", err)
	}
	return b, nil
}
