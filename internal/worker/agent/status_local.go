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
	HubPreviewOnly bool   `json:"hub_preview_only"`
	// PairingKnown is false when the Worker has no source for a pairing code,
	// so that "no code" is not mistaken for "no code pending".
	PairingKnown       bool   `json:"pairing_known"`
	PendingPairingCode string `json:"pending_pairing_code,omitempty"`
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
		HubPreviewOnly:     live.HubPreviewOnly,
		PairingKnown:       live.PairingKnown || live.PendingPairingCode != "",
		PendingPairingCode: live.PendingPairingCode,
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
	c := Status{State: ConnState(r.State), Reason: r.Reason, LastError: r.LastError}
	c.Welcome.Owner = r.Owner
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
		PairingKnown:       r.PairingKnown,
		PendingPairingCode: r.PendingPairingCode,
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
