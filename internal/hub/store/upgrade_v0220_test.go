package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// v0.23.0 adds five Hub migrations after v0.22.0's 0036: 0037 invitation
// worker_name, 0038 worker_hidden_sessions, 0039 session codes (worker_numbers
// and its trigger), 0040 session_grant.command_shell and 0041
// session_instance. These tests open them from scratch and upgrade a database
// at v0.22.0's schema with data in it.
const (
	v0220Schema = 36
	v0230Schema = 41
)

func dbSchemaVersion(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.DB().QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The embedded migrations are numbered without gaps or duplicates (branches
// that each added a "0037" are renumbered when merged), up to v0.23.0's at least.
func TestHubMigrationsSequential(t *testing.T) {
	entries, err := hubMigrations.ReadDir(migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		n++
		var v int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &v); err != nil || v != n {
			t.Fatalf("migration %q is number %d in order, want its prefix to be %04d", e.Name(), n, n)
		}
	}
	if n < v0230Schema {
		t.Fatalf("%d migrations, want at least %d", n, v0230Schema)
	}
}

func TestV0230MigrationsFromScratch(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v := dbSchemaVersion(t, s); v < v0230Schema {
		t.Fatalf("schema version = %d, want at least %d", v, v0230Schema)
	}
	for _, table := range []string{"worker_hidden_sessions", "worker_numbers", "session_codes", "session_instance"} {
		var n int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s: %d, %v", table, n, err)
		}
	}
}

func TestUpgradeFromV0220Schema(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{}
	entries, err := hubMigrations.ReadDir(migrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") || e.Name() >= "0037" {
			continue
		}
		b, err := hubMigrations.ReadFile(migrationsDir + "/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(b)
	}
	old, err := Open(dir, testMigrations(files))
	if err != nil {
		t.Fatal(err)
	}
	if v := dbSchemaVersion(t, old); v != v0220Schema {
		t.Fatalf("old schema version = %d, want %d", v, v0220Schema)
	}
	exec := func(s *Store, q string, args ...any) {
		t.Helper()
		if _, err := s.DB().Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(old, `INSERT INTO users (id, username, active, created_at, updated_at) VALUES ('usr', 'usr', 1, 1, 1)`)
	exec(old, `INSERT INTO user_roles VALUES ('usr', 'user', 'local')`)
	// Enrolled out of id order: numbers follow the enrollment.
	exec(old, `INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at) VALUES ('wb', randomblob(32), 'usr-b', 'usr', 'private', 1)`)
	exec(old, `INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at) VALUES ('wa', randomblob(32), 'usr-a', 'usr', 'private', 2)`)
	exec(old, `INSERT INTO invitation_keys (id, secret_hash, name, mode, kind, ttl, creator_id, created_at, uses, max_uses, revoked)
		VALUES ('k1', randomblob(32), 'phone', 'one_time', 'private', '1h', 'usr', 1, 0, 1, 0)`)
	exec(old, `INSERT INTO session_grant (id, source_worker_id, source_session, target_worker_id, target_session, level,
		authority, granted_by, source_owner_id, target_owner_id, created_at, updated_at)
		VALUES ('g1', 'wa', 'agent', 'wb', 'shell', 3, 'owner', 'usr', 'usr', 'usr', 1, 1)`)
	_ = old.Close()

	cur, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cur.Close() }()
	ctx := context.Background()
	if v := dbSchemaVersion(t, cur); v < v0230Schema {
		t.Fatalf("upgraded schema version = %d, want at least %d", v, v0230Schema)
	}

	// 0037: the unused one-time key names its Worker.
	var wn string
	if err := cur.DB().QueryRow(`SELECT worker_name FROM invitation_keys WHERE id = 'k1'`).Scan(&wn); err != nil || wn != "phone" {
		t.Errorf("k1 worker_name = %q, %v", wn, err)
	}
	// 0038: no Worker has its own choice; one can be set.
	if m, err := cur.ListWorkerHiddenSessions(ctx); err != nil || len(m) != 0 {
		t.Errorf("hidden sessions after the upgrade = %v, %v", m, err)
	}
	allow := true
	if err := cur.SetWorkerHiddenSessions(ctx, "wa", &allow, "usr"); err != nil {
		t.Fatal(err)
	}
	if m, err := cur.ListWorkerHiddenSessions(ctx); err != nil || !m["wa"] || len(m) != 1 {
		t.Errorf("hidden sessions = %v, %v", m, err)
	}
	// 0039: existing Workers are numbered in enrollment order, a new one next.
	nums, err := cur.WorkerNumbers(ctx)
	if err != nil || nums["wb"] != 1 || nums["wa"] != 2 {
		t.Fatalf("worker numbers = %v, %v", nums, err)
	}
	exec(cur, `INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at) VALUES ('wc', randomblob(32), 'usr-c', 'usr', 'private', 3)`)
	if nums, err = cur.WorkerNumbers(ctx); err != nil || nums["wc"] != 3 {
		t.Fatalf("worker numbers after an enrollment = %v, %v", nums, err)
	}
	w, codes, err := cur.SessionCodes(ctx, "wa", []SessionSighting{{Name: "agent", InstanceID: "$1"}}, time.Now())
	if err != nil || w != 2 || len(codes) != 1 || codes[0] != 1 {
		t.Fatalf("session codes = %d %v, %v", w, codes, err)
	}
	// 0040: the existing grant is not a command shell; a Control grant on one
	// named session may become one.
	var shell int
	if err := cur.DB().QueryRow(`SELECT command_shell FROM session_grant WHERE id = 'g1'`).Scan(&shell); err != nil || shell != 0 {
		t.Errorf("g1 command_shell = %d, %v", shell, err)
	}
	exec(cur, `UPDATE session_grant SET command_shell = 1 WHERE id = 'g1'`)
	// 0041: the last instances are kept per Worker and go with it.
	if err := cur.SaveSessionInstances(ctx, "wc", map[string]string{"agent": "$7"}); err != nil {
		t.Fatal(err)
	}
	if m, err := cur.LoadSessionInstances(ctx, "wc"); err != nil || m["agent"] != "$7" {
		t.Fatalf("instances = %v, %v", m, err)
	}
	exec(cur, `DELETE FROM workers WHERE id = 'wc'`)
	if m, err := cur.LoadSessionInstances(ctx, "wc"); err != nil || len(m) != 0 {
		t.Errorf("instances of a removed Worker = %v, %v", m, err)
	}
	// A removed Worker's number is never given again.
	exec(cur, `INSERT INTO workers (id, worker_pub, name, owner_id, kind, created_at) VALUES ('wd', randomblob(32), 'usr-d', 'usr', 'private', 4)`)
	if nums, err = cur.WorkerNumbers(ctx); err != nil || nums["wd"] != 4 {
		t.Errorf("worker numbers after a removal = %v, %v", nums, err)
	}
}
