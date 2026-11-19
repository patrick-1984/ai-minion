package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// Local share ops (tray W6a). They work unenrolled, under share_lock (which
// only locks the Hub out) and in preview-only, because they are the owner's
// policy edit on this machine, not a session write.
const (
	OpLocalShareList  = "worker_share_list"
	OpLocalShareApply = "worker_share_apply"
	// FeatureShareLocal is their local capability.
	FeatureShareLocal = "share_local.v1"
	// FeatureShareReplace is the local capability of an exact replacement of
	// the selection (LocalShareApplyArgs.Replace).
	FeatureShareReplace = "share_replace.v1"
)

// Inventory states of [ShareListResult].
const (
	InventoryOK          = "ok"
	InventoryUnavailable = "unavailable"
)

// ShareListSession is one session of the terminal host and whether it is
// shared with the Hub.
type ShareListSession struct {
	Name   string `json:"name"`
	Shared bool   `json:"shared"`
}

// ShareListResult is the reply of [OpLocalShareList] and [OpLocalShareApply].
type ShareListResult struct {
	// Rule is the share mode (all, none, select, later) and Selected the
	// sessions of select.
	Rule     string   `json:"rule"`
	Selected []string `json:"selected"`
	Revision uint64   `json:"revision"`
	// Lock is share_lock: the Hub cannot change the selection.
	Lock bool `json:"lock"`
	// OverriddenBy names the sources (flag, env) whose share_existing the
	// Worker applies again at its next start; empty when none.
	OverriddenBy []string `json:"overridden_by"`
	// Inventory is ok, or unavailable when the sessions could not be listed:
	// an empty Sessions then means "not known", never "no session".
	Inventory string             `json:"inventory"`
	Sessions  []ShareListSession `json:"sessions"`
}

// LocalShareApplyArgs are the arguments of [OpLocalShareApply]: an optional
// mode, sessions to add and to remove, and the revision the caller read.
type LocalShareApplyArgs struct {
	Mode       string   `json:"mode,omitempty"`
	Add        []string `json:"add,omitempty"`
	Remove     []string `json:"remove,omitempty"`
	IfRevision uint64   `json:"if_revision"`
	// Replace makes the selection exactly Add (mode select), whatever it is
	// when the Worker applies it. A client that sets it requires
	// [FeatureShareReplace], so that an older Worker, which would merge, refuses.
	Replace bool `json:"replace,omitempty"`
}

// shareLocal serves the local share ops.
type shareLocal struct {
	st     *store.Store
	ex     *Executor
	policy *Policy
	// overriddenBy is computed once from the Worker's start settings.
	overriddenBy []string
}

// shareOverriddenBy returns the sources (flag, env) of share_existing in s:
// the Worker applies that mode at every start.
func shareOverriddenBy(s Settings) []string {
	out := []string{}
	if s.Resolver == nil {
		return out
	}
	r := *s.Resolver
	r.File = nil
	if res, err := r.Resolve(config.KeyShareExisting); err == nil && res.Value != "" &&
		(res.Source == config.SourceFlag || res.Source == config.SourceEnv) {
		out = append(out, string(res.Source))
	}
	return out
}

func (s shareLocal) list(ctx context.Context, state store.ShareState) ShareListResult {
	r := ShareListResult{
		Rule: string(state.Mode), Selected: state.Sessions, Revision: state.Revision,
		Lock: s.policy != nil && s.policy.ShareLocked(), OverriddenBy: s.overriddenBy,
		Inventory: InventoryOK, Sessions: []ShareListSession{},
	}
	if r.Selected == nil {
		r.Selected = []string{}
	}
	sessions, err := s.ex.host.ListSessions(ctx)
	if err != nil {
		r.Inventory = InventoryUnavailable
		return r
	}
	for _, sess := range sessions {
		r.Sessions = append(r.Sessions, ShareListSession{Name: sess.Name, Shared: state.Includes(sess.Name)})
	}
	sort.Slice(r.Sessions, func(i, j int) bool { return r.Sessions[i].Name < r.Sessions[j].Name })
	return r
}

func (s shareLocal) listOp(ctx context.Context) (json.RawMessage, error) {
	state, err := s.st.LoadShareStateContext(ctx)
	if err != nil {
		return nil, internalErr("read the share selection", err)
	}
	return json.Marshal(s.list(ctx, state))
}

func (s shareLocal) applyOp(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var a LocalShareApplyArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, apiErr(proto.CodeInvalidArgument, "", "invalid share change")
	}
	state, err := s.st.ApplyShareChangeContext(ctx, store.ShareChange{
		Mode: store.ShareMode(a.Mode), Add: a.Add, Remove: a.Remove, IfRevision: a.IfRevision, Replace: a.Replace,
	})
	switch {
	case errors.Is(err, store.ErrShareRevision):
		return nil, apiErr(proto.CodeConflict, "state_changed", "the share selection changed since it was read; nothing was changed")
	case errors.Is(err, store.ErrInvalidShare):
		return nil, apiErr(proto.CodeInvalidArgument, "", err.Error())
	case err != nil:
		return nil, internalErr("change the share selection", err)
	}
	return json.Marshal(s.list(ctx, state))
}

func localShareListOp(l localExecutor, ctx context.Context, _ localapi.Request) (json.RawMessage, error) {
	if l.share == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "this Worker cannot list its sharing")
	}
	return l.share.listOp(ctx)
}

func localShareApplyOp(l localExecutor, ctx context.Context, rq localapi.Request) (json.RawMessage, error) {
	if l.share == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "this Worker cannot change its sharing here")
	}
	return l.share.applyOp(ctx, rq.Args)
}
