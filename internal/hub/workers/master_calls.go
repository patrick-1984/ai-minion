package workers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
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

// masterAuth is one snapshot of what a lease may reach. scope is the reach of
// the nomination (nil for a lease that stands only on verified grants),
// direct the verified session grants of the incarnation with their digest
// key, and reach what results may show: scope and the shared targets of the
// grants that are usable now.
type masterAuth struct {
	session string
	scope   masterScope
	direct  []store.SessionGrantRow
	key     string
	reach   masterScope
}

// principal builds the caller of a master call. Without verified grants it is
// the bound master principal as before. With them it is the session principal
// that carries the grants; the nomination scope is its legacy part only when
// the incarnation is also a master.
func (a masterAuth) principal(workerID string) authz.Principal {
	if len(a.direct) == 0 {
		return authz.NewBoundMasterPrincipal(workerID, a.scope)
	}
	var legacy authz.Scope
	if a.scope != nil {
		legacy = a.scope
	}
	return authz.NewWorkerSessionPrincipal(workerID, a.session, legacy, directGrantsOf(a.direct))
}

func (s masterScope) add(workerID, name string) {
	names := s[workerID]
	if names == nil {
		names = map[string]bool{}
		s[workerID] = names
	}
	names[name] = true
}

func grantCaller(lease masterLease) store.VerifiedGrantCaller {
	return store.VerifiedGrantCaller{WorkerID: lease.row.WorkerID, OwnerID: lease.row.OwnerID, WorkerPub: lease.row.WorkerPub,
		SessionName: lease.row.SessionName, IncarnationID: lease.row.IncarnationID}
}

// masterAuthority is used before any routing and before returning reads.
// It does not authorize the final dispatch of a Worker operation.
func (m *MasterControl) masterAuthority(ctx context.Context, c Call) (masterAuth, *masterLane, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, lease, err := m.authorizeLocked(c)
	if err != nil {
		return masterAuth{}, nil, err
	}
	check, cancel := context.WithTimeout(ctx, l.expires.Sub(m.now()))
	defer cancel()
	out, err := m.authorityLocked(check, c, l, lease)
	if err != nil {
		return masterAuth{}, nil, err
	}
	if live, _, err := m.authorizeLocked(c); err != nil || live != l || check.Err() != nil {
		return masterAuth{}, nil, errMasterDenied
	}
	return out, l, nil
}

// authorityLocked reads the authority of the lease of lane l: its nomination
// scope and the verified session grants of its incarnation whose named
// target is still the terminal the grant was made for (sameSessions). The
// caller holds m.mu and checks the lane again afterwards.
func (m *MasterControl) authorityLocked(check context.Context, c Call, l *masterLane, lease masterLease) (masterAuth, error) {
	out := masterAuth{session: lease.row.SessionName, reach: masterScope{}}
	if lease.master {
		grants, err := m.db.BoundMasterTargets(check, lease.row, nil)
		if check.Err() != nil {
			return masterAuth{}, errMasterDenied
		}
		if err != nil {
			return masterAuth{}, masterAuthorityError(err)
		}
		out.scope = masterScope{}
		for _, g := range grants {
			if out.scope[g.WorkerID] == nil {
				out.scope[g.WorkerID] = map[string]bool{}
			}
			if g.Kind == ops.KeyResourceWorker {
				for _, name := range m.events.masterSharedNames(g.WorkerID) {
					out.scope.add(g.WorkerID, name)
					out.reach.add(g.WorkerID, name)
				}
			} else if m.events.masterShare(g.WorkerID, g.SessionName) {
				out.scope.add(g.WorkerID, g.SessionName)
				out.reach.add(g.WorkerID, g.SessionName)
			}
		}
	}
	// The verified session grants of this incarnation, read for every call.
	direct, err := m.verifiedGrantsLocked(check, lease, nil)
	if err != nil {
		return masterAuth{}, err
	}
	if !lease.master && len(direct) == 0 {
		// The last verified grant is gone (revoked or unbound): the lease
		// ends now, not at the next report.
		m.pushLocked(l)
		return masterAuth{}, errMasterDenied
	}
	now := m.now()
	out.direct, out.key = direct, directDigest(direct, now)
	for _, g := range direct {
		if g.SuspendedAt != nil || g.ExpiresAt != nil && !now.Before(time.UnixMilli(*g.ExpiresAt)) ||
			g.Scope() != store.GrantSourceEveryone && g.SourceOwnerID != l.info.OwnerID || g.TargetWorkerID == c.Worker.ID {
			continue
		}
		if g.TargetSession == "" {
			for _, name := range m.events.masterSharedNames(g.TargetWorkerID) {
				out.reach.add(g.TargetWorkerID, name)
			}
		} else if m.events.masterShare(g.TargetWorkerID, g.TargetSession) {
			out.reach.add(g.TargetWorkerID, g.TargetSession)
		}
	}
	return out, nil
}

// verifiedGrantsLocked reads the verified session grants of the lease (for
// target, as store.VerifiedSessionGrants) and keeps those whose named target
// is still the terminal the grant was made for: a removed session's grant
// never reaches a session that took its name, while the grant exists.
func (m *MasterControl) verifiedGrantsLocked(check context.Context, lease masterLease, target *store.MasterTarget) ([]store.SessionGrantRow, error) {
	rows, err := m.db.VerifiedSessionGrants(check, grantCaller(lease), target, m.now().UnixMilli())
	if check.Err() != nil {
		return nil, errMasterDenied
	}
	if err != nil {
		return nil, masterAuthorityError(err)
	}
	return sameSessionGrants(m.events, rows), nil
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

// admitMaster is the final fence before a dispatch. auth is the snapshot the
// principal of the call was built from: a lease that stands on a nomination
// is admitted by the nomination as before; when the nomination does not cover
// the destination (or there is none) the verified grants are read again in
// one store transaction and must be the ones of auth, with a usable one that
// covers a shared destination session. It returns the principal built from
// the authority read now, on which the Router decides the request again (the
// requested action, the owner of the calling Worker).
func (m *MasterControl) admitMaster(ctx context.Context, c Call, p *masterPending, on Online, session string, auth masterAuth) (authz.Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, lease, err := m.authorizeLocked(c)
	if err != nil {
		return authz.Principal{}, err
	}
	if on.WorkerID == c.Worker.ID || !m.liveMasterDestination(on) {
		return authz.Principal{}, errMasterDenied
	}
	var names []string
	if session == "" {
		names = m.events.masterSharedNames(on.WorkerID)
	} else if m.events.masterShare(on.WorkerID, session) {
		names = []string{session}
	}
	check, cancel := context.WithTimeout(ctx, l.expires.Sub(m.now()))
	defer cancel()
	target := &store.MasterTarget{WorkerID: on.WorkerID, OwnerID: on.OwnerID, Sessions: names}
	err = store.ErrMasterCandidate
	if lease.master {
		_, err = m.db.BoundMasterTargets(check, lease.row, target)
		if check.Err() != nil {
			return authz.Principal{}, errMasterDenied
		}
	}
	if errors.Is(err, store.ErrMasterCandidate) && len(auth.direct) != 0 {
		// Not reached by a nomination: the verified grants decide.
		var direct []store.SessionGrantRow
		direct, err = m.verifiedGrantsLocked(check, lease, target)
		if err == nil && directDigest(direct, m.now()) != auth.key {
			return authz.Principal{}, errMasterDenied
		}
	}
	if err != nil {
		return authz.Principal{}, masterAuthorityError(err)
	}
	fresh, err := m.authorityLocked(check, c, l, lease)
	if err != nil {
		return authz.Principal{}, err
	}
	if len(auth.direct) != 0 && fresh.key != auth.key {
		return authz.Principal{}, errMasterDenied
	}
	// A database writer can delay the proof. The source's clock and either
	// live link can change even while the lane lock is held.
	if live, _, err := m.authorizeLocked(c); err != nil || live != l || !m.liveMasterDestination(on) ||
		session != "" && !m.events.masterShare(on.WorkerID, session) || check.Err() != nil {
		return authz.Principal{}, errMasterDenied
	}
	// The DB decision committed; a later revoke does not synthesize an
	// outcome for this already admitted request.
	delete(l.pending, p)
	return fresh.principal(c.Worker.ID), nil
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
	auth, l, err := m.masterAuthority(ctx, c)
	if err != nil {
		return nil, err
	}
	principal := auth.principal(c.Worker.ID)
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
			if len(fresh.reach[w.ID]) != 0 && !w.Pending {
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
	if len(auth.direct) != 0 && req.WorkerID == c.Worker.ID {
		// The session principal of a verified grant holds the own-Worker
		// rule of a Worker identity; this path never serves the own Worker.
		return nil, errMasterDenied
	}
	req.Principal = principal
	// The session of the bound lease: the Hub verified the incarnation that
	// holds it, whether a nomination or verified grants authorize the call.
	req.source = verifiedSource(auth.session)
	if isFileOp(op) {
		// A transfer outlives this call: its re-decisions read the authority
		// of the lease again, so a revoked grant, an ended nomination or a
		// lost lease stops it (BRD 5.6).
		target, session := req.WorkerID, req.Session
		req.fileRefresh = func(ctx context.Context) (authz.Principal, error) {
			fresh, _, err := m.masterAuthority(ctx, c)
			if err != nil {
				return authz.Principal{}, err
			}
			if !fresh.reach[target][session] {
				return authz.Principal{}, errMasterDenied
			}
			return fresh.principal(c.Worker.ID), nil
		}
	}
	if op == ops.OpSendText && len(auth.direct) != 0 {
		// The bound prompt was validated as agent-only, submitted and
		// unforced: a terminal that holds message but not operate on the
		// target by a verified Message grant sends it as a prompt (D3).
		only, err := m.router.HoldsMessageOnly(ctx, principal, req.WorkerID, req.Session)
		if err != nil {
			return nil, err
		}
		req.PromptOnly = only
	}
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pending := &masterPending{incarnation: c.Master.IncarnationID, cancel: cancel}
	m.mu.Lock()
	live, lease, err := m.authorizeLocked(c)
	if err != nil || live != l {
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
	req.admit = func(ctx context.Context, on Online) (authz.Principal, error) {
		return m.admitMaster(ctx, c, pending, on, req.Session, auth)
	}
	// The verified grants that authorize the request at its final admission:
	// named by the audit event of a write, counted as used when it is
	// answered (as on the Claimed path).
	var grantIDs []string
	if len(auth.direct) != 0 {
		req.granted = func(ids []string) { grantIDs = ids }
	}
	// An app install stands on an apps_install grant or acceptance row: its
	// audit event names the one the final admission decided on, as the
	// transfer record of a file op does.
	var appBasis authz.FileBasis
	if op == ops.OpAppInstall {
		req.appBasis = func(b authz.FileBasis) { appBasis = b }
	}
	var withdrawn atomic.Bool
	if op != ops.OpSendText && op != ops.OpAppInstall && len(auth.direct) != 0 {
		// An admitted read leaves the pending set: a verified grant that
		// stops authorizing it ends it within the re-check interval (D5),
		// not when a stalled target answers. A write (send_text,
		// app_install) is not re-checked: its final admission decided it,
		// the target goes on with it whatever the Hub does next, and its
		// audit row records what the target answered (an app install that
		// was withdrawn here would still commit on the device, with no row).
		stop := m.recheckRead(qctx, c, req, func() { withdrawn.Store(true); cancel() })
		defer stop()
	}
	result, err := m.router.Call(qctx, req)
	if withdrawn.Load() {
		return nil, errMasterDenied
	}
	if err == nil {
		m.grantUse.Used(grantIDs...)
	}
	if op == ops.OpAppInstall {
		m.recordMasterWrite(ctx, c.Worker.ID, lease.row.SessionName, req, appAuditIDs(appBasis), result, err)
	}
	if isAppOp(op) {
		// Decided at the final admission on the apps_install flags of the
		// verified authority; the answer names no session to filter.
		return result, err
	}
	if op == ops.OpSendText {
		m.recordMasterWrite(ctx, c.Worker.ID, lease.row.SessionName, req, grantIDs, result, err)
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
			if fresh.reach[req.WorkerID][s.Name] {
				sessions = append(sessions, s)
			}
		}
		return ops.SessionsListResult{Sessions: sessions}, nil
	}
	if !fresh.reach[req.WorkerID][req.Session] {
		return nil, errMasterDenied
	}
	return result, nil
}

// masterReadRecheck is how often a running verified read is re-checked; D5
// ends a call within 30 s of the change that withdraws it.
const masterReadRecheck = DefaultReauthInterval

// recheckRead re-checks the authority behind a running read every interval
// of the MasterControl (masterReadRecheck by default) and calls withdraw when
// the lease or the grants no longer reach the destination of req, or when the
// read is no longer allowed on the principal built from them (the same
// decision as before the dispatch: a changed grant set or revision that does
// not give read any more). A re-check that cannot be made (a store error)
// withdraws the call at once: fail closed. The returned stop ends the
// re-checks and waits for them.
func (m *MasterControl) recheckRead(ctx context.Context, c Call, req CallRequest, withdraw func()) (stop func()) {
	every := m.readRecheck
	if every <= 0 {
		every = masterReadRecheck
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			fresh, _, err := m.masterAuthority(ctx, c)
			reached := req.Session == "" && len(fresh.reach[req.WorkerID]) != 0 ||
				req.Session != "" && fresh.reach[req.WorkerID][req.Session]
			if err == nil && !reached {
				err = errMasterDenied
			}
			if err == nil {
				err = m.router.Redecide(ctx, fresh.principal(c.Worker.ID), req)
			}
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				withdraw()
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
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
	case ops.OpFileList, ops.OpFilePull, ops.OpFilePush:
		// files.v1 (BRD 5.6): the file ops of a session caller whose
		// incarnation the Hub verified. The transfer service admits them on
		// the file flags of the verified grants and acceptances.
		t, args, err := masterFileArgs(op, raw)
		if err != nil {
			return r, err
		}
		r.WorkerID, r.Session, r.Args = t.WorkerID, t.SessionName, args
	case ops.OpAppInstall, ops.OpAppInstallStatus:
		// v0.25.0: an app install on another Android Worker, decided on the
		// apps_install flag of the verified grants and acceptances.
		var args ops.Validator
		var workerID string
		if op == ops.OpAppInstall {
			var a ops.AppInstallArgs
			if err := proto.DecodeMasterJSON(raw, &a); err != nil {
				return r, err
			}
			args, workerID, r.Args = a, a.WorkerID, a
		} else {
			var a ops.AppInstallStatusArgs
			if err := proto.DecodeMasterJSON(raw, &a); err != nil {
				return r, err
			}
			args, workerID, r.Args = a, a.WorkerID, a
		}
		if err := args.Validate(); err != nil {
			return r, proto.NewError(proto.CodeInvalidArgument, "invalid master app install arguments")
		}
		r.WorkerID = workerID
	default:
		return r, proto.NewError(proto.CodeForbidden, "operation is outside the master catalog")
	}
	if !ops.ValidUUIDv7(r.WorkerID) {
		return r, proto.NewError(proto.CodeInvalidArgument, "the master target must identify a Worker")
	}
	return r, nil
}

// masterFileArgs decodes the arguments of a file op of the master lane and
// returns its target and the arguments as a value.
func masterFileArgs(op string, raw json.RawMessage) (ops.Target, any, error) {
	var args ops.Validator
	switch op {
	case ops.OpFileList:
		args = &ops.FileListArgs{}
	case ops.OpFilePull:
		args = &ops.FilePullArgs{}
	default:
		args = &ops.FilePushArgs{}
	}
	if err := proto.DecodeMasterJSON(raw, args); err != nil {
		return ops.Target{}, nil, err
	}
	if err := args.Validate(); err != nil {
		return ops.Target{}, nil, proto.NewError(proto.CodeInvalidArgument, "invalid master file operation arguments")
	}
	switch a := args.(type) {
	case *ops.FileListArgs:
		return a.Target, *a, nil
	case *ops.FilePullArgs:
		return a.Target, *a, nil
	case *ops.FilePushArgs:
		return a.Target, *a, nil
	}
	return ops.Target{}, nil, proto.NewError(proto.CodeInternal, "unreadable master file operation")
}

// isAppOp reports whether op is an app install op of the master lane.
func isAppOp(op string) bool { return op == ops.OpAppInstall || op == ops.OpAppInstallStatus }

// isFileOp reports whether op is a file op of the master lane.
func isFileOp(op string) bool {
	return op == ops.OpFileList || op == ops.OpFilePull || op == ops.OpFilePush
}

// recordMasterWrite audits a bound master's write; session is the session
// name of the lease row the call ran under, grantIDs the verified grants that
// authorized it at its final admission.
func (m *MasterControl) recordMasterWrite(ctx context.Context, workerID, session string, req CallRequest, grantIDs []string, result any, err error) {
	if m.auditor == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), masterIOTimeout)
	defer cancel()
	ev := audit.Event{
		Actor: "worker:" + workerID, Action: req.Op, Target: req.WorkerID + ":" + req.Session,
		Outcome: writeAuditOutcome(result, err), Detail: "proof=bound", GrantID: strings.Join(grantIDs, ","),
	}
	if req.Op == ops.OpAppInstall {
		ev.Target = req.WorkerID
		if r, ok := appInstallResultOf(result); ok && err == nil {
			ev.Outcome = r.Outcome
			ev.Detail = proto.AppendDetail(ev.Detail, ops.AppInstallAuditDetail(r))
		}
	}
	if session != "" {
		ev.Source = workerID + ":" + session
	}
	if aerr := m.auditor.Record(ctx, ev); aerr != nil {
		slog.Default().Error("workers: audit write failed", "action", req.Op, "error", aerr.Error())
	}
}

// appAuditIDs is what the audit event of an app install names as its
// grant_id: the apps_install grants that decided it, else its acceptance row
// (as internal/hub/files names a transfer's basis).
func appAuditIDs(b authz.FileBasis) []string {
	if len(b.GrantIDs) != 0 {
		return b.GrantIDs
	}
	if b.AcceptanceID != "" {
		return []string{b.AcceptanceID}
	}
	return nil
}

// appInstallResultOf returns the app install result a routed call answered.
func appInstallResultOf(result any) (ops.AppInstallResult, bool) {
	switch v := result.(type) {
	case ops.AppInstallResult:
		return v, true
	case *ops.AppInstallResult:
		if v != nil {
			return *v, true
		}
	}
	return ops.AppInstallResult{}, false
}
