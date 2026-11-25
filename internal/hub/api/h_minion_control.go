package api

// Minion control (v0.9.0, A17): administrators nominate master sessions and
// let Workers accept control from other Workers. The grant a master session
// holds is computed from these rows (store.SessionControl); the call path,
// authz and origins are those of session control.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// ControlWorkerBase is what the Minion control handlers know of a Worker.
type ControlWorkerBase struct {
	ID, Name, Owner string
	// OwnerID is the owning user's id: a change is stored only while the
	// Worker still has this owner.
	OwnerID string
	Online  bool
	// CanBeMaster is false for a Worker that cannot originate control calls.
	CanBeMaster      bool
	MasterSessionV1  bool
	MasterCandidates []ops.ControlMasterCandidate
	BoundMasters     []ops.ControlBoundMaster
}

// MinionControlStore is what the handlers need; *store.Store implements it.
type MinionControlStore interface {
	ListMasterSessions(ctx context.Context) ([]store.MasterRow, error)
	PutMasterSessionIfOwner(ctx context.Context, m store.MasterRow, owner string) error
	DeleteMasterSessionIfOwner(ctx context.Context, workerID, session, owner string) (bool, error)
	BoundMasterActive(ctx context.Context, workerID, session string) (bool, error)
	RevokeAcceptIfOwners(ctx context.Context, a store.AcceptRow, targetOwner, sourceOwner string) (bool, error)
	ListAccepts(ctx context.Context) ([]store.AcceptRow, error)
	PutAcceptIfOwners(ctx context.Context, a store.AcceptRow, targetOwner, sourceOwner string) error
	DeleteAcceptIfOwners(ctx context.Context, target, source, targetOwner, sourceOwner string) (bool, error)
	ListWorkerControl(ctx context.Context) (map[string]store.WorkerControlRow, error)
	SetWorkerControlIfOwner(ctx context.Context, workerID, owner string, userManaged, acceptAll *bool) error
	SessionControl(ctx context.Context, workerID, sessionName string) (store.SessionControlRow, error)
}

var _ MinionControlStore = (*store.Store)(nil)

// RegisterMinionControl registers the control_* operations (manage: admins).
func RegisterMinionControl(r *Registry, st MinionControlStore, workers func(context.Context) ([]ControlWorkerBase, error),
	bound ...func(context.Context, string, string) ([]ops.ControlChain, error)) error {
	if st == nil || workers == nil {
		return errors.New("api: the Minion control handlers need a store and the Workers")
	}
	h := controlHandlers{st: st, workers: workers,
		pinW: newOwnerPins[ops.ControlSetWorkerArgs](), pinM: newOwnerPins[ops.ControlSetMasterArgs](), pinA: newOwnerPins[ops.ControlSetAcceptArgs]()}
	if len(bound) > 0 {
		h.bound = bound[0]
	}
	// The setters are decided (and audited) on the Worker they change.
	for _, reg := range []error{
		HandleTyped(r, ops.OpControlList, h.list),
		HandleTyped(r, ops.OpControlSetWorker, h.setWorker, WithResource(h.resource)),
		HandleTyped(r, ops.OpControlSetMaster, h.setMaster, WithResource(h.resource)),
		HandleTyped(r, ops.OpControlSetAccept, h.setAccept, WithResource(h.resource)),
		HandleTyped(r, ops.OpControlCheck, h.check),
	} {
		if reg != nil {
			return reg
		}
	}
	return nil
}

type controlHandlers struct {
	st      MinionControlStore
	workers func(context.Context) ([]ControlWorkerBase, error)
	bound   func(context.Context, string, string) ([]ops.ControlChain, error)
	// The owners each setter was decided for, captured with the decision
	// (resource) and checked again in the store's write transaction.
	pinW *ownerPins[ops.ControlSetWorkerArgs]
	pinM *ownerPins[ops.ControlSetMasterArgs]
	pinA *ownerPins[ops.ControlSetAcceptArgs]
}

func (h controlHandlers) state(ctx context.Context) ([]ops.ControlWorker, map[string]ControlWorkerBase, error) {
	base, err := h.workers(ctx)
	if err != nil {
		return nil, nil, err
	}
	masters, err := h.st.ListMasterSessions(ctx)
	if err != nil {
		return nil, nil, err
	}
	accepts, err := h.st.ListAccepts(ctx)
	if err != nil {
		return nil, nil, err
	}
	ctl, err := h.st.ListWorkerControl(ctx)
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]ControlWorkerBase{}
	out := make([]ops.ControlWorker, 0, len(base))
	for _, b := range base {
		byID[b.ID] = b
		c, ok := ctl[b.ID]
		if !ok {
			c = store.WorkerControlRow{UserManaged: true}
		}
		w := ops.ControlWorker{WorkerID: b.ID, Name: b.Name, Owner: b.Owner, Online: b.Online,
			UserManaged: c.UserManaged, AcceptAll: c.AcceptAll, CanBeMaster: b.CanBeMaster,
			MasterSessionV1: b.MasterSessionV1, MasterCandidates: b.MasterCandidates, BoundMasters: b.BoundMasters,
			Accepts: []ops.ControlAccept{}, Masters: []ops.ControlMaster{}}
		for _, m := range masters {
			if m.WorkerID == b.ID {
				w.Masters = append(w.Masters, ops.ControlMaster{SessionName: m.SessionName, Legacy: m.LegacyTargetsJSON != ""})
			}
		}
		for _, a := range accepts {
			if a.TargetWorkerID == b.ID {
				ca := ops.ControlAccept{SourceWorkerID: a.SourceWorkerID}
				if a.Sessions != nil {
					names := append([]string{}, a.Sessions...)
					ca.Sessions = &names
				}
				w.Accepts = append(w.Accepts, ca)
			}
		}
		out = append(out, w)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, byID, nil
}

func (h controlHandlers) list(ctx context.Context, c *Call, _ ops.ControlListArgs) (ops.ControlListResult, error) {
	// The list covers every Worker: an API key limited to some resources
	// may not read it.
	if err := requireHubWideScope(c.Principal); err != nil {
		return ops.ControlListResult{}, err
	}
	ws, _, err := h.state(ctx)
	return ops.ControlListResult{Workers: ws}, err
}

func ownerErr(err error) error {
	switch {
	case errors.Is(err, store.ErrBoundMasterSlot):
		return NewError(proto.CodeConflict, "use the session-bound master action for this session")
	case errors.Is(err, store.ErrWorkerOwnerChanged):
		return NewError(proto.CodeConflict, "the Worker changed owner meanwhile; reload and decide again")
	case errors.Is(err, store.ErrWorkerNotFound):
		return NewError(proto.CodeNotFound, "no such Worker")
	}
	return err
}

func (h controlHandlers) known(ctx context.Context, ids ...string) (map[string]ControlWorkerBase, error) {
	base, err := h.workers(ctx)
	if err != nil {
		return nil, err
	}
	m := map[string]ControlWorkerBase{}
	for _, b := range base {
		m[b.ID] = b
	}
	for _, id := range ids {
		if _, ok := m[id]; !ok {
			return nil, NewError(proto.CodeNotFound, "no such Worker")
		}
	}
	return m, nil
}

func (h controlHandlers) setWorker(ctx context.Context, c *Call, a ops.ControlSetWorkerArgs) (ops.WriteResult, error) {
	var owners []string
	var err error
	if p, ok := c.Args.(*ops.ControlSetWorkerArgs); ok {
		owners, err = pinned(h.pinW.take(p))
	} else {
		owners, err = pinned("", false)
	}
	if err != nil {
		return ops.WriteResult{}, err
	}
	c.AuditDetail = workerControlDetail(a)
	if err := h.st.SetWorkerControlIfOwner(ctx, a.WorkerID, owners[0], a.UserManaged, a.AcceptAll); err != nil {
		return ops.WriteResult{}, ownerErr(err)
	}
	return delivered(), nil
}

func (h controlHandlers) setMaster(ctx context.Context, c *Call, a ops.ControlSetMasterArgs) (ops.WriteResult, error) {
	m, err := h.known(ctx, a.WorkerID)
	if err != nil {
		return ops.WriteResult{}, err
	}
	if a.SessionName == "" || !ops.ValidSessionName(a.SessionName) {
		return ops.WriteResult{}, NewError(proto.CodeInvalidArgument, "session_name is not a valid session name")
	}
	// The session name goes in the source, never in the detail.
	c.AuditSource = a.WorkerID + ":" + a.SessionName
	c.AuditDetail = "session_set=" + strconv.FormatBool(a.Master)
	var owners []string
	if p, ok := c.Args.(*ops.ControlSetMasterArgs); ok {
		owners, err = pinned(h.pinM.take(p))
	} else {
		owners, err = pinned("", false)
	}
	if err != nil {
		return ops.WriteResult{}, err
	}
	if a.Master {
		if !m[a.WorkerID].CanBeMaster {
			return ops.WriteResult{}, NewError(proto.CodeConflict, "this Worker cannot control others (a thin Worker or one older than 0.8.0)")
		}
		err = h.st.PutMasterSessionIfOwner(ctx, store.MasterRow{WorkerID: a.WorkerID, SessionName: a.SessionName,
			CreatedBy: c.Identity.Actor, CreatedAt: time.Now().UnixMilli()}, owners[0])
	} else {
		// A session-bound master is not in master_session: deleting there
		// would answer delivered and revoke nothing.
		var bound bool
		if bound, err = h.st.BoundMasterActive(ctx, a.WorkerID, a.SessionName); err == nil && bound {
			err = store.ErrBoundMasterSlot
		}
		if err == nil {
			_, err = h.st.DeleteMasterSessionIfOwner(ctx, a.WorkerID, a.SessionName, owners[0])
		}
	}
	if err != nil {
		return ops.WriteResult{}, ownerErr(err)
	}
	return delivered(), nil
}

func (h controlHandlers) setAccept(ctx context.Context, c *Call, a ops.ControlSetAcceptArgs) (ops.WriteResult, error) {
	m, err := h.known(ctx, a.WorkerID, a.SourceWorkerID)
	if err != nil {
		return ops.WriteResult{}, err
	}
	if a.WorkerID == a.SourceWorkerID {
		return ops.WriteResult{}, NewError(proto.CodeInvalidArgument, "a Worker does not accept control from itself")
	}
	var sessions []string
	if a.Sessions != nil {
		if len(*a.Sessions) == 0 {
			return ops.WriteResult{}, NewError(proto.CodeInvalidArgument, "sessions is empty: leave it out for all shared sessions")
		}
		for _, n := range *a.Sessions {
			if !ops.ValidSessionName(n) {
				return ops.WriteResult{}, NewError(proto.CodeInvalidArgument, "sessions names an invalid session")
			}
		}
		sessions = append([]string{}, *a.Sessions...)
	}
	c.AuditDetail = acceptDetail(a)
	var owners []string
	if p, ok := c.Args.(*ops.ControlSetAcceptArgs); ok {
		owners, err = pinned(h.pinA.take(p))
	} else {
		owners, err = pinned("", false)
	}
	if err != nil {
		return ops.WriteResult{}, err
	}
	_ = m
	if a.Accept {
		err = h.st.PutAcceptIfOwners(ctx, store.AcceptRow{TargetWorkerID: a.WorkerID, SourceWorkerID: a.SourceWorkerID,
			Sessions: sessions, CreatedBy: c.Identity.Actor, CreatedAt: time.Now().UnixMilli()},
			owners[0], owners[1])
	} else {
		// Not a plain delete: on a Worker that accepts all, a missing row
		// would widen the source to every shared session.
		_, err = h.st.RevokeAcceptIfOwners(ctx, store.AcceptRow{TargetWorkerID: a.WorkerID, SourceWorkerID: a.SourceWorkerID,
			CreatedBy: c.Identity.Actor, CreatedAt: time.Now().UnixMilli()}, owners[0], owners[1])
	}
	if err != nil {
		return ops.WriteResult{}, ownerErr(err)
	}
	return delivered(), nil
}

// workerArgResource decides a setter on the Worker it changes (the dispatcher
// audits it with that Worker as target).
// resource decides a setter on the Worker it changes (the dispatcher audits
// it with that Worker as target) and pins the owners of the Workers it names
// at decision time.
func (h controlHandlers) resource(_ authz.Principal, args any) (authz.Resource, error) {
	ctx, cancel := context.WithTimeout(context.Background(), trustCaptureTimeout)
	defer cancel()
	owner := func(id string) string {
		base, err := h.workers(ctx)
		if err != nil {
			return ""
		}
		for _, b := range base {
			if b.ID == id {
				return b.OwnerID
			}
		}
		return ""
	}
	var id string
	switch a := args.(type) {
	case *ops.ControlSetWorkerArgs:
		id = a.WorkerID
		h.pinW.put(a, owner(id))
	case *ops.ControlSetMasterArgs:
		id = a.WorkerID
		h.pinM.put(a, owner(id))
	case *ops.ControlSetAcceptArgs:
		id = a.WorkerID
		h.pinA.put(a, owner(id)+"\x00"+owner(a.SourceWorkerID))
	}
	if id == "" {
		return nil, NewError(proto.CodeInvalidArgument, "worker_id is required")
	}
	return authz.WorkerResource{ID: id}, nil
}

// pinned returns the owners captured with the decision; a Worker unknown then
// fails closed.
func pinned(owners string, ok bool) ([]string, error) {
	parts := strings.Split(owners, "\x00")
	if !ok {
		return nil, NewError(proto.CodeNotFound, "not found")
	}
	for _, p := range parts {
		if p == "" {
			return nil, NewError(proto.CodeNotFound, "not found")
		}
	}
	return parts, nil
}

// check lists the master sessions whose computed grant reaches the session.
// The owner limit and the target's sharing still apply at call time; they
// are named in the note.
func (h controlHandlers) check(ctx context.Context, c *Call, a ops.ControlCheckArgs) (ops.ControlCheckResult, error) {
	if err := requireHubWideScope(c.Principal); err != nil {
		return ops.ControlCheckResult{}, err
	}
	m, err := h.known(ctx, a.WorkerID)
	if err != nil {
		return ops.ControlCheckResult{}, err
	}
	masters, err := h.st.ListMasterSessions(ctx)
	if err != nil {
		return ops.ControlCheckResult{}, err
	}
	res := ops.ControlCheckResult{Controllers: []ops.ControlChain{}}
	for _, ms := range masters {
		// The computed grant decides, as at call time: it leaves out the
		// master's own Worker except for a migrated v0.8 grant that had it.
		g, err := h.st.SessionControl(ctx, ms.WorkerID, ms.SessionName)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return ops.ControlCheckResult{}, err
		}
		if grantCovers(g.TargetsJSON, a.WorkerID, a.SessionName) {
			res.Controllers = append(res.Controllers, ops.ControlChain{WorkerID: ms.WorkerID, Worker: m[ms.WorkerID].Name, SessionName: ms.SessionName})
		}
	}
	if h.bound != nil {
		attached, err := h.bound(ctx, a.WorkerID, a.SessionName)
		if err != nil {
			return ops.ControlCheckResult{}, err
		}
		res.Controllers = append(res.Controllers, attached...)
	}
	if len(res.Controllers) == 0 {
		res.Note = "No master session can control this session: nominate a master session on another Worker and let this Worker accept control from that Worker."
	} else {
		res.Note = "At call time the session must also be shared by its Worker, and the master Worker's owner must be allowed to reach this Worker."
	}
	return res, nil
}

func grantCovers(targetsJSON, worker, session string) bool {
	var ts []ops.KeyResource
	if err := json.Unmarshal([]byte(targetsJSON), &ts); err != nil {
		return false
	}
	for _, t := range ts {
		switch t.Kind {
		case ops.KeyResourceEverything:
			return true
		case ops.KeyResourceWorker:
			if t.WorkerID == worker {
				return true
			}
		case ops.KeyResourceSession:
			if t.WorkerID == worker && t.SessionName == session {
				return true
			}
		}
	}
	return false
}

// acceptDetail is the audit detail of control_set_accept: the source Worker
// and how far the acceptance reaches, never a session name.
func acceptDetail(a ops.ControlSetAcceptArgs) string {
	reach := "none"
	switch {
	case !a.Accept:
	case a.Sessions == nil:
		reach = "all"
	default:
		reach = "limited"
	}
	return "source_worker=" + a.SourceWorkerID + " accept=" + strconv.FormatBool(a.Accept) + " sessions=" + reach
}

// workerControlDetail is the audit detail of control_set_worker: the
// switches the call set.
func workerControlDetail(a ops.ControlSetWorkerArgs) string {
	var parts []string
	if a.UserManaged != nil {
		parts = append(parts, "user_managed="+strconv.FormatBool(*a.UserManaged))
	}
	if a.AcceptAll != nil {
		parts = append(parts, "accept_all="+strconv.FormatBool(*a.AcceptAll))
	}
	return strings.Join(parts, " ")
}
