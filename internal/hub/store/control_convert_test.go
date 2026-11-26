package store

// The differential test of the control conversion (DESIGN section 9, K7
// section 4): the reach of today's readers must equal the reach of the
// planned grants and rules, on generated fleets and on hand-written cases.
//
// The oracle below is a FROZEN copy of today's readers (controlTargetsFrom
// and intersectLegacy of minion_control.go, BoundMasterTargets with
// masterEnrollment and masterAllowedWorkers of bound_master.go, and the
// Worker-grant rule of authz/decide.go). Do not replace it by calls into
// production code: those readers will change, the oracle must not.
//
// Outside both sides, on purpose: the share rule and preview-only (live
// state of the target Worker, applied after the consent on both sides) and
// the lease of a bound master.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"
)

// ---- frozen oracle -------------------------------------------------------

type oracleResource struct {
	Kind        string `json:"kind"`
	WorkerID    string `json:"worker_id,omitempty"`
	SessionName string `json:"session_name,omitempty"`
}

// oracleControlTargetsFrom is controlTargetsFrom as of 53a708cf.
func oracleControlTargetsFrom(ctx context.Context, db *sql.DB, source, legacy string) ([]oracleResource, error) {
	type accept struct {
		target   string
		sessions *string
	}
	rows, err := db.QueryContext(ctx,
		`SELECT target_worker_id, sessions_json FROM control_accept WHERE source_worker_id = ?1 AND target_worker_id <> ?1
		 UNION
		 SELECT worker_id, NULL FROM worker_control WHERE accept_all = 1 AND worker_id <> ?1
		   AND worker_id NOT IN (SELECT target_worker_id FROM control_accept WHERE source_worker_id = ?1)`, source)
	if err != nil {
		return nil, err
	}
	var acc []accept
	for rows.Next() {
		var a accept
		var sess sql.NullString
		if err := rows.Scan(&a.target, &sess); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if sess.Valid {
			v := sess.String
			a.sessions = &v
		}
		acc = append(acc, a)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []oracleResource
	for _, a := range acc {
		if a.sessions == nil {
			out = append(out, oracleResource{Kind: "worker", WorkerID: a.target})
			continue
		}
		var names []string
		_ = json.Unmarshal([]byte(*a.sessions), &names)
		for _, n := range names {
			out = append(out, oracleResource{Kind: "session", WorkerID: a.target, SessionName: n})
		}
	}
	if legacy != "" {
		out = append(out, oracleResource{Kind: "worker", WorkerID: source})
		out = oracleIntersectLegacy(out, legacy)
	}
	return out, nil
}

// oracleIntersectLegacy is intersectLegacy as of 53a708cf.
func oracleIntersectLegacy(computed []oracleResource, legacy string) []oracleResource {
	var lim []oracleResource
	if err := json.Unmarshal([]byte(legacy), &lim); err != nil {
		return nil
	}
	everything := false
	whole := map[string]bool{}
	sessions := map[string][]string{}
	for _, l := range lim {
		switch l.Kind {
		case "everything":
			everything = true
		case "worker":
			whole[l.WorkerID] = true
		case "session":
			sessions[l.WorkerID] = append(sessions[l.WorkerID], l.SessionName)
		}
	}
	var out []oracleResource
	for _, c := range computed {
		switch {
		case everything || whole[c.WorkerID]:
			out = append(out, c)
		case c.Kind == "worker":
			for _, n := range sessions[c.WorkerID] {
				out = append(out, oracleResource{Kind: "session", WorkerID: c.WorkerID, SessionName: n})
			}
		default:
			for _, n := range sessions[c.WorkerID] {
				if n == c.SessionName {
					out = append(out, c)
				}
			}
		}
	}
	return out
}

// oracleMasterEnrollment is masterEnrollment as of 53a708cf.
func oracleMasterEnrollment(ctx context.Context, db *sql.DB, workerID, owner string, pub []byte) (admin bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_roles r WHERE r.user_id=u.id AND r.role='admin')
	 FROM workers w JOIN users u ON u.id=w.owner_id WHERE w.id=? AND w.owner_id=? AND w.worker_pub=?
	 AND w.approval='approved' AND u.active=1 AND EXISTS(SELECT 1 FROM user_roles r WHERE r.user_id=u.id)`,
		workerID, owner, pub).Scan(&admin)
	return admin, err
}

// oracleMasterAllowedWorkers is masterAllowedWorkers as of 53a708cf.
func oracleMasterAllowedWorkers(ctx context.Context, db *sql.DB, source, owner string, admin bool) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM workers WHERE id<>? AND approval='approved' AND (? OR owner_id=?)`, source, admin, owner)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// oracleBoundTargets is the authority part of BoundMasterTargets as of
// 53a708cf (target == nil), without the share fence. ok is false where the
// reader returns ErrMasterRevision or ErrWorkerOwnerChanged.
func oracleBoundTargets(ctx context.Context, db *sql.DB, cur BoundMasterRow) (out []oracleResource, ok bool, err error) {
	admin, err := oracleMasterEnrollment(ctx, db, cur.WorkerID, cur.OwnerID, cur.WorkerPub)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !cur.Active {
		return nil, false, nil
	}
	targets, err := oracleControlTargetsFrom(ctx, db, cur.WorkerID, cur.LegacyTargetsJSON)
	if err != nil {
		return nil, false, err
	}
	allowed, err := oracleMasterAllowedWorkers(ctx, db, cur.WorkerID, cur.OwnerID, admin)
	if err != nil {
		return nil, false, err
	}
	for _, t := range targets {
		if allowed[t.WorkerID] {
			out = append(out, t)
		}
	}
	return out, true, nil
}

// oracleSubject is workerSubject of authz/decide.go as of 53a708cf: the
// Worker exists and its owner is an active user with a role.
func oracleSubject(ctx context.Context, db *sql.DB, worker string) (owner string, admin, ok bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT owner_id FROM workers WHERE id = ?`, worker).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	var active, isAdmin, isUser bool
	err = db.QueryRowContext(ctx, `SELECT active,
	 EXISTS(SELECT 1 FROM user_roles r WHERE r.user_id=u.id AND r.role='admin'),
	 EXISTS(SELECT 1 FROM user_roles r WHERE r.user_id=u.id AND r.role='user') FROM users u WHERE u.id = ?`, owner).Scan(&active, &isAdmin, &isUser)
	if errors.Is(err, sql.ErrNoRows) {
		return owner, false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	return owner, isAdmin, active && (isAdmin || isUser), nil
}

// oracleCovers is the scope match of a computed grant: a "worker" resource
// covers every session of its Worker, a "session" resource one name.
func oracleCovers(targets []oracleResource, worker, session string) bool {
	for _, r := range targets {
		if r.WorkerID == worker && (r.Kind == "worker" || r.Kind == "session" && r.SessionName == session) {
			return true
		}
	}
	return false
}

// ---- reach ---------------------------------------------------------------

type cvPair struct{ sw, ss, inc, tw, ts string }

func (p cvPair) String() string {
	return fmt.Sprintf("%s@%s[%s] -> %s@%s", p.ss, p.sw, p.inc, p.ts, p.tw)
}

type cvSource struct{ w, s, inc string }

// cvUniverse is the closed universe: every Worker (and one id that does not
// exist) with every session name (and one name no row mentions).
type cvUniverse struct {
	workers []string
	names   []string
	sources []cvSource
}

var cvNames = []string{"s0", "s1", "s2", "s3", "s4"}

const (
	cvGhost       = "ghost"
	cvUnmentioned = "unmentioned"
)

func cvReadUniverse(t *testing.T, s *Store) cvUniverse {
	t.Helper()
	u := cvUniverse{names: append(append([]string{}, cvNames...), cvUnmentioned)}
	u.workers = append(cvStrings(t, s, `SELECT id FROM workers ORDER BY id`), cvGhost)
	for _, w := range u.workers {
		for _, n := range u.names {
			u.sources = append(u.sources, cvSource{w, n, ""})
		}
	}
	rows, err := s.ListBoundMasters(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		u.sources = append(u.sources, cvSource{b.WorkerID, b.SessionName, b.IncarnationID})
	}
	return u
}

func cvStrings(t *testing.T, s *Store, query string) []string {
	t.Helper()
	rows, err := s.DB().Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// cvOldReach evaluates the frozen oracle over the universe.
func cvOldReach(t *testing.T, s *Store, u cvUniverse) map[cvPair]bool {
	t.Helper()
	ctx, db := context.Background(), s.DB()
	out := map[cvPair]bool{}
	bound := map[cvSource]BoundMasterRow{}
	rows, err := s.ListBoundMasters(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		bound[cvSource{b.WorkerID, b.SessionName, b.IncarnationID}] = b
	}
	for _, src := range u.sources {
		var targets []oracleResource
		if src.inc == "" {
			// Path L: SessionControl, then the Worker-grant rule of authz.
			var legacy string
			err := db.QueryRowContext(ctx, `SELECT COALESCE(legacy_targets_json, '') FROM master_session WHERE worker_id = ? AND session_name = ?`,
				src.w, src.s).Scan(&legacy)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if targets, err = oracleControlTargetsFrom(ctx, db, src.w, legacy); err != nil {
				t.Fatal(err)
			}
		} else {
			var ok bool
			if targets, ok, err = oracleBoundTargets(ctx, db, bound[src]); err != nil {
				t.Fatal(err)
			} else if !ok {
				continue
			}
		}
		owner, admin, ok, err := oracleSubject(ctx, db, src.w)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			continue
		}
		for _, tw := range u.workers {
			var resOwner string
			err := db.QueryRowContext(ctx, `SELECT owner_id FROM workers WHERE id = ?`, tw).Scan(&resOwner)
			if errors.Is(err, sql.ErrNoRows) {
				continue // the resource does not resolve
			}
			if err != nil {
				t.Fatal(err)
			}
			if !admin && resOwner != owner {
				continue // base(): a user reaches only what the owner owns
			}
			for _, ts := range u.names {
				if oracleCovers(targets, tw, ts) {
					out[cvPair{src.w, src.s, src.inc, tw, ts}] = true
				}
			}
		}
	}
	return out
}

type cvFacts struct {
	owner    map[string]string // Worker -> owner; absent: no such Worker
	approved map[string]bool
	active   map[string]bool // user is active and holds a role
	admin    map[string]bool
}

func cvReadFacts(t *testing.T, s *Store) cvFacts {
	t.Helper()
	f := cvFacts{owner: map[string]string{}, approved: map[string]bool{}, active: map[string]bool{}, admin: map[string]bool{}}
	rows, err := s.DB().Query(`SELECT id, owner_id, approval FROM workers`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, owner, approval string
		if err := rows.Scan(&id, &owner, &approval); err != nil {
			t.Fatal(err)
		}
		f.owner[id], f.approved[id] = owner, approval == "approved"
	}
	_ = rows.Close()
	rows, err = s.DB().Query(`SELECT u.id, u.active, r.role FROM users u JOIN user_roles r ON r.user_id = u.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, role string
		var active bool
		if err := rows.Scan(&id, &active, &role); err != nil {
			t.Fatal(err)
		}
		f.active[id] = active
		if role == "admin" {
			f.admin[id] = true
		}
	}
	return f
}

// legacyValid is the "legacy" authority rule of the new evaluator: the owner
// of the source Worker is active and holds a role; a target of another owner
// is reached only while that owner is an administrator; a verified source
// (a bound master) also needs both Workers approved.
func (f cvFacts) legacyValid(src cvSource, target string) bool {
	owner, ok := f.owner[src.w]
	targetOwner, tok := f.owner[target]
	if !ok || !tok || target == src.w || !f.active[owner] {
		return false
	}
	if src.inc != "" && (!f.approved[src.w] || !f.approved[target]) {
		return false
	}
	return targetOwner == owner || f.admin[owner]
}

// cvNewReach evaluates the plan: grant rows plus accept-all rules.
func cvNewReach(plan ControlConversion, f cvFacts, u cvUniverse) map[cvPair]bool {
	out := map[cvPair]bool{}
	masters := map[cvSource]ConvertedMaster{}
	for _, m := range plan.Masters {
		masters[cvSource{m.SourceWorkerID, m.SourceSession, m.SourceIncarnation}] = m
	}
	for _, src := range u.sources {
		for _, tw := range u.workers {
			if !f.legacyValid(src, tw) {
				continue
			}
			for _, ts := range u.names {
				covered := false
				for _, g := range plan.Grants {
					if g.SourceWorkerID == src.w && g.SourceSession == src.s && g.SourceIncarnation == src.inc &&
						g.TargetWorkerID == tw && (g.TargetSession == "" || g.TargetSession == ts) {
						covered = true
					}
				}
				if m, ok := masters[src]; ok && !covered {
					for _, r := range plan.Rules {
						if !r.AcceptAll || r.TargetWorkerID != tw || tw == src.w || slices.Contains(r.Except, src.w) {
							continue
						}
						if !m.Limited {
							covered = true
						}
						for _, l := range m.Limit {
							if l.WorkerID == tw && (l.Session == "" || l.Session == ts) {
								covered = true
							}
						}
					}
				}
				if covered {
					out[cvPair{src.w, src.s, src.inc, tw, ts}] = true
				}
			}
		}
	}
	return out
}

// cvOwnWorkerReach is the reach the plan reports as dropped because it lies
// inside the master's own Worker (the one known narrowing).
func cvOwnWorkerReach(plan ControlConversion, f cvFacts, u cvUniverse) map[cvPair]bool {
	out := map[cvPair]bool{}
	for _, d := range plan.Dropped {
		if d.Reason != ConvertDropOwnWorker {
			continue
		}
		if owner, ok := f.owner[d.SourceWorkerID]; !ok || !f.active[owner] {
			continue
		}
		for _, ts := range u.names {
			if d.TargetSession == "" || d.TargetSession == ts {
				out[cvPair{d.SourceWorkerID, d.SourceSession, d.SourceIncarnation, d.TargetWorkerID, ts}] = true
			}
		}
	}
	return out
}

// cvCompare plans the conversion and requires the same reach as the oracle
// for every pair of the universe. It returns the plan and the old reach.
func cvCompare(t *testing.T, s *Store, label string) (ControlConversion, map[cvPair]bool) {
	t.Helper()
	before := cvFingerprint(t, s)
	plan, err := s.PlanControlConversion(context.Background())
	if err != nil {
		t.Fatalf("%s: plan: %v", label, err)
	}
	if after := cvFingerprint(t, s); after != before {
		t.Fatalf("%s: planning wrote to the database:\n%s\n%s", label, before, after)
	}
	u, f := cvReadUniverse(t, s), cvReadFacts(t, s)
	old, got, own := cvOldReach(t, s, u), cvNewReach(plan, f, u), cvOwnWorkerReach(plan, f, u)
	for _, g := range plan.Grants {
		if g.Level != 3 || g.Authority != "legacy" || g.Note != "migrated" || g.SourceSession == "" ||
			g.SourceWorkerID == g.TargetWorkerID || f.owner[g.SourceWorkerID] == "" || f.owner[g.TargetWorkerID] == "" {
			t.Errorf("%s: a grant that no row can hold: %+v", label, g)
		}
	}
	failures := 0
	for _, src := range u.sources {
		for _, tw := range u.workers {
			for _, ts := range u.names {
				p := cvPair{src.w, src.s, src.inc, tw, ts}
				if own[p] && (p.tw != p.sw || got[p]) {
					t.Errorf("%s: %v: reported as own-Worker reach but is not", label, p)
				}
				if old[p] == (got[p] || own[p]) {
					continue
				}
				if failures++; failures <= 8 {
					t.Errorf("%s: %v: old reach %v, planned reach %v", label, p, old[p], got[p])
				}
			}
		}
	}
	if failures > 8 {
		t.Errorf("%s: %d more differences", label, failures-8)
	}
	return plan, old
}

// cvFingerprint is the content of every table the plan reads or could write.
func cvFingerprint(t *testing.T, s *Store) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT count(*) FROM session_grant`,
		`SELECT count(*) || ':' || COALESCE(group_concat(worker_id || session_name || COALESCE(legacy_targets_json, 'NULL')), '') FROM master_session`,
		`SELECT count(*) || ':' || COALESCE(group_concat(target_worker_id || source_worker_id || COALESCE(sessions_json, 'NULL')), '') FROM control_accept`,
		`SELECT count(*) || ':' || COALESCE(group_concat(worker_id || user_managed || accept_all), '') FROM worker_control`,
		`SELECT count(*) || ':' || COALESCE(group_concat(worker_id || session_name || generation || active), '') FROM bound_master_session`,
	} {
		b.WriteString(strings.Join(cvStrings(t, s, q), ",") + ";")
	}
	return b.String()
}

// ---- fixtures ------------------------------------------------------------

func cvExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.DB().Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func cvPub(i int) []byte { return bytes.Repeat([]byte{byte(i + 1)}, 32) }

// cvUser adds a user; role is "admin", "user" or "" (no role).
func cvUser(t *testing.T, s *Store, id, role string, active bool) {
	t.Helper()
	cvExec(t, s, `INSERT INTO users(id,username,active,created_at,updated_at) VALUES(?,?,?,1,1)`, id, id, active)
	if role != "" {
		cvExec(t, s, `INSERT INTO user_roles(user_id,role,source) VALUES(?,?,'local')`, id, role)
	}
}

func cvWorker(t *testing.T, s *Store, i int, id, owner string, approved bool) {
	t.Helper()
	approval := "pending"
	if approved {
		approval = "approved"
	}
	cvExec(t, s, `INSERT INTO workers(id,worker_pub,name,owner_id,kind,created_at,approval) VALUES(?,?,?,?,'private',1,?)`,
		id, cvPub(i), id, owner, approval)
}

func cvMaster(t *testing.T, s *Store, worker, session string, legacy any) {
	t.Helper()
	cvExec(t, s, `INSERT INTO master_session(worker_id,session_name,legacy_targets_json,created_by,created_at) VALUES(?,?,?,'t',1)`,
		worker, session, legacy)
}

// cvAccept adds an acceptance; sessions nil is SQL NULL (the whole Worker).
func cvAccept(t *testing.T, s *Store, target, source string, sessions any) {
	t.Helper()
	cvExec(t, s, `INSERT INTO control_accept(target_worker_id,source_worker_id,sessions_json,created_by,created_at) VALUES(?,?,?,'t',1)`,
		target, source, sessions)
}

func cvAcceptAll(t *testing.T, s *Store, worker string) {
	t.Helper()
	cvExec(t, s, `INSERT INTO worker_control(worker_id,user_managed,accept_all) VALUES(?,1,1)`, worker)
}

func cvBound(t *testing.T, s *Store, i int, worker, owner, session string, active bool, legacy string) {
	t.Helper()
	cvExec(t, s, `INSERT INTO bound_master_session(worker_id,owner_id,session_name,incarnation_id,worker_pub,generation,active,legacy_targets_json,changed_at)
	 VALUES(?,?,?,?,?,1,?,?,1)`, worker, owner, session, "inc-"+worker+"-"+session, cvPub(i), active, legacy)
}

func cvJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// cvRandomLimit is a v0.8 limit: empty, everything, unreadable, or a few
// Workers and sessions (the own Worker and a missing one included).
func cvRandomLimit(r *rand.Rand, workers []string) string {
	switch r.IntN(10) {
	case 0:
		return "[]"
	case 1:
		return `[{"kind":"everything"}]`
	case 2:
		return `{"not":"a list"`
	}
	var lim []oracleResource
	for n := 1 + r.IntN(4); n > 0; n-- {
		w := cvGhost
		if r.IntN(8) > 0 {
			w = workers[r.IntN(len(workers))]
		}
		if r.IntN(2) == 0 {
			lim = append(lim, oracleResource{Kind: "worker", WorkerID: w})
		} else {
			lim = append(lim, oracleResource{Kind: "session", WorkerID: w, SessionName: cvNames[r.IntN(len(cvNames))]})
		}
	}
	return cvJSON(lim)
}

// cvGenFleet fills an empty store with a random fleet.
func cvGenFleet(t *testing.T, s *Store, seed uint64) {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, 0x5eed))
	var owners []string
	for i, n := 0, 2+r.IntN(3); i < n; i++ {
		id := fmt.Sprintf("f%d-u%d", seed, i)
		role := "user"
		switch r.IntN(10) {
		case 0, 1, 2, 3:
			role = "admin"
		case 4:
			role = ""
		}
		cvUser(t, s, id, role, r.IntN(8) > 0)
		if role == "admin" && r.IntN(3) == 0 {
			cvExec(t, s, `INSERT INTO user_roles(user_id,role,source) VALUES(?,'user','local')`, id)
		}
		owners = append(owners, id)
	}
	var workers []string
	ownerOf := map[string]string{}
	sessions := map[string][]string{}
	for i, n := 0, 3+r.IntN(6); i < n; i++ {
		id := fmt.Sprintf("w%d", i)
		ownerOf[id] = owners[r.IntN(len(owners))]
		cvWorker(t, s, i, id, ownerOf[id], r.IntN(7) > 0)
		workers = append(workers, id)
		sessions[id] = cvNames[:r.IntN(len(cvNames)+1)]
	}
	for i, w := range workers {
		for _, name := range sessions[w] {
			switch r.IntN(6) {
			case 0, 1: // a name-only master, half of them with a v0.8 limit
				var legacy any
				if r.IntN(2) == 0 {
					legacy = cvRandomLimit(r, workers)
				}
				cvMaster(t, s, w, name, legacy)
			case 2: // a bound master: inactive, stale owner, with or without a limit
				owner, legacy := ownerOf[w], ""
				if r.IntN(8) == 0 {
					owner = owners[r.IntN(len(owners))]
				}
				if r.IntN(3) == 0 {
					legacy = cvRandomLimit(r, workers)
				}
				cvBound(t, s, i, w, owner, name, r.IntN(4) > 0, legacy)
			}
		}
	}
	if r.IntN(6) == 0 {
		cvMaster(t, s, cvGhost, "s0", nil)
	}
	ids := append(append([]string{}, workers...), cvGhost)
	for _, target := range ids {
		for _, source := range ids {
			if target == source || r.IntN(10) > 2 || (target == cvGhost || source == cvGhost) && r.IntN(4) > 0 {
				continue
			}
			var sess any
			switch r.IntN(6) {
			case 0, 1: // NULL: the whole Worker
			case 2:
				sess = "[]"
			case 3:
				sess = "null"
			default:
				var names []string
				for n := 1 + r.IntN(3); n > 0; n-- {
					names = append(names, cvNames[r.IntN(len(cvNames))])
				}
				sess = cvJSON(names)
			}
			cvAccept(t, s, target, source, sess)
		}
		switch r.IntN(10) {
		case 0, 1, 2:
			cvAcceptAll(t, s, target)
		case 3:
			cvExec(t, s, `INSERT INTO worker_control(worker_id,user_managed,accept_all) VALUES(?,0,0)`, target)
		}
	}
}

func cvReset(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range []string{"control_accept", "master_session", "worker_control", "bound_master_session", "workers"} {
		cvExec(t, s, `DELETE FROM `+table)
	}
}

// ---- tests ---------------------------------------------------------------

func TestControlConversionDifferentialNeverMoreNeverLess(t *testing.T) {
	s := openTest(t)
	const fleets = 320
	reached, grants, rules := 0, 0, 0
	for seed := uint64(1); seed <= fleets; seed++ {
		cvReset(t, s)
		cvGenFleet(t, s, seed)
		plan, old := cvCompare(t, s, fmt.Sprintf("seed %d", seed))
		reached, grants, rules = reached+len(old), grants+len(plan.Grants), rules+len(plan.Rules)
		if t.Failed() {
			t.Fatalf("seed %d: the planned grants do not give today's reach (re-run with this seed in cvGenFleet)", seed)
		}
	}
	// A generator that reaches nothing would prove nothing.
	if reached < 10*fleets || grants < fleets || rules < fleets/2 {
		t.Fatalf("the fleets are too thin: %d reached pairs, %d grants, %d rules", reached, grants, rules)
	}
}

// cvEdge is two owners (an administrator and a user) and four Workers:
// A and B of the administrator, C and D of the user.
func cvEdge(t *testing.T) *Store {
	t.Helper()
	s := openTest(t)
	cvUser(t, s, "root", "admin", true)
	cvUser(t, s, "usr", "user", true)
	cvWorker(t, s, 0, "A", "root", true)
	cvWorker(t, s, 1, "B", "root", true)
	cvWorker(t, s, 2, "C", "usr", true)
	cvWorker(t, s, 3, "D", "usr", true)
	return s
}

func cvGrantList(plan ControlConversion) string {
	var out []string
	for _, g := range plan.Grants {
		out = append(out, fmt.Sprintf("%s@%s[%s]>%s@%s", g.SourceSession, g.SourceWorkerID, g.SourceIncarnation, g.TargetSession, g.TargetWorkerID))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func cvDropped(plan ControlConversion, reason string) []ConvertDropped {
	var out []ConvertDropped
	for _, d := range plan.Dropped {
		if d.Reason == reason {
			out = append(out, d)
		}
	}
	return out
}

func TestControlConversionEmptyListWinsOverAcceptAll(t *testing.T) {
	s := cvEdge(t)
	cvMaster(t, s, "A", "s0", nil)
	cvMaster(t, s, "C", "s0", nil)
	cvAcceptAll(t, s, "B")
	cvAccept(t, s, "B", "A", "[]")
	plan, old := cvCompare(t, s, "empty list")
	if len(plan.Grants) != 0 || len(plan.Rules) != 1 || cvJSON(plan.Rules[0]) != `{"TargetWorkerID":"B","AcceptAll":true,"Except":["A"]}` {
		t.Fatalf("plan = %+v", plan)
	}
	if len(cvDropped(plan, ConvertDropAcceptEmpty)) != 1 {
		t.Fatalf("dropped = %+v", plan.Dropped)
	}
	// A is kept out by its explicit empty row; C is a user of another owner.
	if len(old) != 0 {
		t.Fatalf("old reach = %v", old)
	}
	// The same rule reaches B from a master of the same owner without a row.
	cvWorker(t, s, 4, "E", "root", true)
	cvMaster(t, s, "E", "s1", nil)
	if _, old := cvCompare(t, s, "late master"); !old[cvPair{"E", "s1", "", "B", cvUnmentioned}] || len(old) != 6 {
		t.Fatalf("old reach = %v", old)
	}
}

func TestControlConversionLimitEmptiedByThePurge(t *testing.T) {
	s := cvEdge(t)
	cvMaster(t, s, "A", "s0", "[]")
	cvAccept(t, s, "B", "A", nil)
	cvAcceptAll(t, s, "C")
	plan, old := cvCompare(t, s, "emptied limit")
	if len(plan.Grants) != 0 || len(old) != 0 || len(cvDropped(plan, ConvertDropLimitEmpty)) != 1 {
		t.Fatalf("plan = %+v, old reach = %v", plan, old)
	}
	if len(plan.Masters) != 1 || !plan.Masters[0].Limited || len(plan.Masters[0].Limit) != 0 {
		t.Fatalf("masters = %+v", plan.Masters)
	}
}

func TestControlConversionLimitNamingTheOwnWorker(t *testing.T) {
	s := cvEdge(t)
	cvMaster(t, s, "A", "s0", `[{"kind":"worker","worker_id":"A"},{"kind":"session","worker_id":"B","session_name":"s1"},{"kind":"session","worker_id":"C","session_name":"s2"}]`)
	cvAccept(t, s, "B", "A", nil)
	cvAcceptAll(t, s, "C")
	plan, old := cvCompare(t, s, "own Worker")
	if got := cvGrantList(plan); got != "s0@A[]>s1@B" {
		t.Fatalf("grants = %q", got)
	}
	own := cvDropped(plan, ConvertDropOwnWorker)
	if len(own) != 1 || own[0].TargetWorkerID != "A" || own[0].TargetSession != "" {
		t.Fatalf("dropped = %+v", plan.Dropped)
	}
	// Today: s1 on B (the row), s2 on C (accept-all within the limit), and
	// every session of the own Worker, which no grant row can hold.
	if !old[cvPair{"A", "s0", "", "B", "s1"}] || !old[cvPair{"A", "s0", "", "C", "s2"}] || !old[cvPair{"A", "s0", "", "A", cvUnmentioned}] || len(old) != 2+len(cvNames)+1 {
		t.Fatalf("old reach = %v", old)
	}
}

func TestControlConversionSkipsAnInactiveOrStaleBoundRow(t *testing.T) {
	s := cvEdge(t)
	cvBound(t, s, 0, "A", "root", "s0", false, "")
	cvBound(t, s, 1, "B", "usr", "s0", true, "") // decided for another owner
	cvBound(t, s, 2, "C", "usr", "s1", true, "")
	cvAccept(t, s, "B", "A", nil)
	cvAccept(t, s, "A", "B", nil)
	cvAccept(t, s, "D", "C", `["s3"]`)
	plan, old := cvCompare(t, s, "bound rows")
	if got := cvGrantList(plan); got != "s1@C[inc-C-s1]>s3@D" {
		t.Fatalf("grants = %q", got)
	}
	if len(cvDropped(plan, ConvertDropBoundInactive)) != 1 || len(cvDropped(plan, ConvertDropBoundStale)) != 1 || len(old) != 1 {
		t.Fatalf("dropped = %+v, old reach = %v", plan.Dropped, old)
	}
}

func TestControlConversionPendingWorker(t *testing.T) {
	s := cvEdge(t)
	cvWorker(t, s, 4, "P", "root", false)
	cvMaster(t, s, "A", "s0", nil)
	cvBound(t, s, 1, "B", "root", "s1", true, "")
	cvAccept(t, s, "P", "A", nil)
	cvAccept(t, s, "P", "B", nil)
	cvAccept(t, s, "C", "B", nil)
	plan, old := cvCompare(t, s, "pending Worker")
	// The rows are static consent; approval is judged at call time. A bound
	// master never reaches a pending Worker, the name-only path leaves that
	// to the connection (a pending Worker serves no call).
	if got := cvGrantList(plan); got != "s0@A[]>@P s1@B[inc-B-s1]>@C s1@B[inc-B-s1]>@P" {
		t.Fatalf("grants = %q", got)
	}
	if old[cvPair{"B", "s1", "inc-B-s1", "P", "s0"}] || !old[cvPair{"B", "s1", "inc-B-s1", "C", "s0"}] || !old[cvPair{"A", "s0", "", "P", "s0"}] {
		t.Fatalf("old reach = %v", old)
	}
}

func TestControlConversionOfAnEmptyHub(t *testing.T) {
	s := openTest(t)
	plan, err := s.PlanControlConversion(context.Background())
	if err != nil || len(plan.Grants)+len(plan.Rules)+len(plan.Masters)+len(plan.Dropped) != 0 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
}
