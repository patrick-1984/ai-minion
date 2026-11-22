package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

type nomFixture struct {
	s       *Store
	workers []ops.WorkerInfo // w1 alice, w2 bob
	rows    []string         // session members: [0] on w1, [1] on w2
}

// nomSetup has alice and bob as administrators, each with a Worker and one
// observed session. Both Workers are authorized for admin reads.
func nomSetup(t *testing.T) nomFixture {
	t.Helper()
	s := openTest(t)
	ctx := context.Background()
	workers, r, now := usageFixture(t, s, 1)
	for _, w := range workers {
		if err := s.SaveUsage(ctx, w.ID, w.OwnerID, r, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []string{"alice", "bob"} {
		if _, err := s.db.Exec(`INSERT INTO user_roles(user_id,role,source) VALUES(?, 'admin','local')`, u); err != nil {
			t.Fatal(err)
		}
	}
	f := nomFixture{s: s, workers: workers}
	for _, w := range workers {
		rows := []string{}
		rs, err := s.db.Query(`SELECT id FROM usage_sessions WHERE worker_id=?`, w.ID)
		if err != nil {
			t.Fatal(err)
		}
		for rs.Next() {
			var id string
			_ = rs.Scan(&id)
			rows = append(rows, id)
		}
		_ = rs.Close()
		f.rows = append(f.rows, rows[0])
	}
	return f
}

func (f nomFixture) bucket(t *testing.T, owner, kind, label string) string {
	t.Helper()
	res, err := f.s.SetUsageBucket(context.Background(), owner, f.workers, ops.UsageBucketSetArgs{Action: "save", Kind: kind, Label: label})
	if err != nil {
		t.Fatal(err)
	}
	return res.ID
}

func (f nomFixture) nominate(owner, scope, kind, bucket string, domain bool) error {
	_, err := f.s.SetUsageNomination(context.Background(), owner, BucketAuthority{Actor: owner, Domain: domain},
		ops.UsageNominationSetArgs{Action: "nominate", Scope: scope, Kind: kind, BucketID: bucket})
	return err
}

func (f nomFixture) clear(actor, scope, kind string, domain bool) (ops.UsageNominationSetResult, error) {
	return f.s.SetUsageNomination(context.Background(), actor, BucketAuthority{Actor: actor, Domain: domain},
		ops.UsageNominationSetArgs{Action: "clear", Scope: scope, Kind: kind})
}

func (f nomFixture) count(t *testing.T, where string) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM usage_nominations WHERE ` + where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNominationUserSlotIsPerOwnerAndKind(t *testing.T) {
	f := nomSetup(t)
	b1 := f.bucket(t, "alice", "session", "one")
	b2 := f.bucket(t, "alice", "session", "two")
	p := f.bucket(t, "alice", "provider", "prov")
	for _, id := range []string{b1, b2} {
		if err := f.nominate("alice", "user", "session", id, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.nominate("alice", "user", "provider", p, false); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `scope='user' AND owner_id='alice'`); n != 2 {
		t.Fatalf("rows = %d, want one per kind", n)
	}
	var got string
	_ = f.s.db.QueryRow(`SELECT bucket_id FROM usage_nominations WHERE scope='user' AND kind='session'`).Scan(&got)
	if got != b2 {
		t.Fatal("second nomination must replace the first")
	}
	bl, err := f.s.UsageBuckets(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bl {
		want := b.ID == b2 || b.ID == p
		if (len(b.Nominated) == 1 && b.Nominated[0] == "user") != want {
			t.Fatalf("flags %+v", b)
		}
	}
	// A bucket of someone else cannot be nominated, and a user slot needs no domain authority.
	if err := f.nominate("bob", "user", "session", b1, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign bucket: %v", err)
	}
	if r, err := f.clear("alice", "user", "session", false); err != nil || !r.Changed {
		t.Fatalf("clear: %+v %v", r, err)
	}
	if r, err := f.clear("alice", "user", "session", false); err != nil || r.Changed {
		t.Fatalf("second clear: %+v %v", r, err)
	}
}

func TestNominationRowsAreNotNullAndChecked(t *testing.T) {
	f := nomSetup(t)
	b := f.bucket(t, "alice", "session", "one")
	for name, q := range map[string]string{
		"null owner":   `INSERT INTO usage_nominations(scope,owner_id,kind,bucket_id,actor_id,updated_at) VALUES('user',NULL,'session','` + b + `','a',1)`,
		"missing":      `INSERT INTO usage_nominations(scope,owner_id,kind,bucket_id,actor_id,updated_at) VALUES('user','alice','session','nope','a',1)`,
		"bad scope":    `INSERT INTO usage_nominations(scope,owner_id,kind,bucket_id,actor_id,updated_at) VALUES('all','alice','session','` + b + `','a',1)`,
		"bad kind":     `INSERT INTO usage_nominations(scope,owner_id,kind,bucket_id,actor_id,updated_at) VALUES('user','alice','x','` + b + `','a',1)`,
		"wrong kind":   `INSERT INTO usage_nominations(scope,owner_id,kind,bucket_id,actor_id,updated_at) VALUES('user','alice','provider','` + b + `','a',1)`,
		"null bucket":  `INSERT INTO usage_nominations(scope,owner_id,kind,bucket_id,actor_id,updated_at) VALUES('user','alice','session',NULL,'a',1)`,
		"empty bucket": `INSERT INTO usage_nominations(scope,owner_id,kind,bucket_id,actor_id,updated_at) VALUES('user','alice','session','','a',1)`,
	} {
		if _, err := f.s.db.Exec(q); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// The domain index allows a single publisher per kind at the table level.
	bb := f.bucket(t, "bob", "session", "bob")
	ins := `INSERT INTO usage_nominations(scope,owner_id,kind,bucket_id,actor_id,updated_at) VALUES('domain',?,'session',?,?,1)`
	if _, err := f.s.db.Exec(ins, "alice", b, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(ins, "bob", bb, "bob"); err == nil {
		t.Fatal("two domain slots of one kind")
	}
}

func TestDomainSlotSecondAdminReplacesAndClears(t *testing.T) {
	f := nomSetup(t)
	ab := f.bucket(t, "alice", "session", "alice")
	bb := f.bucket(t, "bob", "session", "bob")
	if err := f.nominate("alice", "domain", "session", ab, true); err != nil {
		t.Fatal(err)
	}
	if err := f.nominate("bob", "domain", "session", bb, true); err != nil {
		t.Fatalf("second admin replace: %v", err)
	}
	slots, err := f.s.UsageDomainSlots(context.Background())
	if err != nil || len(slots) != 1 || slots[0].PublisherID != "bob" || slots[0].PublisherName != "bob" || slots[0].Label != "bob" {
		t.Fatalf("slots %+v %v", slots, err)
	}
	// Alice clears a slot that bob filled, although she owns no bucket in it.
	if r, err := f.clear("alice", "domain", "session", true); err != nil || !r.Changed {
		t.Fatalf("clear %+v %v", r, err)
	}
	if f.count(t, `scope='domain'`) != 0 {
		t.Fatal("slot remains")
	}
	// A domain change without authority, or by an actor who is not an admin, is refused.
	if err := f.nominate("alice", "domain", "session", ab, false); !errors.Is(err, ErrNominationForbidden) {
		t.Fatalf("no authority: %v", err)
	}
	if _, err := f.clear("alice", "domain", "session", false); !errors.Is(err, ErrNominationForbidden) {
		t.Fatalf("clear without authority: %v", err)
	}
	if _, err := f.s.db.Exec(`DELETE FROM user_roles WHERE user_id='alice'`); err != nil {
		t.Fatal(err)
	}
	if err := f.nominate("alice", "domain", "session", ab, true); !errors.Is(err, ErrNominationForbidden) {
		t.Fatalf("stale authority of a former admin: %v", err)
	}
}

func TestDomainSlotConcurrentReplaceAndClear(t *testing.T) {
	f := nomSetup(t)
	ab := f.bucket(t, "alice", "provider", "alice")
	bb := f.bucket(t, "bob", "provider", "bob")
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for _, who := range []struct{ id, bucket string }{{"alice", ab}, {"bob", bb}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				if i%3 == 2 {
					if _, err := f.clear(who.id, "domain", "provider", true); err != nil {
						errs <- err
					}
					continue
				}
				if err := f.nominate(who.id, "domain", "provider", who.bucket, true); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent change failed: %v", err)
	}
	if n := f.count(t, `scope='domain' AND kind='provider'`); n > 1 {
		t.Fatalf("%d domain rows", n)
	}
	// Every committed change is audited with actor and opaque references only.
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action LIKE 'usage.nomination.%' AND actor IN ('alice','bob') AND target LIKE 'domain/provider%'`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("audit rows %d %v", n, err)
	}
}

func TestDomainNominationAuditIsInTheTransaction(t *testing.T) {
	f := nomSetup(t)
	ab := f.bucket(t, "alice", "session", "Alice private label")
	if err := f.nominate("alice", "domain", "session", ab, true); err != nil {
		t.Fatal(err)
	}
	var actor, action, target string
	if err := f.s.db.QueryRow(`SELECT actor,action,target FROM audit_events WHERE action='usage.nomination.set'`).Scan(&actor, &action, &target); err != nil {
		t.Fatal(err)
	}
	if actor != "alice" || target != "domain/session/"+ab || strings.Contains(target, "Alice") {
		t.Fatalf("audit %q %q", actor, target)
	}
	// An ineligible nomination rolls back and leaves no audit row behind.
	pb := f.bucket(t, "alice", "session", "mixed")
	if _, err := f.s.SetUsageBucket(context.Background(), "alice", f.workers, ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: pb, Members: []string{f.rows[1]}}); err != nil {
		t.Fatal(err)
	}
	before := f.count(t, `1=1`)
	err := f.nominate("alice", "domain", "session", pb, true)
	var refusal *NominationRefusal
	if !errors.As(err, &refusal) || refusal.Members != 1 || !errors.Is(err, ErrNominationIneligible) {
		t.Fatalf("want refusal, got %v", err)
	}
	if f.count(t, `1=1`) != before {
		t.Fatal("refused nomination changed the slot")
	}
}

func TestDomainBucketLockedAgainstEditsWithoutAuthority(t *testing.T) {
	f := nomSetup(t)
	ctx := context.Background()
	b := f.bucket(t, "alice", "session", "published")
	other := f.bucket(t, "alice", "session", "other")
	plain := f.bucket(t, "alice", "session", "plain")
	set := func(auth BucketAuthority, a ops.UsageBucketSetArgs) error {
		_, err := f.s.SetUsageBucketAs(ctx, "alice", f.workers[:1], a, auth)
		return err
	}
	if err := set(BucketAuthority{}, ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: b, Members: []string{f.rows[0]}}); err != nil {
		t.Fatalf("assign before publication: %v", err)
	}
	if err := f.nominate("alice", "domain", "session", b, true); err != nil {
		t.Fatal(err)
	}
	denied := []ops.UsageBucketSetArgs{
		{Action: "save", Kind: "session", BucketID: b, Label: "renamed"},
		{Action: "delete", Kind: "session", BucketID: b},
		{Action: "assign", Kind: "session", BucketID: other, Members: []string{f.rows[0]}}, // moves a member out
		{Action: "assign", Kind: "session", Members: []string{f.rows[0]}},                  // removes the membership
		{Action: "assign", Kind: "session", BucketID: other, WorkerIDs: []string{"w1"}},    // indirect: by Worker
		{Action: "assign", Kind: "session", BucketID: other, All: true},                    // indirect: all
		{Action: "assign", Kind: "session", BucketID: b, Members: []string{f.rows[0]}},     // changes its content
	}
	for _, a := range denied {
		if err := set(BucketAuthority{Actor: "alice"}, a); !errors.Is(err, ErrNominationForbidden) {
			t.Fatalf("%+v: %v", a, err)
		}
	}
	// Buckets that are not published remain free to edit without authority.
	if err := set(BucketAuthority{}, ops.UsageBucketSetArgs{Action: "save", Kind: "session", BucketID: plain, Label: "free"}); err != nil {
		t.Fatalf("plain rename: %v", err)
	}
	if err := set(BucketAuthority{}, ops.UsageBucketSetArgs{Action: "delete", Kind: "session", BucketID: plain}); err != nil {
		t.Fatalf("plain delete: %v", err)
	}
	if f.count(t, `scope='domain'`) != 1 {
		t.Fatal("nomination lost by refused edits")
	}
	// With authority the change goes through and is audited; deleting cascades.
	if err := set(BucketAuthority{Actor: "alice", Domain: true}, ops.UsageBucketSetArgs{Action: "save", Kind: "session", BucketID: b, Label: "renamed"}); err != nil {
		t.Fatal(err)
	}
	if err := set(BucketAuthority{Actor: "alice", Domain: true}, ops.UsageBucketSetArgs{Action: "delete", Kind: "session", BucketID: b}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `1=1`) != 0 {
		t.Fatal("bucket deletion must delete its nomination")
	}
	var n int
	_ = f.s.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action LIKE 'usage.domain_bucket.%'`).Scan(&n)
	if n != 2 {
		t.Fatalf("bucket edit audit rows = %d", n)
	}
}

func TestNominationCascadesWithUser(t *testing.T) {
	f := nomSetup(t)
	b := f.bucket(t, "bob", "session", "b")
	if err := f.nominate("bob", "domain", "session", b, true); err != nil {
		t.Fatal(err)
	}
	if err := f.nominate("bob", "user", "session", b, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`DELETE FROM workers WHERE owner_id='bob'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`DELETE FROM users WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `1=1`) != 0 {
		t.Fatal("nominations survived the user")
	}
}

func TestDomainNominationRetiredWhenPublisherLosesAuthority(t *testing.T) {
	f := nomSetup(t)
	ab := f.bucket(t, "alice", "session", "a")
	bb := f.bucket(t, "bob", "session", "b")
	pa := f.bucket(t, "alice", "provider", "p")
	_ = pa
	publish := func() {
		t.Helper()
		if err := f.nominate("alice", "domain", "session", ab, true); err != nil {
			t.Fatal(err)
		}
		if err := f.nominate("alice", "user", "session", ab, false); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	// An unrelated admin's slot and the user slot are not touched; the IdP
	// replacement of unchanged roles does not retire anything.
	if _, err := f.s.db.Exec(`INSERT INTO user_roles(user_id,role,source) VALUES('alice','admin','idp')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.ReplaceIDPRoles(context.Background(), "alice", []string{"admin"}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `scope='domain'`) != 1 {
		t.Fatal("an unchanged sign-in sync retired the nomination")
	}
	// Losing one of two admin grants keeps authority.
	if _, _, err := f.s.ReplaceIDPRoles(context.Background(), "alice", []string{"user"}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `scope='domain'`) != 1 {
		t.Fatal("still admin through the local grant")
	}
	// Role loss.
	if _, err := f.s.db.Exec(`DELETE FROM user_roles WHERE user_id='alice' AND role='admin'`); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `scope='domain'`) != 0 || f.count(t, `scope='user'`) != 1 {
		t.Fatal("role loss must retire the domain nomination only")
	}
	// Re-promotion does not republish.
	if _, err := f.s.db.Exec(`INSERT INTO user_roles(user_id,role,source) VALUES('alice','admin','local')`); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `scope='domain'`) != 0 {
		t.Fatal("re-promotion republished")
	}
	// Deactivation through the real path.
	if err := f.nominate("bob", "domain", "session", bb, true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DeactivateUser(context.Background(), DeactivateUserParams{UserID: "bob", NowMS: 5}); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `scope='domain'`) != 0 {
		t.Fatal("deactivation did not retire the nomination")
	}
	if _, err := f.s.db.Exec(`UPDATE users SET active=1 WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `scope='domain'`) != 0 {
		t.Fatal("reactivation republished")
	}
	if err := f.nominate("bob", "domain", "session", bb, true); err != nil {
		t.Fatalf("a fresh explicit nomination works: %v", err)
	}
}

func TestSourceOwnerPublicationPermission(t *testing.T) {
	f := nomSetup(t)
	ctx := context.Background()
	b := f.bucket(t, "alice", "session", "mixed")
	// alice holds her own session and bob's.
	for _, m := range f.rows {
		if _, err := f.s.SetUsageBucket(ctx, "alice", f.workers, ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: b, Members: []string{m}}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, _ := f.s.UsageAllowRepublish(ctx, "bob"); ok {
		t.Fatal("default must be off")
	}
	sources, err := f.s.UsagePublishableSources(ctx, "alice", "session", b)
	if err != nil || len(sources) != 1 || sources[0].WorkerID != "w1" {
		t.Fatalf("filter before aggregation: %+v %v", sources, err)
	}
	for _, scope := range []string{"user", "domain"} {
		err := f.nominate("alice", scope, "session", b, true)
		if !errors.Is(err, ErrNominationIneligible) {
			t.Fatalf("%s: %v", scope, err)
		}
		if strings.Contains(err.Error(), "bob") || strings.Contains(err.Error(), "w2") {
			t.Fatalf("refusal names the owner: %v", err)
		}
	}
	if err := f.s.SetUsagePublication(ctx, "bob", true); err != nil {
		t.Fatal(err)
	}
	if err := f.nominate("alice", "domain", "session", b, true); err != nil {
		t.Fatalf("after consent: %v", err)
	}
	if sources, _ = f.s.UsagePublishableSources(ctx, "alice", "session", b); len(sources) != 2 {
		t.Fatalf("sources after consent: %+v", sources)
	}
	// Withdrawing the permission filters bob's observations at once, and a
	// move of bob's session into a published bucket is refused when he refuses.
	if err := f.s.SetUsagePublication(ctx, "bob", false); err != nil {
		t.Fatal(err)
	}
	if sources, _ = f.s.UsagePublishableSources(ctx, "alice", "session", b); len(sources) != 1 {
		t.Fatalf("sources after withdrawal: %+v", sources)
	}
	c := f.bucket(t, "alice", "session", "own")
	if err := f.nominate("alice", "user", "session", c, false); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.SetUsageBucketAs(ctx, "alice", f.workers, ops.UsageBucketSetArgs{Action: "assign", Kind: "session", BucketID: c, Members: []string{f.rows[1]}}, BucketAuthority{Actor: "alice", Domain: true})
	if !errors.Is(err, ErrNominationIneligible) {
		t.Fatalf("smuggled foreign member: %v", err)
	}
	if got, _ := f.s.db.Query(`SELECT 1 FROM usage_members WHERE bucket_id=? AND member_id=?`, c, f.rows[1]); got != nil {
		defer got.Close()
		if got.Next() {
			t.Fatal("refused assignment was not rolled back")
		}
	}
	// Ordinary A72 reporting is unchanged by consent.
	if out, err := f.s.ReadUsage(ctx, f.workers, "alice", ops.UsageListArgs{Section: "sessions"}, time.Now()); err != nil || len(out.Sessions) != 2 {
		t.Fatalf("admin reporting: %+v %v", out, err)
	}
}

// hubSchemaBefore returns the embedded Hub migrations without 0030 and later.
func hubSchemaBefore(t *testing.T, version string) Option {
	t.Helper()
	m := fstest.MapFS{}
	err := fs.WalkDir(hubMigrations, migrationsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(p) >= version {
			return err
		}
		b, err := fs.ReadFile(hubMigrations, p)
		m[p] = &fstest.MapFile{Data: b}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return WithMigrations(m, migrationsDir)
}

func TestNominationMigrationFrom0029AndBackupRestore(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	old, err := Open(dir, hubSchemaBefore(t, "0030"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`INSERT INTO users(id,username,created_at,updated_at) VALUES('alice','alice',1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`INSERT INTO usage_buckets(id,owner_id,kind,label,position) VALUES('b1','alice','session','keep',0)`); err != nil {
		t.Fatal(err)
	}
	if hasTable(old.db, "usage_nominations") {
		t.Fatal("0029 schema already has nominations")
	}
	_ = old.Close()

	fixed := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	cur, err := Open(dir, WithClock(func() time.Time { return fixed }))
	if err != nil {
		t.Fatal(err)
	}
	if !hasTable(cur.db, "usage_nominations") || !hasTable(cur.db, "usage_publication_consent") {
		t.Fatal("migration did not create the tables")
	}
	var n int
	if err := cur.db.QueryRow(`SELECT count(*) FROM usage_buckets`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("existing buckets: %d %v", n, err)
	}
	if err := cur.SetUsagePublication(ctx, "alice", true); err != nil {
		t.Fatal(err)
	}
	_ = cur.Close()

	backup := filepath.Join(dir, "backups", "minion-20261109T080000.000Z.db")
	b, err := sql.Open("sqlite", "file:"+backup+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	if hasTable(b, "usage_nominations") {
		t.Fatal("pre-migration backup holds the new schema")
	}
	_ = b.Close()

	// Restore the backup over the live database: the previous release opens it,
	// and the current release migrates it again without losing the buckets.
	for _, ext := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(filepath.Join(dir, DBFile+ext))
	}
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DBFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(dir, hubSchemaBefore(t, "0030"))
	if err != nil {
		t.Fatalf("previous release on restored backup: %v", err)
	}
	if hasTable(restored.db, "usage_nominations") {
		t.Fatal("restored database has the new schema")
	}
	_ = restored.Close()
	again, err := Open(dir, WithClock(func() time.Time { return fixed.Add(time.Hour) }))
	if err != nil {
		t.Fatalf("re-migrating the restored backup: %v", err)
	}
	defer again.Close()
	if !hasTable(again.db, "usage_nominations") {
		t.Fatal("re-migration failed")
	}
	if err := again.db.QueryRow(`SELECT count(*) FROM usage_buckets WHERE label='keep'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("restored data: %d %v", n, err)
	}
	if allow, _ := again.UsageAllowRepublish(ctx, "alice"); allow {
		t.Fatal("restored database kept a consent made after the backup")
	}
}

func hasTable(db *sql.DB, name string) bool {
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n > 0
}
