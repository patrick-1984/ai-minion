package api

// The access matrix: a read-only view of who reaches what on today's model
// (master sessions and the Workers that accept them). Nothing decides from
// it. Unlike control_check it applies the owner rule and the target's
// sharing, and names the first layer that blocks a pair.

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
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
	// OwnerAdmin: the owner is an active administrator. OwnerActive: the
	// owner is an active account with a role; the Hub refuses every call of
	// a Worker whose owner is not.
	OwnerAdmin, OwnerActive bool
	Pending                 bool
	Online                  bool
	// AgentOnly: the connected Worker announces send_text.agent_only, which
	// a Message level needs to deliver its prompt.
	AgentOnly bool
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
	// Pending are the dispatched, unsettled share restrictions: a session one
	// of them excludes is not shared (store requireMasterShare).
	Pending []store.PendingShare
	// Grants are the direct session grants (every row, also suspended and
	// expired ones); Admins the ids of the active administrators.
	Grants []store.SessionGrantRow
	Admins map[string]bool
	// Attached tells, per (Worker, session) of a session-bound master,
	// whether its terminal holds a lease for the nominated incarnation now.
	// A missing entry is "not attached": the broker would refuse.
	Attached map[[2]string]bool
	// Leases tells, per (Worker, session, incarnation), whether the terminal
	// incarnation is a live, eligible candidate of its Worker's master lane
	// now: the lane then holds a lease for it as soon as a verified grant
	// (source_incarnation) names it. A missing entry is "no lease": the
	// master broker refuses every call of that incarnation.
	Leases map[[3]string]bool
	// Now (Unix milliseconds) decides expiry; 0 means the current time.
	Now int64
}

// AccessLive is the part of the facts that is not in the store, by Worker id
// (Attached: by Worker id and session name; Leases: by Worker id, session
// name and incarnation id).
type AccessLive struct {
	Online    map[string]bool
	AgentOnly map[string]bool
	Sessions  map[string][]string
	Attached  map[[2]string]bool
	Leases    map[[3]string]bool
}

// AccessFactsFrom joins one store snapshot with the live state.
func AccessFactsFrom(s store.AccessSnapshot, live AccessLive) AccessFacts {
	f := AccessFacts{Masters: s.Masters, Bound: s.Bound, Accepts: s.Accepts, Control: s.Control, Shares: s.Shares,
		Pending: s.Pending, Grants: s.Grants, Admins: map[string]bool{}, Attached: live.Attached, Leases: live.Leases}
	for id, u := range s.Users {
		if u.Admin {
			f.Admins[id] = true
		}
	}
	for _, w := range s.Workers {
		u := s.Users[w.OwnerID]
		f.Workers = append(f.Workers, AccessWorker{ID: w.ID, Name: w.Name, Owner: u.Username, OwnerID: w.OwnerID,
			OwnerAdmin: u.Admin, OwnerActive: u.Active, Pending: w.Pending, Online: live.Online[w.ID],
			AgentOnly: live.AgentOnly[w.ID], Sessions: live.Sessions[w.ID]})
	}
	return f
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
	// owner is the owner a session-bound nomination was decided under.
	owner string
	limit func(worker, session string) bool
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
	// SharingWorker and PendingWorker name the Worker a failing layer is on.
	SharingWorker, PendingWorker string
	// Limited: a Message level that works as View, because the target Worker
	// cannot deliver a prompt to an agent pane only.
	Limited bool
	// Proof is how the route that gives this verdict knows the source
	// session: Verified for a session-bound master and for a grant bound to
	// one terminal incarnation (both served by the master broker), Claimed
	// for a name-only master and a grant by name.
	Proof string
	// grant is the row behind a grant verdict.
	grant *store.SessionGrantRow
}

// authorized is the level the Hub's decision (authz) gives, before the layers
// of the Workers.
func (v accessVerdict) authorized() string {
	if !v.Covered || v.Authority != "" || v.Time != "" {
		return ""
	}
	return v.Granted
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
			e.masters[[2]string{b.WorkerID, b.SessionName}] = accessMaster{b.WorkerID, b.SessionName, ops.AccessProofVerified, b.OwnerID, legacyLimit(b.LegacyTargetsJSON)}
		}
	}
	for _, m := range f.Masters {
		if _, ok := e.masters[[2]string{m.WorkerID, m.SessionName}]; !ok {
			e.masters[[2]string{m.WorkerID, m.SessionName}] = accessMaster{m.WorkerID, m.SessionName, ops.AccessProofClaimed, "", legacyLimit(m.LegacyTargetsJSON)}
		}
	}
	// Direct grants by name (the call path of the Worker's socket) and the
	// ones bound to one incarnation of the source (the master broker, while
	// that incarnation holds a lease).
	for _, g := range f.Grants {
		if g.SourceWorkerID != g.TargetWorkerID {
			k := [2]string{g.SourceWorkerID, g.SourceSession}
			e.grants[k] = append(e.grants[k], g)
		}
	}
	return e
}

// shared tells whether the stored share rule of worker includes session and
// no pending restriction excludes it.
func (e *accessEval) shared(worker, session string) bool {
	d, ok := e.f.Shares[worker]
	if !ok || !d.Rule.Includes(session) {
		return false
	}
	for _, p := range e.f.Pending {
		if p.WorkerID == worker && !p.Rule.Includes(session) {
			return false
		}
	}
	return true
}

func (e *accessEval) accepted(source, target, session string) bool {
	if a, ok := e.accepts[[2]string{source, target}]; ok {
		return a.Sessions == nil || slices.Contains(a.Sessions, session)
	}
	return e.f.Control[target].AcceptAll
}

// proof tells how the Hub knows a source session when no route covers a
// pair: by its master nomination, else by name.
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
	switch {
	case src.Pending:
		v.Pending, v.PendingWorker = ops.AccessBlockedWorkerPending, src.ID
	case t.Pending:
		v.Pending, v.PendingWorker = ops.AccessBlockedWorkerPending, t.ID
	}
	switch {
	case !e.shared(t.ID, name):
		v.Sharing, v.SharingWorker = ops.AccessBlockedNotShared, t.ID
	case sourceShare && !e.shared(src.ID, session):
		v.Sharing, v.SharingWorker = ops.AccessBlockedNotShared, src.ID
	}
	if !t.Online || !src.Online {
		v.Offline = ops.AccessBlockedWorkerOffline
	}
	v.Limited = v.Granted == ops.AccessLevelMessage && !t.AgentOnly
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
	if v.Limited {
		v.InEffect = ops.AccessLevelView
	}
}

// masterVerdict judges master reach as the call path does. A name-only master
// is decided by authz on its computed targets: the owner of the source Worker
// must be an active account and an administrator or the owner of the target.
// A session-bound master is served by the master catalog (list, snapshot and
// a prompt to an agent pane: the Message level) and only while its terminal
// holds a lease for the nominated incarnation.
func (e *accessEval) masterVerdict(m accessMaster, src, t AccessWorker, name string) accessVerdict {
	v := accessVerdict{Via: ops.AccessViaMaster, Granted: ops.AccessLevelControl, Proof: m.proof}
	verified := m.proof == ops.AccessProofVerified
	if verified {
		v.Granted = ops.AccessLevelMessage
	}
	switch {
	case !src.OwnerActive:
		v.Authority = ops.AccessBlockedOwnerInactive
	case !src.OwnerAdmin && src.OwnerID != t.OwnerID:
		v.Authority = ops.AccessBlockedOwnerRule
	case verified && m.owner != src.OwnerID:
		v.Authority = ops.AccessBlockedAuthority
	case verified && !e.f.Attached[[2]string{src.ID, m.session}]:
		v.Authority = ops.AccessBlockedMasterNotAttached
	}
	e.workerLayers(&v, src, t, m.session, name, verified)
	v.settle(v.Authority, v.Pending, v.Sharing, v.Offline)
	return v
}

// grantVerdict judges a direct grant as authz does (directUsable and
// directFor): the owner of the source Worker is an active account, both
// Workers still have the owners of decision time, and the authority stands.
// A grant by name is used on the Worker's socket (Claimed). A grant bound to
// one incarnation (Verified) is used by the master broker only, while that
// incarnation holds a lease (its source session is then shared), and within
// the master catalog: list, snapshot and a prompt to an agent pane, so at
// most Message.
func (e *accessEval) grantVerdict(g store.SessionGrantRow, src, t AccessWorker, name string) accessVerdict {
	v := accessVerdict{Via: g.ID, Granted: accessGrantLevel(g.Level), Proof: ops.AccessProofClaimed, grant: &g}
	verified := g.SourceIncarnation != ""
	if verified {
		v.Proof = ops.AccessProofVerified
		if accessRank(v.Granted) > accessRank(ops.AccessLevelMessage) {
			v.Granted = ops.AccessLevelMessage
		}
	}
	switch {
	case g.SuspendedAt != nil:
		v.Time = ops.AccessBlockedSuspended
	case g.ExpiresAt != nil && *g.ExpiresAt <= e.now:
		v.Time = ops.AccessBlockedExpired
	}
	switch {
	case verified && !e.f.Leases[[3]string{src.ID, g.SourceSession, g.SourceIncarnation}]:
		v.Authority = ops.AccessBlockedSourceIncarnation
	case !src.OwnerActive:
		v.Authority = ops.AccessBlockedOwnerInactive
	case v.Granted == "" || g.SourceOwnerID == "" || t.OwnerID == "" || g.SourceOwnerID != src.OwnerID || g.TargetOwnerID != t.OwnerID:
		v.Authority = ops.AccessBlockedAuthority
	case g.Authority == store.GrantAuthorityAdmin:
		if !e.f.Admins[g.GrantedBy] {
			v.Authority = ops.AccessBlockedAuthority
		}
	case g.Authority != store.GrantAuthorityOwner || g.SourceOwnerID != g.TargetOwnerID:
		v.Authority = ops.AccessBlockedAuthority
	}
	e.workerLayers(&v, src, t, g.SourceSession, name, verified)
	v.settle(v.Time, v.Authority, v.Pending, v.Sharing, v.Offline)
	return v
}

// pair is the one evaluator: the best reach session on src has to the live
// session name on t. A master reach and a grant may both cover the pair; the
// higher level in effect wins, then the higher level the Hub would authorize
// and, on a tie, the master.
func (e *accessEval) pair(src AccessWorker, session string, t AccessWorker, name string) accessVerdict {
	var best accessVerdict
	// Never the same Worker: its sessions reach each other anyway.
	if src.ID == t.ID || !slices.Contains(t.Sessions, name) {
		return best
	}
	better := func(v accessVerdict) bool {
		if !best.Covered {
			return true
		}
		if d := accessRank(v.InEffect) - accessRank(best.InEffect); d != 0 {
			return d > 0
		}
		return accessRank(v.authorized()) > accessRank(best.authorized())
	}
	if m, ok := e.masters[[2]string{src.ID, session}]; ok && e.accepted(src.ID, t.ID, name) && m.limit(t.ID, name) {
		best = e.masterVerdict(m, src, t, name)
	}
	for _, g := range e.grants[[2]string{src.ID, session}] {
		if g.TargetWorkerID != t.ID || g.TargetSession != "" && g.TargetSession != name {
			continue
		}
		if v := e.grantVerdict(g, src, t, name); better(v) {
			best = v
		}
	}
	return best
}

// grantsProof is the proof of a source that only holds grants: Verified when
// every one of them is bound to an incarnation.
func (e *accessEval) grantsProof(worker, session string) string {
	gs := e.grants[[2]string{worker, session}]
	for _, g := range gs {
		if g.SourceIncarnation == "" {
			return ops.AccessProofClaimed
		}
	}
	if len(gs) == 0 {
		return ops.AccessProofClaimed
	}
	return ops.AccessProofVerified
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
		proof           string
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
					InEffect: v.InEffect, Via: v.Via, BlockedBy: v.Blocked}, v.Proof})
			}
		}
		// A source that covers nothing is still a row for its owner, unless
		// a filter asked for another Worker.
		if len(cells) == 0 && (!own(src) || only != "" && only != src.ID) {
			continue
		}
		// The proof of a row is the weakest proof of the routes its cells
		// use: Verified only when every cell is served by a verified route.
		proof := e.proof(src.ID, s.session)
		if len(cells) == 0 && !s.master {
			proof = e.grantsProof(src.ID, s.session)
		}
		for i, c := range cells {
			if i == 0 || c.proof == ops.AccessProofClaimed {
				proof = c.proof
			}
		}
		row := ops.AccessSource{WorkerID: src.ID, WorkerName: src.Name, Owner: src.Owner,
			Session: s.session, Proof: proof, Master: s.master}
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

// The pages a failing layer is changed on (relative URLs of the Hub).
const (
	accessFixGrants  = "/access/grants"
	accessFixControl = "/access/control"
)

func accessFixWorker(id string) string { return "/workers/" + url.PathEscape(id) }

// accessAuthority is the authority check of a covered pair: the stable code,
// the sentence and the page where it is changed.
func accessAuthority(e *accessEval, v accessVerdict) (code, text, fix string) {
	master := v.Via == ops.AccessViaMaster
	fix = accessFixGrants
	if master {
		fix = accessFixControl
	}
	switch v.Authority {
	case "":
		switch {
		case master:
			return ops.AccessCodeAuthorityOK, "the owner of the source Worker may reach this Worker", ""
		case v.grant != nil && v.grant.Authority == store.GrantAuthorityAdmin:
			return ops.AccessCodeAuthorityOK, "an active administrator decided this grant", ""
		}
		return ops.AccessCodeAuthorityOK, "both Workers still belong to the owner who decided this grant", ""
	case ops.AccessBlockedOwnerInactive:
		return v.Authority, "the owner of the source Worker is not an active account", ""
	case ops.AccessBlockedOwnerRule:
		return v.Authority, "the owner of the source Worker is neither an administrator nor the owner of the target Worker", fix
	case ops.AccessBlockedMasterNotAttached:
		return v.Authority, "the terminal this master session was nominated on is not attached now", fix
	case ops.AccessBlockedSourceIncarnation:
		return v.Authority, "the grant is bound to one terminal incarnation of the source session, which holds no lease of its Worker's master lane now", fix
	}
	if master {
		return v.Authority, "the source Worker changed its owner since this master session was nominated", fix
	}
	if g := v.grant; g != nil && g.Authority == store.GrantAuthorityAdmin && !e.f.Admins[g.GrantedBy] {
		return v.Authority, "the administrator who decided this grant is no longer an active administrator", fix
	}
	return v.Authority, "a Worker changed its owner since this grant was decided", fix
}

// AccessExplain answers one pair from the same evaluator as the matrix. It
// is false when a Worker is unknown or when the viewer may not see both ends
// (they own one end, or neither): the request names the sessions in clear,
// so any answer about a hidden end, a covered pair or a suspended grant
// included, would confirm a name the matrix masks. A session of another
// owner is then answered the same whether it exists or not, whether a grant
// covers it or not, and whether or not it is a master.
func AccessExplain(f AccessFacts, viewer string, all bool, a ops.AccessExplainArgs) (ops.AccessExplainResult, bool) {
	e := newAccessEval(f)
	src, ok := e.workers[a.SourceWorkerID]
	t, ok2 := e.workers[a.TargetWorkerID]
	own := func(w AccessWorker) bool { return all || viewer != "" && w.OwnerID == viewer }
	if !ok || !ok2 || !own(src) || !own(t) {
		return ops.AccessExplainResult{}, false
	}
	v := e.pair(src, a.SourceSession, t, a.TargetSession)
	_, master := e.masters[[2]string{src.ID, a.SourceSession}]
	// The proof of the route that covers the pair; without one, how the Hub
	// knows the source session.
	proof := e.proof(src.ID, a.SourceSession)
	if v.Covered {
		proof = v.Proof
	}
	res := ops.AccessExplainResult{
		Source: ops.AccessSource{WorkerID: src.ID, WorkerName: src.Name, Owner: src.Owner, Session: a.SourceSession, Proof: proof, Master: master},
		Target: ops.AccessTarget{WorkerID: t.ID, WorkerName: t.Name, Owner: t.Owner, Online: t.Online, Session: a.TargetSession},
	}
	res.InEffect, res.Via = v.InEffect, v.Via

	failed := false
	// add appends one check: pass with its code and text, or fail with the
	// failing code, text and fix; after the first failure the rest are skipped.
	add := func(id string, fail bool, code, text, fix string) {
		c := ops.AccessCheck{ID: id, State: ops.AccessCheckPass, Code: code, Text: text}
		switch {
		case failed:
			c.State, c.Code, c.Text = ops.AccessCheckSkip, ops.AccessCodeSkipped, "not evaluated: an earlier check failed"
		case fail:
			c.State, c.Fix = ops.AccessCheckFail, fix
			failed = true
		}
		res.Checks = append(res.Checks, c)
	}
	if proof == ops.AccessProofVerified {
		add(ops.AccessCheckCaller, false, ops.AccessCodeCallerVerified, "the source session is a kernel-proven terminal (Verified)", "")
	} else {
		add(ops.AccessCheckCaller, false, ops.AccessCodeCallerClaimed, "the source session is named by its Worker (Claimed)", "")
	}
	switch {
	case v.Covered && v.Via == ops.AccessViaMaster:
		add(ops.AccessCheckGrant, false, ops.AccessCodeMaster,
			"master reach gives "+accessLevelWord(v.Granted)+": the target Worker accepts this master session", "")
	case v.Covered:
		add(ops.AccessCheckGrant, false, ops.AccessCodeGrant, "grant "+v.Via+" gives "+accessLevelWord(v.Granted), "")
	case src.ID == t.ID:
		add(ops.AccessCheckGrant, true, ops.AccessCodeSameWorker,
			"no grant and no master reach covers this pair: sessions of one Worker reach each other without a grant", "")
	case !slices.Contains(t.Sessions, a.TargetSession):
		add(ops.AccessCheckGrant, true, ops.AccessCodeNoSuchSession,
			"no grant and no master reach covers this pair: the target Worker does not list a session of that name now", "")
	default:
		add(ops.AccessCheckGrant, true, ops.AccessCodeNoGrant, "no grant and no master reach covers this pair", accessFixGrants)
	}
	code, text, fix := accessAuthority(e, v)
	add(ops.AccessCheckAuthority, v.Authority != "", code, text, fix)
	switch v.Time {
	case ops.AccessBlockedExpired:
		add(ops.AccessCheckTime, true, v.Time, "the grant has expired", accessFixGrants)
	case ops.AccessBlockedSuspended:
		add(ops.AccessCheckTime, true, v.Time, "the grant is suspended until an administrator approves it again", accessFixGrants)
	default:
		add(ops.AccessCheckTime, false, ops.AccessCodeTimeOK, "not expired and not suspended", "")
	}
	// A link to a Worker only for a viewer who may open it.
	workerFix := func(id string) string {
		if w, ok := e.workers[id]; ok && own(w) {
			return accessFixWorker(id)
		}
		return ""
	}
	switch {
	case v.Sharing == "":
		add(ops.AccessCheckSharing, false, ops.AccessCodeShared, "the target Worker shares the session", "")
	case v.SharingWorker == src.ID:
		add(ops.AccessCheckSharing, true, v.Sharing, "the master session is not shared by its Worker", workerFix(src.ID))
	default:
		add(ops.AccessCheckSharing, true, v.Sharing, "the session is not shared by its Worker", workerFix(t.ID))
	}
	switch {
	case v.Pending != "":
		add(ops.AccessCheckWorkers, true, v.Pending, "a Worker is waiting for approval", workerFix(v.PendingWorker))
	case v.Offline != "":
		add(ops.AccessCheckWorkers, true, v.Offline, "a Worker is offline", "")
	case v.Limited:
		add(ops.AccessCheckWorkers, false, ops.AccessCodeMessageAsView,
			"both Workers are approved and connected; the target Worker cannot take a prompt for agent panes only, so Message works as View until it is updated", "")
	default:
		add(ops.AccessCheckWorkers, false, ops.AccessCodeWorkersOK, "both Workers are approved and connected", "")
	}
	res.Checks = append(res.Checks, ops.AccessCheck{ID: ops.AccessCheckPreviewOnly, State: ops.AccessCheckUnknown,
		Code: ops.AccessBlockedPreviewOnlyUnknown, Text: "the target Worker decides this and does not report it"})
	return res, true
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
