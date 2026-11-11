package store

import (
	"context"
	"strings"
	"testing"
)

// A v0.5 database (0018 state) with IdP admins that own unlimited and reusable
// invitations migrates through 0019 and 0020 and the startup reconciliation:
// admins whose refresh token is damaged or empty lose the grant and their
// invitations get the legacy caps; an admin with a valid token keeps them.
func TestDemotedIdPAdminsInvitationsAreCappedAtStartup(t *testing.T) {
	files := map[string]string{}
	entries, err := hubMigrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") || e.Name() >= "0019" {
			continue
		}
		b, err := hubMigrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(b)
	}
	dir := t.TempDir()
	old, err := Open(dir, testMigrations(files))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db := old.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	damaged, err := old.Box().Seal(identitiesTable, identitiesTokenCol, "id-damaged", []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	damaged[len(damaged)-1] ^= 0xff
	empty, err := old.Box().Seal(identitiesTable, identitiesTokenCol, "id-empty", nil)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := old.Box().Seal(identitiesTable, identitiesTokenCol, "id-valid", []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, u := range []struct {
		name string
		tok  []byte
	}{{"damaged", damaged}, {"empty", empty}, {"valid", valid}} {
		exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, ?, 1, 1)`, "u-"+u.name, u.name)
		exec(`INSERT INTO user_roles (user_id, role, source) VALUES (?, 'admin', 'idp')`, "u-"+u.name)
		exec(`INSERT INTO external_identities (id, user_id, issuer, subject, refresh_token_enc, created_at, last_login_at) VALUES (?, ?, 'iss', ?, ?, 1, 1)`,
			"id-"+u.name, "u-"+u.name, u.name, u.tok)
		for _, k := range []struct{ mode, ttl string }{{"one_time", "unlimited"}, {"reusable", "unlimited"}} {
			n++
			var maxUses any
			if k.mode == "one_time" {
				maxUses = 1
			}
			exec(`INSERT INTO invitation_keys (id, secret_hash, mode, kind, ttl, creator_id, created_at, uses, max_uses) VALUES (?, randomblob(32), ?, 'private', ?, ?, 1, 3, ?)`,
				"k-"+u.name+"-"+k.mode, k.mode, k.ttl, "u-"+u.name, maxUses)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 2; round++ { // the second start changes nothing more
		cur, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"damaged", "empty", "valid"} {
			for _, mode := range []string{"one_time", "reusable"} {
				var exp, maxUses *int64
				var notice string
				if err := cur.DB().QueryRowContext(ctx, `SELECT expires_at, max_uses, notice FROM invitation_keys WHERE id = ?`, "k-"+name+"-"+mode).Scan(&exp, &maxUses, &notice); err != nil {
					t.Fatal(err)
				}
				capped := name != "valid"
				if capped {
					if exp == nil || !strings.Contains(notice, "no expiry") {
						t.Fatalf("round %d %s/%s: expiry %v notice %q", round, name, mode, exp, notice)
					}
					if mode == "reusable" && (maxUses == nil || *maxUses != 13 || !strings.Contains(notice, "no use limit")) {
						t.Fatalf("round %d %s/%s: max_uses %v notice %q", round, name, mode, maxUses, notice)
					}
				} else if exp != nil || notice != "" || (mode == "reusable" && maxUses != nil) {
					t.Fatalf("round %d valid admin's key changed: %v %v %q", round, exp, maxUses, notice)
				}
			}
		}
		if err := cur.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
