package store

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// openBeforeMigration0020 opens a database that is at the end of the v0.5 plus
// F-07 schema (every migration before 0020), for the fixtures of the F-02
// migration.
func openBeforeMigration0020(t *testing.T, dir string) *Store {
	t.Helper()
	files := map[string]string{}
	entries, err := hubMigrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") || e.Name() >= "0020" {
			continue
		}
		b, err := hubMigrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(b)
	}
	old, err := Open(dir, testMigrations(files))
	if err != nil {
		t.Fatal(err)
	}
	return old
}

// The migration of F-02 on a database with Workers and invitation keys made
// under the old rules: existing Workers are approved, non-admin keys without an
// expiry expire a week after the upgrade, reusable keys without a limit get ten
// more uses, and the owner sees a notice on each key that was limited.
func TestMigration0020OnAV05Database(t *testing.T) {
	dir := t.TempDir()
	old := openBeforeMigration0020(t, dir)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := old.DB().Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users (id, username, active, created_at, updated_at) VALUES ('adm', 'adm', 1, 1, 1), ('usr', 'usr', 1, 1, 1)`)
	exec(`INSERT INTO user_roles VALUES ('adm', 'admin', 'local'), ('usr', 'user', 'local')`)
	exec(`INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at) VALUES ('w1', zeroblob(32), 'old-worker', 'usr', 'private', 1)`)
	key := func(id string, secret byte, creator, mode, ttl string, expires any, maxUses any, uses int, revoked int) {
		exec(`INSERT INTO invitation_keys (id, secret_hash, name, mode, kind, ttl, creator_id, created_at, expires_at, uses, max_uses, revoked)
		      VALUES (?, ?, ?, ?, 'private', ?, ?, 1, ?, ?, ?, ?)`,
			id, bytes.Repeat([]byte{secret}, 32), id, mode, ttl, creator, expires, uses, maxUses, revoked)
	}
	key("usr-unlimited-once", 1, "usr", "one_time", "unlimited", nil, 1, 0, 0)
	key("usr-unlimited-reusable", 2, "usr", "reusable", "unlimited", nil, nil, 4, 0)
	key("usr-reusable-dated", 3, "usr", "reusable", "1m", int64(9_999_999_999_999), nil, 2, 0)
	key("usr-limited", 4, "usr", "reusable", "1w", int64(9_999_999_999_999), 5, 1, 0)
	key("usr-revoked", 5, "usr", "reusable", "unlimited", nil, nil, 0, 1)
	key("adm-unlimited", 6, "adm", "reusable", "unlimited", nil, nil, 3, 0)
	_ = old.Close()

	cur, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close() }()
	ctx := context.Background()

	w, err := cur.WorkerByID(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Pending || w.ApprovedBy != "migration" || w.ApprovedAt == 0 {
		t.Errorf("an existing Worker must be approved by the migration: %+v", w)
	}

	now := func() int64 {
		var ms int64
		if err := cur.DB().QueryRow(`SELECT CAST(strftime('%s', 'now') AS INTEGER) * 1000`).Scan(&ms); err != nil {
			t.Fatal(err)
		}
		return ms
	}
	week := int64(7 * 24 * 3600 * 1000)
	get := func(id string) InvitationRow {
		t.Helper()
		r, err := cur.InvitationByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	near := func(got, want int64) bool { return got > want-120_000 && got < want+120_000 }

	r := get("usr-unlimited-once")
	if !near(r.ExpiresAt, now()+week) || r.MaxUses != 1 || r.Notice == "" {
		t.Errorf("one-time unlimited: %+v", r)
	}
	r = get("usr-unlimited-reusable")
	if !near(r.ExpiresAt, now()+week) || r.MaxUses != 4+10 || !strings.Contains(r.Notice, "7 days") || !strings.Contains(r.Notice, "10 more") {
		t.Errorf("reusable unlimited: %+v", r)
	}
	r = get("usr-reusable-dated")
	if r.ExpiresAt != 9_999_999_999_999 || r.MaxUses != 2+10 || strings.Contains(r.Notice, "7 days") || r.Notice == "" {
		t.Errorf("reusable with an expiry: %+v", r)
	}
	r = get("usr-limited")
	if r.ExpiresAt != 9_999_999_999_999 || r.MaxUses != 5 || r.Notice != "" {
		t.Errorf("a key that already had both caps must not change: %+v", r)
	}
	r = get("usr-revoked")
	if r.ExpiresAt != 0 || r.MaxUses != 0 || r.Notice != "" {
		t.Errorf("a revoked key is left alone: %+v", r)
	}
	r = get("adm-unlimited")
	if r.ExpiresAt != 0 || r.MaxUses != 0 || r.Notice != "" || r.Source != InvitationSourceUser {
		t.Errorf("an admin's key is unchanged: %+v", r)
	}
	// A Worker row inserted without saying anything stays closed.
	if _, err := cur.DB().Exec(`INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at) VALUES ('w2', randomblob(32), 'raw', 'usr', 'private', 1)`); err != nil {
		t.Fatal(err)
	}
	if w, _ := cur.WorkerByID(ctx, "w2"); !w.Pending {
		t.Error("a raw insert must default to pending")
	}
}
