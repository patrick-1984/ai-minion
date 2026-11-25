package store

// Conversion of the minion-control tables into session grants (A104/A106,
// DESIGN section 9, story G9 first half). This file only PLANS: it reads the
// old tables in one read-only transaction and returns the grant rows, the
// accept-all rules and a report of what cannot be carried over. Nothing is
// written and no reader uses the plan yet.
//
// The plan is static consent only. What today's readers decide from live
// state stays a rule of the later evaluator ("legacy" authority): the owner
// of the source Worker is active and holds a role, a target of another owner
// is reached only while that owner is an administrator, and a grant with a
// source incarnation (a bound master) also needs both Workers approved and
// keeps the share fences of the bound path.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

// Values of every converted grant.
const (
	ConvertedLevelControl    = 3
	ConvertedAuthorityLegacy = "legacy"
	ConvertedNoteMigrated    = "migrated"
)

// Reasons of a [ConvertDropped] entry.
const (
	// The Worker of a nomination, an acceptance or a setting no longer exists.
	ConvertDropSourceWorkerGone = "source_worker_gone"
	ConvertDropTargetWorkerGone = "target_worker_gone"
	// A bound nomination that is not active, or whose owner or key no longer
	// matches its Worker, reaches nothing today.
	ConvertDropBoundInactive = "bound_inactive"
	ConvertDropBoundStale    = "bound_stale"
	// A v0.8 limit that names the master's own Worker: a grant inside one
	// Worker is refused (D2). This is the only entry that reaches LESS than
	// before; TargetSession "" means the whole Worker.
	ConvertDropOwnWorker = "own_worker"
	// A v0.8 limit that cannot be read, or that names nothing any more,
	// allows nothing.
	ConvertDropLimitUnreadable = "limit_unreadable"
	ConvertDropLimitEmpty      = "limit_empty"
	// An acceptance limited to no session stays "nothing" (it still keeps
	// the source out of an accept-all rule, see ConvertedRule.Except).
	ConvertDropAcceptEmpty = "accept_empty"
	// An empty session name never matched a session; as a grant it would
	// mean "all sessions".
	ConvertDropEmptySessionName = "empty_session_name"
)

// ConvertedGrant is one grant row that the conversion would write.
type ConvertedGrant struct {
	SourceWorkerID    string
	SourceSession     string
	SourceIncarnation string // "" = a name-only master; else the bound incarnation
	TargetWorkerID    string
	TargetSession     string // "" = every session of the target Worker
	Level             int
	Authority         string
	Note              string
}

// ConvertedRule is a setting that no grant row can express: the target
// accepts every master, also of Workers enrolled later. Except lists the
// source Workers with an explicit acceptance row for this target: for them
// only that row counts (it wins even when it allows nothing).
type ConvertedRule struct {
	TargetWorkerID string
	AcceptAll      bool
	Except         []string
}

// ConvertedTarget is one entry of a v0.8 limit; Session "" is the whole Worker.
type ConvertedTarget struct {
	WorkerID string
	Session  string
}

// ConvertedMaster is a nomination that the accept-all rules apply to. With
// Limited the rules reach only what Limit names (an empty Limit: nothing);
// the grant rows of the plan are already narrowed.
type ConvertedMaster struct {
	SourceWorkerID    string
	SourceSession     string
	SourceIncarnation string
	Limited           bool
	Limit             []ConvertedTarget
}

// ConvertDropped reports something that is not carried over, with the reason.
type ConvertDropped struct {
	Reason            string
	SourceWorkerID    string
	SourceSession     string
	SourceIncarnation string
	TargetWorkerID    string
	TargetSession     string
	Detail            string
}

// ControlConversion is the plan of [Store.PlanControlConversion].
type ControlConversion struct {
	Grants  []ConvertedGrant
	Rules   []ConvertedRule
	Masters []ConvertedMaster
	Dropped []ConvertDropped
}

type convertWorker struct {
	owner string
	pub   []byte
}

type convertAccept struct {
	target string
	whole  bool // sessions_json is NULL
	names  []string
}

// convertLimit is a parsed v0.8 limit.
type convertLimit struct {
	limited  bool
	whole    map[string]bool
	sessions map[string][]string
}

// parseConvertLimit reads legacy_targets_json as today's reader does: "" is
// no limit, an unreadable value allows nothing, "everything" lifts the limit.
func parseConvertLimit(legacy string) (lim convertLimit, readable bool) {
	if legacy == "" {
		return convertLimit{}, true
	}
	lim = convertLimit{limited: true, whole: map[string]bool{}, sessions: map[string][]string{}}
	var items []struct {
		Kind        string `json:"kind"`
		WorkerID    string `json:"worker_id"`
		SessionName string `json:"session_name"`
	}
	if err := json.Unmarshal([]byte(legacy), &items); err != nil {
		return lim, false
	}
	for _, it := range items {
		switch it.Kind {
		case "everything":
			return convertLimit{}, true
		case "worker":
			lim.whole[it.WorkerID] = true
		case "session":
			lim.sessions[it.WorkerID] = append(lim.sessions[it.WorkerID], it.SessionName)
		}
	}
	return lim, true
}

// narrow returns what the limit leaves of one accepted target: all reports
// the whole Worker, otherwise the named sessions. session "" asks for the
// whole Worker.
func (l convertLimit) narrow(worker, session string) (all bool, names []string) {
	switch {
	case !l.limited || l.whole[worker]:
		if session == "" {
			return true, nil
		}
		return false, []string{session}
	case session == "":
		return false, l.sessions[worker]
	}
	for _, n := range l.sessions[worker] {
		if n == session {
			return false, []string{session}
		}
	}
	return false, nil
}

// PlanControlConversion reads master_session, control_accept, worker_control
// and bound_master_session in one read-only transaction and returns the grant
// rows and rules that give the same reach as today's readers. It writes
// nothing.
func (s *Store) PlanControlConversion(ctx context.Context) (ControlConversion, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ControlConversion{}, fmt.Errorf("store: plan control conversion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	plan, err := planControlConversion(ctx, tx)
	if err != nil {
		return ControlConversion{}, fmt.Errorf("store: plan control conversion: %w", err)
	}
	return plan, nil
}

func planControlConversion(ctx context.Context, tx *sql.Tx) (ControlConversion, error) {
	var plan ControlConversion
	workers, err := convertReadWorkers(ctx, tx)
	if err != nil {
		return plan, err
	}
	accepts, explicit, err := convertReadAccepts(ctx, tx, workers, &plan)
	if err != nil {
		return plan, err
	}
	if err := convertReadRules(ctx, tx, workers, explicit, &plan); err != nil {
		return plan, err
	}

	seen := map[ConvertedGrant]bool{}
	// addMaster plans one nomination. ownWorker says whether today's reader
	// lets a v0.8 limit reach the master's own Worker (the name-only path).
	addMaster := func(worker, session, incarnation, legacy string, ownWorker bool) {
		src := ConvertDropped{SourceWorkerID: worker, SourceSession: session, SourceIncarnation: incarnation}
		drop := func(reason, targetWorker, targetSession, detail string) {
			d := src
			d.Reason, d.TargetWorkerID, d.TargetSession, d.Detail = reason, targetWorker, targetSession, detail
			plan.Dropped = append(plan.Dropped, d)
		}
		lim, readable := parseConvertLimit(legacy)
		if !readable {
			drop(ConvertDropLimitUnreadable, "", "", "an unreadable v0.8 limit allows nothing")
		}
		grant := func(target, name string) {
			if target == worker {
				return
			}
			g := ConvertedGrant{SourceWorkerID: worker, SourceSession: session, SourceIncarnation: incarnation,
				TargetWorkerID: target, TargetSession: name, Level: ConvertedLevelControl,
				Authority: ConvertedAuthorityLegacy, Note: ConvertedNoteMigrated}
			if !seen[g] {
				seen[g] = true
				plan.Grants = append(plan.Grants, g)
			}
		}
		emit := func(target, name string, own bool) {
			all, names := lim.narrow(target, name)
			if all {
				names = []string{""}
			}
			for _, n := range names {
				switch {
				case n == "" && !all:
					drop(ConvertDropEmptySessionName, target, "", "a session without a name is never reached")
				case own:
					drop(ConvertDropOwnWorker, target, n, "a grant inside one Worker is refused; its sessions share the local socket")
				default:
					grant(target, n)
				}
			}
		}
		for _, a := range accepts[worker] {
			if a.whole {
				emit(a.target, "", false)
				continue
			}
			for _, n := range a.names {
				if n != "" {
					emit(a.target, n, false)
				}
			}
		}
		// Today's reader adds the own Worker for every v0.8 limit, also for
		// "everything" (which parses as no limit here).
		if legacy != "" && ownWorker {
			emit(worker, "", true)
		}

		m := ConvertedMaster{SourceWorkerID: worker, SourceSession: session, SourceIncarnation: incarnation, Limited: lim.limited}
		for w := range lim.whole {
			if w != worker && w != "" {
				m.Limit = append(m.Limit, ConvertedTarget{WorkerID: w})
			}
		}
		for w, names := range lim.sessions {
			if w == worker || w == "" || lim.whole[w] {
				continue
			}
			dup := map[string]bool{}
			for _, n := range names {
				if n != "" && !dup[n] {
					dup[n] = true
					m.Limit = append(m.Limit, ConvertedTarget{WorkerID: w, Session: n})
				}
			}
		}
		sort.Slice(m.Limit, func(i, j int) bool {
			if m.Limit[i].WorkerID != m.Limit[j].WorkerID {
				return m.Limit[i].WorkerID < m.Limit[j].WorkerID
			}
			return m.Limit[i].Session < m.Limit[j].Session
		})
		if lim.limited && readable && len(m.Limit) == 0 {
			drop(ConvertDropLimitEmpty, "", "", "the v0.8 limit names no other Worker: the master reaches nothing there")
		}
		plan.Masters = append(plan.Masters, m)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT worker_id, session_name, COALESCE(legacy_targets_json, '') FROM master_session ORDER BY worker_id, session_name`)
	if err != nil {
		return plan, err
	}
	type nomination struct{ worker, session, incarnation, legacy string }
	var named []nomination
	for rows.Next() {
		var n nomination
		if err := rows.Scan(&n.worker, &n.session, &n.legacy); err != nil {
			_ = rows.Close()
			return plan, err
		}
		named = append(named, n)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return plan, err
	}
	for _, n := range named {
		if _, ok := workers[n.worker]; !ok {
			plan.Dropped = append(plan.Dropped, ConvertDropped{Reason: ConvertDropSourceWorkerGone, SourceWorkerID: n.worker,
				SourceSession: n.session, Detail: "the Worker of this master session no longer exists"})
			continue
		}
		addMaster(n.worker, n.session, "", n.legacy, true)
	}

	brows, err := tx.QueryContext(ctx, `SELECT `+boundMasterColumns+` FROM bound_master_session ORDER BY worker_id, session_name`)
	if err != nil {
		return plan, err
	}
	var bound []BoundMasterRow
	for brows.Next() {
		b, err := scanBoundMaster(brows)
		if err != nil {
			_ = brows.Close()
			return plan, err
		}
		bound = append(bound, b)
	}
	_ = brows.Close()
	if err := brows.Err(); err != nil {
		return plan, err
	}
	for _, b := range bound {
		d := ConvertDropped{SourceWorkerID: b.WorkerID, SourceSession: b.SessionName, SourceIncarnation: b.IncarnationID}
		w, ok := workers[b.WorkerID]
		switch {
		case !b.Active:
			d.Reason, d.Detail = ConvertDropBoundInactive, "the bound nomination is not active"
		case !ok || w.owner != b.OwnerID || !bytes.Equal(w.pub, b.WorkerPub):
			d.Reason, d.Detail = ConvertDropBoundStale, "the bound nomination was made for another owner or key of the Worker"
		case b.IncarnationID == "":
			d.Reason, d.Detail = ConvertDropBoundStale, "a bound nomination without an incarnation cannot stay verified"
		default:
			// The bound path never reaches the master's own Worker.
			addMaster(b.WorkerID, b.SessionName, b.IncarnationID, b.LegacyTargetsJSON, false)
			continue
		}
		plan.Dropped = append(plan.Dropped, d)
	}

	sort.Slice(plan.Grants, func(i, j int) bool {
		a, b := plan.Grants[i], plan.Grants[j]
		switch {
		case a.SourceWorkerID != b.SourceWorkerID:
			return a.SourceWorkerID < b.SourceWorkerID
		case a.SourceSession != b.SourceSession:
			return a.SourceSession < b.SourceSession
		case a.SourceIncarnation != b.SourceIncarnation:
			return a.SourceIncarnation < b.SourceIncarnation
		case a.TargetWorkerID != b.TargetWorkerID:
			return a.TargetWorkerID < b.TargetWorkerID
		}
		return a.TargetSession < b.TargetSession
	})
	return plan, nil
}

func convertReadWorkers(ctx context.Context, tx *sql.Tx) (map[string]convertWorker, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, owner_id, worker_pub FROM workers`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]convertWorker{}
	for rows.Next() {
		var id string
		var w convertWorker
		if err := rows.Scan(&id, &w.owner, &w.pub); err != nil {
			return nil, err
		}
		out[id] = w
	}
	return out, rows.Err()
}

// convertReadAccepts returns the acceptances by source Worker and, by target
// Worker, the sources with an explicit row. Rows that name a Worker that no
// longer exists are reported once.
func convertReadAccepts(ctx context.Context, tx *sql.Tx, workers map[string]convertWorker, plan *ControlConversion) (map[string][]convertAccept, map[string][]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT target_worker_id, source_worker_id, sessions_json FROM control_accept
		 WHERE target_worker_id <> source_worker_id ORDER BY source_worker_id, target_worker_id`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	bySource, explicit := map[string][]convertAccept{}, map[string][]string{}
	for rows.Next() {
		var source string
		var a convertAccept
		var sess sql.NullString
		if err := rows.Scan(&a.target, &source, &sess); err != nil {
			return nil, nil, err
		}
		// Every explicit row keeps its source out of accept-all, whatever it allows.
		explicit[a.target] = append(explicit[a.target], source)
		d := ConvertDropped{SourceWorkerID: source, TargetWorkerID: a.target}
		if _, ok := workers[a.target]; !ok {
			d.Reason, d.Detail = ConvertDropTargetWorkerGone, "the accepting Worker no longer exists"
			plan.Dropped = append(plan.Dropped, d)
			continue
		}
		if _, ok := workers[source]; !ok {
			d.Reason, d.Detail = ConvertDropSourceWorkerGone, "the accepted Worker no longer exists"
			plan.Dropped = append(plan.Dropped, d)
			continue
		}
		if a.whole = !sess.Valid; !a.whole {
			// As today's reader: what does not parse allows nothing.
			_ = json.Unmarshal([]byte(sess.String), &a.names)
			usable := 0
			for _, n := range a.names {
				if n == "" {
					d.Reason, d.Detail = ConvertDropEmptySessionName, "a session without a name is never reached"
					plan.Dropped = append(plan.Dropped, d)
					continue
				}
				usable++
			}
			if usable == 0 {
				d.Reason, d.Detail = ConvertDropAcceptEmpty, "accepted for no session: no grant"
				plan.Dropped = append(plan.Dropped, d)
			}
		}
		bySource[source] = append(bySource[source], a)
	}
	return bySource, explicit, rows.Err()
}

func convertReadRules(ctx context.Context, tx *sql.Tx, workers map[string]convertWorker, explicit map[string][]string, plan *ControlConversion) error {
	rows, err := tx.QueryContext(ctx, `SELECT worker_id FROM worker_control WHERE accept_all = 1 ORDER BY worker_id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if _, ok := workers[id]; !ok {
			plan.Dropped = append(plan.Dropped, ConvertDropped{Reason: ConvertDropTargetWorkerGone, TargetWorkerID: id,
				Detail: "the Worker that accepts all masters no longer exists"})
			continue
		}
		except := append([]string{}, explicit[id]...)
		sort.Strings(except)
		plan.Rules = append(plan.Rules, ConvertedRule{TargetWorkerID: id, AcceptAll: true, Except: except})
	}
	return rows.Err()
}
