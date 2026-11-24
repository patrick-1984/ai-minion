package workers

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/audit"
	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// masterScope includes a Worker only when at least one shared session in
// that Worker is granted. Operations against an individual session still
// require its exact entry.
type masterScope map[string]map[string]bool

func (s masterScope) Actions(r authz.ResolvedResource) []ops.Action {
	names := s[r.WorkerID]
	if len(names) != 0 && (r.Kind == authz.ResourceWorker || r.Kind == authz.ResourceSession && names[r.SessionName]) {
		return authz.GrantActions()
	}
	return nil
}

// masterAuthority is used before any routing and before returning reads.
// It does not authorize the final dispatch of a Worker operation.
func (m *MasterControl) masterAuthority(ctx context.Context, c Call) (masterScope, *masterLane, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, lease, err := m.authorizeLocked(c)
	if err != nil {
		return nil, nil, err
	}
	check, cancel := context.WithTimeout(ctx, l.expires.Sub(m.now()))
	defer cancel()
	grants, err := m.db.BoundMasterTargets(check, lease.row, nil)
	if check.Err() != nil {
		return nil, nil, errMasterDenied
	}
	if err != nil {
		return nil, nil, masterAuthorityError(err)
	}
	out := masterScope{}
	for _, g := range grants {
		names := out[g.WorkerID]
		if names == nil {
			names = map[string]bool{}
			out[g.WorkerID] = names
		}
		if g.Kind == ops.KeyResourceWorker {
			for _, name := range m.events.masterSharedNames(g.WorkerID) {
				names[name] = true
			}
		} else if m.events.masterShare(g.WorkerID, g.SessionName) {
			names[g.SessionName] = true
		}
	}
	if live, _, err := m.authorizeLocked(c); err != nil || live != l || check.Err() != nil {
		return nil, nil, errMasterDenied
	}
	return out, l, nil
}

func masterAuthorityError(err error) error {
	if errors.Is(err, store.ErrMasterRevision) || errors.Is(err, store.ErrMasterCandidate) || errors.Is(err, store.ErrWorkerOwnerChanged) {
		return errMasterDenied
	}
	if errors.Is(err, context.Canceled) {
		return errMasterDenied
	}
	return err
}

func (m *MasterControl) admitMaster(ctx context.Context, c Call, p *masterPending, on Online, session string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, lease, err := m.authorizeLocked(c)
	if err != nil {
		return err
	}
	if on.WorkerID == c.Worker.ID || !m.liveMasterDestination(on) {
		return errMasterDenied
	}
	var names []string
	if session == "" {
		names = m.events.masterSharedNames(on.WorkerID)
	} else if m.events.masterShare(on.WorkerID, session) {
		names = []string{session}
	}
	check, cancel := context.WithTimeout(ctx, l.expires.Sub(m.now()))
	defer cancel()
	_, err = m.db.BoundMasterTargets(check, lease.row, &store.MasterTarget{WorkerID: on.WorkerID, OwnerID: on.OwnerID, Sessions: names})
	if check.Err() != nil {
		return errMasterDenied
	}
	if err != nil {
		return masterAuthorityError(err)
	}
	// A database writer can delay the proof. The source's clock and either
	// live link can change even while the lane lock is held.
	if live, _, err := m.authorizeLocked(c); err != nil || live != l || !m.liveMasterDestination(on) ||
		session != "" && !m.events.masterShare(on.WorkerID, session) || check.Err() != nil {
		return errMasterDenied
	}
	// The DB decision committed; a later revoke does not synthesize an
	// outcome for this already admitted request.
	delete(l.pending, p)
	return nil
}

func (m *MasterControl) liveMasterDestination(on Online) bool {
	target, ok := m.registry.Get(on.WorkerID)
	return ok && !on.Pending && !target.Pending && target.OwnerID == on.OwnerID &&
		m.registry.hasLiveLink(on.WorkerID, on.TranscriptHash)
}

func (m *MasterControl) serveMasterCall(ctx context.Context, c Call) {
	if c.Master == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultResultTimeout)
	defer cancel()
	// A lost caller can cancel work that has not been admitted. The Router
	// still reports partial/unknown when the destination may have seen it.
	caller := make(chan struct{})
	go func() {
		defer close(caller)
		_, _ = c.Stream.ReadFrame()
		cancel()
	}()
	defer func() {
		_ = c.Stream.SetReadDeadline(time.Now())
		<-caller
	}()

	result, err := m.runMasterCall(ctx, c)
	_ = c.Stream.SetWriteDeadline(time.Now().Add(masterIOTimeout))
	if err != nil {
		var pe *proto.Error
		if !errors.As(err, &pe) {
			pe = proto.NewError(proto.CodeInternal, "the Hub could not complete the master operation")
		}
		_ = c.Stream.WriteFrame(pe.Frame())
		return
	}
	frame := OpResult{Type: proto.TypeOpResult}
	frame.Result, err = json.Marshal(result)
	if err != nil {
		_ = c.Stream.WriteFrame(proto.NewError(proto.CodeInternal, "the master operation returned an unreadable result").Frame())
		return
	}
	if len(frame.Result) > 64<<10 {
		_ = c.Stream.WriteFrame(proto.NewError(proto.CodeFrameTooLarge, "the master operation result exceeds the response limit").Frame())
		return
	}
	_ = c.Stream.WriteFrame(frame)
}

func (m *MasterControl) runMasterCall(ctx context.Context, c Call) (any, error) {
	scope, l, err := m.masterAuthority(ctx, c)
	if err != nil {
		return nil, err
	}
	principal := authz.NewBoundMasterPrincipal(c.Worker.ID, scope)
	op, raw := c.Master.Operation, c.Master.Arguments
	if op == ops.OpWorkersList {
		var args ops.WorkersListArgs
		if err := proto.DecodeMasterJSON(raw, &args); err != nil {
			return nil, err
		}
		list, err := m.router.ListWorkers(ctx, principal, true)
		if err != nil {
			return nil, err
		}
		fresh, _, err := m.masterAuthority(ctx, c)
		if err != nil {
			return nil, err
		}
		out := make([]ops.WorkerInfo, 0, len(list))
		notListed := 0
		for _, w := range list {
			if len(fresh[w.ID]) != 0 && !w.Pending {
				out = append(out, w)
			} else if !w.Pending {
				notListed++
			}
		}
		return ops.WorkersListResult{Workers: out, NotListed: notListed}, nil
	}

	req, err := masterRoute(c.RequestID, op, raw)
	if err != nil {
		return nil, err
	}
	req.Principal = principal
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pending := &masterPending{incarnation: c.Master.IncarnationID, cancel: cancel}
	m.mu.Lock()
	if live, _, err := m.authorizeLocked(c); err != nil || live != l {
		m.mu.Unlock()
		return nil, errMasterDenied
	}
	l.pending[pending] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(l.pending, pending)
		m.mu.Unlock()
	}()
	req.admit = func(ctx context.Context, on Online) error { return m.admitMaster(ctx, c, pending, on, req.Session) }
	result, err := m.router.Call(qctx, req)
	if op == ops.OpSendText {
		m.recordMasterWrite(ctx, c.Worker.ID, req, result, err)
		if errors.Is(err, context.Canceled) {
			return nil, errMasterDenied
		}
		return result, err
	}
	if err != nil {
		return nil, err
	}
	fresh, _, err := m.masterAuthority(ctx, c)
	if err != nil {
		return nil, err
	}
	if op == ops.OpSessionsList {
		out, ok := result.(ops.SessionsListResult)
		if !ok {
			return nil, proto.NewError(proto.CodeInternal, "unreadable master session listing")
		}
		sessions := make([]ops.SessionInfo, 0, len(out.Sessions))
		for _, s := range out.Sessions {
			if fresh[req.WorkerID][s.Name] {
				sessions = append(sessions, s)
			}
		}
		return ops.SessionsListResult{Sessions: sessions}, nil
	}
	if !fresh[req.WorkerID][req.Session] {
		return nil, errMasterDenied
	}
	return result, nil
}

func masterRoute(requestID, op string, raw json.RawMessage) (CallRequest, error) {
	r := CallRequest{Op: op}
	switch op {
	case ops.OpSessionsList:
		var a struct {
			WorkerID string `json:"worker_id"`
		}
		if err := proto.DecodeMasterJSON(raw, &a); err != nil {
			return r, err
		}
		r.WorkerID, r.Args = a.WorkerID, ops.SessionsListArgs{}
	case ops.OpSessionSnapshot:
		var a ops.SessionSnapshotArgs
		if err := proto.DecodeMasterJSON(raw, &a); err != nil {
			return r, err
		}
		if err := a.Validate(); err != nil {
			return r, proto.NewError(proto.CodeInvalidArgument, "invalid master snapshot target")
		}
		r.WorkerID, r.Session, r.Args = a.Target.WorkerID, a.Target.SessionName, a
	case ops.OpSendText:
		var a struct {
			ops.WriteArgs
			Target ops.Target `json:"target"`
			Text   string     `json:"text"`
			Submit bool       `json:"submit"`
		}
		if err := proto.DecodeMasterJSON(raw, &a); err != nil {
			return r, err
		}
		prompt := ops.SendTextArgs{WriteArgs: a.WriteArgs, Target: a.Target, Text: a.Text, Submit: a.Submit, AgentOnly: true}
		if prompt.RequestID != requestID || prompt.Force || !prompt.Submit || prompt.Validate() != nil {
			return r, proto.NewError(proto.CodeInvalidArgument, "a master prompt requires normal submission and a matching request ID")
		}
		r.WorkerID, r.Session, r.Args = a.Target.WorkerID, a.Target.SessionName, prompt
	default:
		return r, proto.NewError(proto.CodeForbidden, "operation is outside the master catalog")
	}
	if !ops.ValidUUIDv7(r.WorkerID) {
		return r, proto.NewError(proto.CodeInvalidArgument, "the master target must identify a Worker")
	}
	return r, nil
}

func (m *MasterControl) recordMasterWrite(ctx context.Context, workerID string, req CallRequest, result any, err error) {
	if m.auditor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), masterIOTimeout)
	defer cancel()
	_ = m.auditor.Record(ctx, audit.Event{
		Actor: "worker:" + workerID, Action: req.Op, Target: req.WorkerID + ":" + req.Session,
		Outcome: writeAuditOutcome(result, err),
	})
}
