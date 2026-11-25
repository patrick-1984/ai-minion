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

type accessMaster struct {
	worker, session, proof, legacy string
}

// AccessMatrix computes the matrix from one set of facts for a viewer: all
// sees every Worker, otherwise only the Workers owned by viewer. only, when
// set, keeps the pairs with that Worker at either end.
func AccessMatrix(f AccessFacts, viewer string, all bool, only string) ops.AccessMatrixResult {
	res := ops.AccessMatrixResult{Sources: []ops.AccessSource{}, Targets: []ops.AccessTarget{}, Cells: []ops.AccessCell{}}
	workers := map[string]AccessWorker{}
	for _, w := range f.Workers {
		workers[w.ID] = w
	}
	visible := func(w AccessWorker) bool { return all || viewer != "" && w.OwnerID == viewer }
	shared := func(worker, session string) bool {
		d, ok := f.Shares[worker]
		return ok && d.Rule.Includes(session)
	}
	// An explicit acceptance row wins over accept_all, also when it names no
	// session at all.
	accepts := map[[2]string]store.AcceptRow{}
	for _, a := range f.Accepts {
		accepts[[2]string{a.SourceWorkerID, a.TargetWorkerID}] = a
	}
	accepted := func(source, target, session string) bool {
		if a, ok := accepts[[2]string{source, target}]; ok {
			return a.Sessions == nil || slices.Contains(a.Sessions, session)
		}
		return f.Control[target].AcceptAll
	}

	// Master sessions: session-bound nominations (verified) and name-only
	// rows (claimed). A bound row replaces the name-only row of its name.
	var masters []accessMaster
	seen := map[[2]string]bool{}
	for _, b := range f.Bound {
		if b.Active {
			masters = append(masters, accessMaster{b.WorkerID, b.SessionName, ops.AccessProofVerified, b.LegacyTargetsJSON})
			seen[[2]string{b.WorkerID, b.SessionName}] = true
		}
	}
	for _, m := range f.Masters {
		if !seen[[2]string{m.WorkerID, m.SessionName}] {
			masters = append(masters, accessMaster{m.WorkerID, m.SessionName, ops.AccessProofClaimed, m.LegacyTargetsJSON})
		}
	}
	sort.SliceStable(masters, func(i, j int) bool {
		a, b := workers[masters[i].worker], workers[masters[j].worker]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return ops.NaturalCompare(masters[i].session, masters[j].session) < 0
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
	for _, m := range masters {
		src, ok := workers[m.worker]
		if !ok || !visible(src) {
			continue
		}
		limit := legacyLimit(m.legacy)
		var cells []found
		for _, t := range targets {
			// Never the master's own Worker: its sessions reach each other anyway.
			if t.ID == src.ID || !visible(t) || only != "" && only != src.ID && only != t.ID {
				continue
			}
			for _, name := range t.Sessions {
				if !accepted(src.ID, t.ID, name) || !limit(t.ID, name) {
					continue
				}
				c := ops.AccessCell{Source: len(res.Sources), Granted: ops.AccessLevelControl, Via: ops.AccessViaMaster}
				switch {
				case !src.OwnerAdmin && src.OwnerID != t.OwnerID:
					c.BlockedBy = ops.AccessBlockedOwnerRule
				case src.Pending || t.Pending:
					c.BlockedBy = ops.AccessBlockedWorkerPending
				case !shared(t.ID, name) || m.proof == ops.AccessProofVerified && !shared(src.ID, m.session):
					c.BlockedBy = ops.AccessBlockedNotShared
				case !t.Online || !src.Online:
					c.BlockedBy = ops.AccessBlockedWorkerOffline
				default:
					c.InEffect = ops.AccessLevelControl
				}
				cells = append(cells, found{t.ID, name, c})
			}
		}
		// A master that covers nothing is still a source, unless a filter
		// asked for another Worker.
		if len(cells) == 0 && only != "" && only != src.ID {
			continue
		}
		res.Sources = append(res.Sources, ops.AccessSource{WorkerID: src.ID, WorkerName: src.Name, Owner: src.Owner,
			Session: m.session, Proof: m.proof, Master: true})
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
				res.Targets = append(res.Targets, ops.AccessTarget{WorkerID: t.ID, WorkerName: t.Name, Owner: t.Owner, Online: t.Online, Session: name})
			}
		}
	}
	for _, c := range covered {
		c.cell.Target = index[[2]string{c.worker, c.session}]
		res.Cells = append(res.Cells, c.cell)
	}
	return res
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
