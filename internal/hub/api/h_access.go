package api

// The access matrix: a read-only view of who reaches what on today's model
// (master sessions and the Workers that accept them). Nothing decides from
// it. Unlike control_check it applies the owner rule and the target's
// sharing, and names the first layer that blocks a pair.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/authz"
	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// AccessWorker is what the matrix knows of one Worker.
type AccessWorker struct {
	ID, Name, Owner, OwnerID string
	// OwnerAdmin: the owner is an active administrator.
	OwnerAdmin bool
	Pending    bool
	Online     bool
	// Sessions are the live session names the Hub lists for the Worker.
	Sessions []string
}

// AccessFacts is one reading of the durable tables plus the live sessions.
type AccessFacts struct {
	Workers []AccessWorker
	Masters []store.MasterRow
	Bound   []store.BoundMasterRow
	Accepts []store.AcceptRow
	Control map[string]store.WorkerControlRow
	Shares  map[string]ops.ShareDecl
	// Grants are the direct session grants (every row, also suspended and
	// expired ones); Admins the ids of the active administrators.
	Grants []store.SessionGrantRow
	Admins map[string]bool
	// Now (Unix milliseconds) decides expiry; 0 means the current time.
	Now int64
}

// RegisterAccessMatrix registers access_matrix. It is decided on the caller's
// own user resource (every signed-in user may ask); the result is limited to
// the Workers the caller owns unless the caller is an administrator.
func RegisterAccessMatrix(r *Registry, facts func(context.Context) (AccessFacts, error),
	isAdmin func(ctx context.Context, userID string) (bool, error)) error {
	if facts == nil || isAdmin == nil {
		return errors.New("api: the access matrix needs its facts")
	}
	return HandleTyped(r, ops.OpAccessMatrix, func(ctx context.Context, c *Call, a ops.AccessMatrixArgs) (ops.AccessMatrixResult, error) {
		if err := requireHubWideScope(c.Principal); err != nil {
			return ops.AccessMatrixResult{}, err
		}
		f, err := facts(ctx)
		if err != nil {
			return ops.AccessMatrixResult{}, err
		}
		user := c.Principal.UserID
		if c.Principal.Kind == authz.KindWorker {
			user = ""
		}
		all := false
		if user != "" {
			if all, err = isAdmin(ctx, user); err != nil {
				return ops.AccessMatrixResult{}, err
			}
		}
		res := AccessMatrix(f, user, all, a.WorkerID)
		res.AsOf = ops.Timestamp(time.Now())
		return res, nil
	}, WithResource(func(p authz.Principal, _ any) (authz.Resource, error) {
		// The optional worker_id is a filter, not the resource: the result is
		// limited per Worker below.
		if p.UserID == "" {
			return nil, NewError(proto.CodeForbidden, "not allowed")
		}
		return authz.UserResource{ID: p.UserID}, nil
	}))
}

// RegisterAccessExplain registers access_explain with the accepted action and
// the visibility rule of access_matrix.
func RegisterAccessExplain(r *Registry, facts func(context.Context) (AccessFacts, error),
	isAdmin func(ctx context.Context, userID string) (bool, error)) error {
	if facts == nil || isAdmin == nil {
		return errors.New("api: the access explainer needs its facts")
	}
	return HandleTyped(r, ops.OpAccessExplain, func(ctx context.Context, c *Call, a ops.AccessExplainArgs) (ops.AccessExplainResult, error) {
		var none ops.AccessExplainResult
		if err := requireHubWideScope(c.Principal); err != nil {
			return none, err
		}
		if a.SourceWorkerID == "" || a.SourceSession == "" || a.TargetWorkerID == "" || a.TargetSession == "" {
			return none, NewError(proto.CodeInvalidArgument, "source and target need a Worker and a session")
		}
		f, err := facts(ctx)
		if err != nil {
			return none, err
		}
		user := c.Principal.UserID
		if c.Principal.Kind == authz.KindWorker {
			user = ""
		}
		all := false
		if user != "" {
			if all, err = isAdmin(ctx, user); err != nil {
				return none, err
			}
		}
		res, ok := AccessExplain(f, user, all, a)
		if !ok {
			return none, NewError(proto.CodeNotFound, "no such pair")
		}
		return res, nil
	}, WithResource(func(p authz.Principal, _ any) (authz.Resource, error) {
		if p.UserID == "" {
			return nil, NewError(proto.CodeForbidden, "not allowed")
		}
		return authz.UserResource{ID: p.UserID}, nil
	}))
}

type accessMaster struct {
	worker, session, proof string
	limit                  func(worker, session string) bool
}

// accessEval is one set of facts prepared for the matrix and the explainer.
// Both ask pair, so they cannot disagree.
type accessEval struct {
	f       AccessFacts
	now     int64
	workers map[string]AccessWorker
	accepts map[[2]string]store.AcceptRow
	masters map[[2]string]accessMaster
	grants  map[[2]string][]store.SessionGrantRow
}

// accessVerdict is what one grant or one master reach gives for one pair.
type accessVerdict struct {
	Covered  bool
	Via      string // "master" or the grant id
	Granted  string
	InEffect string
	Blocked  string // the first failing layer, in the matrix order
	// The layers one by one ("" = passes), for the explainer.
	Authority, Time, Sharing, Pending, Offline string
}

func newAccessEval(f AccessFacts) *accessEval {
	e := &accessEval{f: f, now: f.Now, workers: map[string]AccessWorker{}, accepts: map[[2]string]store.AcceptRow{},
		masters: map[[2]string]accessMaster{}, grants: map[[2]string][]store.SessionGrantRow{}}
	if e.now == 0 {
		e.now = time.Now().UnixMilli()
	}
	for _, w := range f.Workers {
		e.workers[w.ID] = w
	}
	// An explicit acceptance row wins over accept_all, also when it names no
	// session at all.
	for _, a := range f.Accepts {
		e.accepts[[2]string{a.SourceWorkerID, a.TargetWorkerID}] = a
	}
	// Master sessions: session-bound nominations (verified) and name-only
	// rows (claimed). A bound row replaces the name-only row of its name.
	for _, b := range f.Bound {
		if b.Active {
			e.masters[[2]string{b.WorkerID, b.SessionName}] = accessMaster{b.WorkerID, b.SessionName, ops.AccessProofVerified, legacyLimit(b.LegacyTargetsJSON)}
		}
	}
	for _, m := range f.Masters {
		if _, ok := e.masters[[2]string{m.WorkerID, m.SessionName}]; !ok {
			e.masters[[2]string{m.WorkerID, m.SessionName}] = accessMaster{m.WorkerID, m.SessionName, ops.AccessProofClaimed, legacyLimit(m.LegacyTargetsJSON)}
		}
	}
	// Direct grants. A grant bound to one incarnation of the source is not
	// read by the call path yet (store.SessionGrantsFor), so it is no reach.
	for _, g := range f.Grants {
		if g.SourceIncarnation == "" && g.SourceWorkerID != g.TargetWorkerID {
			k := [2]string{g.SourceWorkerID, g.SourceSession}
			e.grants[k] = append(e.grants[k], g)
		}
	}
	return e
}

func (e *accessEval) shared(worker, session string) bool {
	d, ok := e.f.Shares[worker]
	return ok && d.Rule.Includes(session)
}

func (e *accessEval) accepted(source, target, session string) bool {
	if a, ok := e.accepts[[2]string{source, target}]; ok {
		return a.Sessions == nil || slices.Contains(a.Sessions, session)
	}
	return e.f.Control[target].AcceptAll
}

// proof tells how the Hub knows a source session.
func (e *accessEval) proof(worker, session string) string {
	if m, ok := e.masters[[2]string{worker, session}]; ok {
		return m.proof
	}
	return ops.AccessProofClaimed
}

func accessGrantLevel(level int) string {
	switch level {
	case 1:
		return ops.AccessLevelView
	case 2:
		return ops.AccessLevelMessage
	case 3:
		return ops.AccessLevelControl
	}
	return ""
}

func accessRank(level string) int {
	switch level {
	case ops.AccessLevelControl:
		return 3
	case ops.AccessLevelMessage:
		return 2
	case ops.AccessLevelView:
		return 1
	}
	return 0
}

// workerLayers fills the layers both kinds of reach share.
func (e *accessEval) workerLayers(v *accessVerdict, src, t AccessWorker, session, name string, sourceShare bool) {
	if src.Pending || t.Pending {
		v.Pending = ops.AccessBlockedWorkerPending
	}
	if !e.shared(t.ID, name) || sourceShare && !e.shared(src.ID, session) {
		v.Sharing = ops.AccessBlockedNotShared
	}
	if !t.Online || !src.Online {
		v.Offline = ops.AccessBlockedWorkerOffline
	}
}

// settle names the first failing layer in the matrix order.
func (v *accessVerdict) settle(order ...string) {
	v.Covered = true
	for _, b := range order {
		if b != "" {
			v.Blocked = b
			return
		}
	}
	v.InEffect = v.Granted
}

func (e *accessEval) masterVerdict(m accessMaster, src, t AccessWorker, name string) accessVerdict {
	v := accessVerdict{Via: ops.AccessViaMaster, Granted: ops.AccessLevelControl}
	if !src.OwnerAdmin && src.OwnerID != t.OwnerID {
		v.Authority = ops.AccessBlockedOwnerRule
	}
	e.workerLayers(&v, src, t, m.session, name, m.proof == ops.AccessProofVerified)
	v.settle(v.Authority, v.Pending, v.Sharing, v.Offline)
	return v
}

func (e *accessEval) grantVerdict(g store.SessionGrantRow, src, t AccessWorker, name string) accessVerdict {
	v := accessVerdict{Via: g.ID, Granted: accessGrantLevel(g.Level)}
	switch {
	case g.SuspendedAt != nil:
		v.Time = ops.AccessBlockedSuspended
	case g.ExpiresAt != nil && *g.ExpiresAt <= e.now:
		v.Time = ops.AccessBlockedExpired
	}
	switch g.Authority {
	case store.GrantAuthorityAdmin:
		if !e.f.Admins[g.GrantedBy] {
			v.Authority = ops.AccessBlockedAuthority
		}
	default:
		// An owner grant holds while both Workers still share the owner it
		// was written for.
		if src.OwnerID != t.OwnerID || g.SourceOwnerID != src.OwnerID || g.TargetOwnerID != t.OwnerID {
			v.Authority = ops.AccessBlockedAuthority
		}
	}
	e.workerLayers(&v, src, t, g.SourceSession, name, false)
	v.settle(v.Time, v.Authority, v.Pending, v.Sharing, v.Offline)
	return v
}

// pair is the one evaluator: the best reach session on src has to the live
// session name on t. A master reach and a grant may both cover the pair; the
// higher level in effect wins and, on a tie, the master.
func (e *accessEval) pair(src AccessWorker, session string, t AccessWorker, name string) accessVerdict {
	var best accessVerdict
	// Never the same Worker: its sessions reach each other anyway.
	if src.ID == t.ID || !slices.Contains(t.Sessions, name) {
		return best
	}
	if m, ok := e.masters[[2]string{src.ID, session}]; ok && e.accepted(src.ID, t.ID, name) && m.limit(t.ID, name) {
		best = e.masterVerdict(m, src, t, name)
	}
	for _, g := range e.grants[[2]string{src.ID, session}] {
		if g.TargetWorkerID != t.ID || g.TargetSession != "" && g.TargetSession != name {
			continue
		}
		if v := e.grantVerdict(g, src, t, name); !best.Covered || accessRank(v.InEffect) > accessRank(best.InEffect) {
			best = v
		}
	}
	return best
}

func accessMask(owner string) string { return "a session of " + owner }

// AccessMatrix computes the matrix from one set of facts for a viewer: all
// sees every pair, otherwise only the pairs with a Worker owned by viewer at
// either end; a session of another owner is then shown without its name.
// only, when set, keeps the pairs with that Worker at either end.
func AccessMatrix(f AccessFacts, viewer string, all bool, only string) ops.AccessMatrixResult {
	res := ops.AccessMatrixResult{Sources: []ops.AccessSource{}, Targets: []ops.AccessTarget{}, Cells: []ops.AccessCell{}}
	e := newAccessEval(f)
	own := func(w AccessWorker) bool { return all || viewer != "" && w.OwnerID == viewer }

	// Sources: every master session and every session that holds a grant.
	type source struct {
		worker, session string
		master          bool
	}
	var sources []source
	for k := range e.masters {
		sources = append(sources, source{k[0], k[1], true})
	}
	for k := range e.grants {
		if _, ok := e.masters[k]; !ok {
			sources = append(sources, source{k[0], k[1], false})
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		a, b := e.workers[sources[i].worker], e.workers[sources[j].worker]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return ops.NaturalCompare(sources[i].session, sources[j].session) < 0
	})
	targets := append([]AccessWorker{}, f.Workers...)
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].Name != targets[j].Name {
			return targets[i].Name < targets[j].Name
		}
		return targets[i].ID < targets[j].ID
	})

	type found struct {
		worker, session string
		cell            ops.AccessCell
	}
	var covered []found
	used := map[[2]string]bool{}
	for _, s := range sources {
		src, ok := e.workers[s.worker]
		if !ok {
			continue
		}
		var cells []found
		for _, t := range targets {
			if !own(src) && !own(t) || only != "" && only != src.ID && only != t.ID {
				continue
			}
			for _, name := range t.Sessions {
				v := e.pair(src, s.session, t, name)
				if !v.Covered {
					continue
				}
				cells = append(cells, found{t.ID, name, ops.AccessCell{Source: len(res.Sources), Granted: v.Granted,
					InEffect: v.InEffect, Via: v.Via, BlockedBy: v.Blocked}})
			}
		}
		// A source that covers nothing is still a row for its owner, unless
		// a filter asked for another Worker.
		if len(cells) == 0 && (!own(src) || only != "" && only != src.ID) {
			continue
		}
		row := ops.AccessSource{WorkerID: src.ID, WorkerName: src.Name, Owner: src.Owner,
			Session: s.session, Proof: e.proof(src.ID, s.session), Master: s.master}
		if !own(src) {
			row.Session, row.Masked = accessMask(src.Owner), true
		}
		res.Sources = append(res.Sources, row)
		for _, c := range cells {
			used[[2]string{c.worker, c.session}] = true
		}
		covered = append(covered, cells...)
	}
	// Targets are the sessions some source covers, grouped by Worker.
	index := map[[2]string]int{}
	for _, t := range targets {
		for _, name := range t.Sessions {
			key := [2]string{t.ID, name}
			if _, dup := index[key]; used[key] && !dup {
				index[key] = len(res.Targets)
				tg := ops.AccessTarget{WorkerID: t.ID, WorkerName: t.Name, Owner: t.Owner, Online: t.Online, Session: name}
				if !own(t) {
					tg.Session, tg.Masked = accessMask(t.Owner), true
				}
				res.Targets = append(res.Targets, tg)
			}
		}
	}
	for _, c := range covered {
		c.cell.Target = index[[2]string{c.worker, c.session}]
		res.Cells = append(res.Cells, c.cell)
	}
	return res
}

func accessLevelWord(level string) string {
	switch level {
	case ops.AccessLevelControl:
		return "Control"
	case ops.AccessLevelMessage:
		return "Message"
	case ops.AccessLevelView:
		return "View"
	}
	return "nothing"
}

// AccessExplain answers one pair from the same evaluator as the matrix. It
// is false when a Worker is unknown or the viewer owns neither end.
func AccessExplain(f AccessFacts, viewer string, all bool, a ops.AccessExplainArgs) (ops.AccessExplainResult, bool) {
	e := newAccessEval(f)
	src, ok := e.workers[a.SourceWorkerID]
	t, ok2 := e.workers[a.TargetWorkerID]
	own := func(w AccessWorker) bool { return all || viewer != "" && w.OwnerID == viewer }
	if !ok || !ok2 || !own(src) && !own(t) {
		return ops.AccessExplainResult{}, false
	}
	_, master := e.masters[[2]string{src.ID, a.SourceSession}]
	proof := e.proof(src.ID, a.SourceSession)
	res := ops.AccessExplainResult{
		Source: ops.AccessSource{WorkerID: src.ID, WorkerName: src.Name, Owner: src.Owner, Session: a.SourceSession, Proof: proof, Master: master},
		Target: ops.AccessTarget{WorkerID: t.ID, WorkerName: t.Name, Owner: t.Owner, Online: t.Online, Session: a.TargetSession},
	}
	if !own(src) {
		res.Source.Session, res.Source.Masked = accessMask(src.Owner), true
	}
	if !own(t) {
		res.Target.Session, res.Target.Masked = accessMask(t.Owner), true
	}
	v := e.pair(src, a.SourceSession, t, a.TargetSession)
	res.InEffect, res.Via = v.InEffect, v.Via

	failed := false
	add := func(id string, fail bool, pass, failText string) {
		c := ops.AccessCheck{ID: id, State: ops.AccessCheckPass, Text: pass}
		switch {
		case failed:
			c.State, c.Text = ops.AccessCheckSkip, "not evaluated: an earlier check failed"
		case fail:
			c.State, c.Text = ops.AccessCheckFail, failText
			failed = true
		}
		res.Checks = append(res.Checks, c)
	}
	caller := "the source session is named by its Worker (Claimed)"
	if proof == ops.AccessProofVerified {
		caller = "the source session is a kernel-proven terminal (Verified)"
	}
	add(ops.AccessCheckCaller, false, caller, "")
	by := "grant " + v.Via + " gives " + accessLevelWord(v.Granted)
	if v.Via == ops.AccessViaMaster {
		by = "master reach gives Control: the target Worker accepts this master session"
	}
	none := "no grant and no master reach covers this pair"
	switch {
	case src.ID == t.ID:
		none += ": sessions of one Worker reach each other without a grant"
	case !slices.Contains(t.Sessions, a.TargetSession):
		none += ": the target Worker does not list a session of that name now"
	}
	add(ops.AccessCheckGrant, !v.Covered, by, none)
	authority, authorityFail := "an active administrator decided this grant", "the administrator who decided this grant is no longer an active administrator"
	if v.Via == ops.AccessViaMaster {
		authority, authorityFail = "the owner of the source Worker may reach this Worker", "the owner of the source Worker is neither an administrator nor the owner of the target Worker"
	} else if v.Authority == "" && !f.Admins[grantBy(e, src.ID, a.SourceSession, v.Via)] {
		authority = "both Workers still belong to the owner who decided this grant"
	} else if v.Authority != "" && src.OwnerID != t.OwnerID && grantAuthority(e, src.ID, a.SourceSession, v.Via) != store.GrantAuthorityAdmin {
		authorityFail = "the two Workers no longer belong to the owner who decided this grant"
	} else if v.Authority != "" && grantAuthority(e, src.ID, a.SourceSession, v.Via) != store.GrantAuthorityAdmin {
		authorityFail = "a Worker changed its owner since this grant was decided"
	}
	add(ops.AccessCheckAuthority, v.Authority != "", authority, authorityFail)
	timeFail := "the grant is suspended until an administrator approves it again"
	if v.Time == ops.AccessBlockedExpired {
		timeFail = "the grant has expired"
	}
	add(ops.AccessCheckTime, v.Time != "", "not expired and not suspended", timeFail)
	add(ops.AccessCheckSharing, v.Sharing != "", "the target Worker shares the session", "the session is not shared by its Worker")
	workersFail := "a Worker is waiting for approval"
	if v.Pending == "" {
		workersFail = "a Worker is offline"
	}
	add(ops.AccessCheckWorkers, v.Pending != "" || v.Offline != "", "both Workers are approved and connected", workersFail)
	res.Checks = append(res.Checks, ops.AccessCheck{ID: ops.AccessCheckPreviewOnly, State: ops.AccessCheckUnknown,
		Text: "the target Worker decides this and does not report it"})
	return res, true
}

func grantOf(e *accessEval, worker, session, id string) store.SessionGrantRow {
	for _, g := range e.grants[[2]string{worker, session}] {
		if g.ID == id {
			return g
		}
	}
	return store.SessionGrantRow{}
}

func grantBy(e *accessEval, worker, session, id string) string {
	return grantOf(e, worker, session, id).GrantedBy
}

func grantAuthority(e *accessEval, worker, session, id string) string {
	return grantOf(e, worker, session, id).Authority
}

// legacyLimit is the extra limit of a migrated v0.8 grant: nothing it did not
// name is reachable, and an unreadable limit allows nothing.
func legacyLimit(legacy string) func(worker, session string) bool {
	if legacy == "" {
		return func(string, string) bool { return true }
	}
	var lim []ops.KeyResource
	if err := json.Unmarshal([]byte(legacy), &lim); err != nil {
		return func(string, string) bool { return false }
	}
	return func(worker, session string) bool {
		for _, l := range lim {
			switch l.Kind {
			case ops.KeyResourceEverything:
				return true
			case ops.KeyResourceWorker:
				if l.WorkerID == worker {
					return true
				}
			case ops.KeyResourceSession:
				if l.WorkerID == worker && l.SessionName == session {
					return true
				}
			}
		}
		return false
	}
}
