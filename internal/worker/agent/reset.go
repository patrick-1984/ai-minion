package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/transport"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// This file is Story 4.10 on the Worker: the two ways a pairing ends (AD-9,
// R94, R81). A signed live revoke from the Hub ([Connector.HandleRevoke]) and
// the operator's `minion worker reset` ([ResetWorker]).

// ---- live revoke ------------------------------------------------------------

// revokeOpName names the answer of a live revoke in its op.result frame.
const revokeOpName = "revoke"

// revokeTimeout bounds each read of a control stream and the wipe it causes.
const revokeTimeout = 10 * time.Second

// ErrRevokeRejected is returned by [Connector.HandleRevoke] for a revoke that
// the pinned Hub key did not sign for this connection, a malformed one, and a
// revoke whose wipe did not happen. Nothing was deleted then.
var ErrRevokeRejected = errors.New("agent: the revoke was not accepted; nothing was deleted")

type revokeFrame struct {
	Type string       `json:"type"`
	Sig  identity.B64 `json:"sig"`
}

// IsRevokeFrame reports whether the first frame of an op stream is a revoke
// (as opposed to an op.request). The op server of Story 5.2 uses it to hand
// such a stream to [Connector.HandleRevoke] instead of the executor.
func IsRevokeFrame(raw json.RawMessage) bool {
	var f struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &f) == nil && f.Type == proto.TypeRevoke
}

// HandleRevoke handles a revoke frame that the Hub sent over the live
// connection l, as the first frame of an op stream (AD-9). Only a signature by
// the pinned Hub key with purpose "revoke" over the transcript hash of this
// very connection wipes the pairing; anything else is ignored and logged and
// the Worker keeps its state. The wipe is the one of the revoke that answers a
// connect: pin and enrollment go, the key pair, the share selection, the
// session counter and the tmux sessions stay. The connector then ends the
// connection by itself, because the enrollment changed, and idles until it is
// enrolled again.
//
// confirm, when not nil, is called once the signature is valid and before the
// pairing is deleted, because deleting ends the connection: the op server
// answers the Hub with [RevokeAck] there. Should the wipe then fail, the Hub
// has still removed the Worker, so the next connect is answered with the
// revoke again.
func (c *Connector) HandleRevoke(l *Link, raw json.RawMessage, confirm func()) error {
	var f revokeFrame
	if err := json.Unmarshal(raw, &f); err != nil || f.Type != proto.TypeRevoke || len(f.Sig) == 0 {
		c.log.Warn("ignored a malformed revoke frame; nothing was deleted")
		return ErrRevokeRejected
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), revokeTimeout)
	defer cancel()
	if err := identity.Verify(l.pin.Pub, identity.PurposeRevoke, l.transcript, f.Sig); err != nil {
		// Let applyRevoke log and record the refusal.
		c.applyRevoke(ctx, l.gen, l.pin, l.transcript, f.Sig)
		return ErrRevokeRejected
	}
	if confirm != nil {
		confirm()
	}
	if wiped, _ := c.applyRevoke(ctx, l.gen, l.pin, l.transcript, f.Sig); !wiped {
		return ErrRevokeRejected
	}
	return nil
}

// RevokeAck is the frame a Worker answers a valid live revoke with. The Hub
// counts only this frame as the Worker's confirmation.
func RevokeAck() map[string]string {
	return map[string]string{"type": proto.TypeOpResult, "op": revokeOpName}
}

// ServeRevokes is a [LinkHandler] that serves live revokes on its own: it
// accepts the op streams of the link, reads the first frame of each and
// handles a revoke; every other stream is refused with unsupported_op. Use it
// through [Connector.OnLink] only on a Worker that has no op server, because
// both would accept the same streams. The op server (Story 5.2) calls
// [IsRevokeFrame] and [Connector.HandleRevoke] itself.
//
// A Worker that has no handler for the live revoke is still revoked: the Hub
// ends the connection of a removed Worker, and the answer to the reconnect is
// the signed revoke of the tombstone (AD-9).
func (c *Connector) ServeRevokes(l *Link) {
	for {
		st, err := l.Session.Accept(l.Context())
		if err != nil {
			return
		}
		go c.serveControlStream(l, st)
	}
}

func (c *Connector) serveControlStream(l *Link, st *transport.Stream) {
	defer func() { _ = st.Close() }()
	_ = st.SetReadDeadline(time.Now().Add(revokeTimeout))
	raw, err := st.ReadFrame()
	if err != nil {
		return
	}
	_ = st.SetDeadline(time.Now().Add(revokeTimeout))
	if !IsRevokeFrame(raw) {
		_ = st.WriteFrame(proto.NewError(proto.CodeUnsupportedOp, "this Worker serves no op streams yet").Frame())
		return
	}
	answered := false
	err = c.HandleRevoke(l, raw, func() {
		answered = st.WriteFrame(RevokeAck()) == nil
	})
	if err != nil && !answered {
		_ = st.WriteFrame(proto.NewError(proto.CodeForbidden, "the revoke is not valid for this connection").Frame())
	}
}

// ---- reset ------------------------------------------------------------------

// ResetConfigKeys are the keys that `minion worker reset` removes from the
// config file: the local policy (preview_only, share_existing) and the Hub
// settings (server, hub_ca_file, hub_insecure) that the first-run wizard asks
// for again. Machine settings (state directory, tmux socket, name, logging)
// stay. Values given by flag or environment are not in the file and cannot be
// reset from here.
var ResetConfigKeys = []string{
	config.KeyPreviewOnly, config.KeyShareExisting,
	config.KeyServer, config.KeyHubCAFile, config.KeyHubInsecure,
}

// ResetStore is the persistence port of [ResetWorker]; *store.Store
// implements it.
type ResetStore interface {
	// ResetPairing deletes the pin, the enrollment and the share selection
	// in one transaction and keeps everything else.
	ResetPairing(ctx context.Context) (store.PairingReset, error)
}

var _ ResetStore = (*store.Store)(nil)

// ResetOptions configure [ResetWorker].
type ResetOptions struct {
	// StateDir holds worker.key and the config file. Required.
	StateDir string
	// ConfigPath is the config file; empty means <StateDir>/config.json.
	ConfigPath string
	// Store is the Worker state. Required.
	Store ResetStore
	// Enrollments, when set, is told about the wipe, so that a connector in
	// the same process drops its connection and idles at once. `minion worker
	// reset` runs in another process and leaves it nil: it refuses while a
	// Worker answers on its local control socket, so no connection is alive.
	Enrollments *Enrollments
}

// ResetReport says what [ResetWorker] removed.
type ResetReport struct {
	// WasEnrolled is true when a Hub pin and enrollment existed.
	WasEnrolled bool
	// ShareCleared is true when a share selection existed.
	ShareCleared bool
	// KeyDeleted is true when a key pair existed.
	KeyDeleted bool
	// ConfigKeys are the keys removed from the config file, sorted.
	ConfigKeys []string
}

// Nothing reports whether the Worker was already in the unconfigured state.
func (r ResetReport) Nothing() bool {
	return !r.WasEnrolled && !r.ShareCleared && !r.KeyDeleted && len(r.ConfigKeys) == 0
}

// ResetWorker returns the Worker to the unconfigured state (R81, AD-9): it
// deletes the key pair, the pin and the enrollment, the share selection and
// the local policy, so that the next `minion worker up` runs the wizard. It
// keeps the tmux sessions (it never talks to tmux), the session counter, the
// result cache and the local audit.
//
// The steps are ordered so that a failure half way is safe and a second run
// finishes the job: the database first (without an enrollment the connector
// does not dial), then the config file, the key last. Every step is
// idempotent. The Hub is not told: the Worker stays listed there until a user
// removes it, and a re-enrollment creates a new Worker with the new key.
func ResetWorker(ctx context.Context, o ResetOptions) (ResetReport, error) {
	var rep ResetReport
	if o.StateDir == "" || o.Store == nil {
		return rep, errors.New("agent: reset needs the state directory and the Worker state")
	}
	path := o.ConfigPath
	if path == "" {
		path = filepath.Join(o.StateDir, "config.json")
	}
	// An unreadable config file stops the reset before anything is deleted.
	cfg, err := config.LoadFile(path)
	if err != nil {
		return rep, fmt.Errorf("read the config file: %w", err)
	}
	pr, err := o.Store.ResetPairing(ctx)
	if err != nil {
		return rep, fmt.Errorf("reset the Worker state: %w", err)
	}
	rep.WasEnrolled, rep.ShareCleared = pr.Enrollment, pr.Share
	if o.Enrollments != nil {
		if _, err := o.Enrollments.Wipe(ctx); err != nil { // already gone; this advances the generation
			return rep, fmt.Errorf("reset the enrollment state: %w", err)
		}
	}
	if rep.ConfigKeys, err = clearConfigKeys(cfg, ResetConfigKeys); err != nil {
		return rep, err
	}

	keyPath := filepath.Join(o.StateDir, WorkerKeyFile)
	switch err := os.Remove(keyPath); {
	case err == nil:
		rep.KeyDeleted = true
	case errors.Is(err, fs.ErrNotExist):
	default:
		return rep, fmt.Errorf("delete the Worker key %s: %w", keyPath, err)
	}
	return rep, nil
}

// clearConfigKeys removes keys from the loaded config file and reports the
// ones that were present. A missing file, or one without these keys, is left
// alone: reset never creates a config file.
func clearConfigKeys(f *config.ConfigFile, keys []string) ([]string, error) {
	var removed []string
	for _, k := range keys {
		if _, ok, _ := f.Lookup(k); ok {
			f.Delete(k)
			removed = append(removed, k)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	sort.Strings(removed)
	if err := f.Save(); err != nil {
		return nil, fmt.Errorf("write the config file: %w", err)
	}
	return removed, nil
}
