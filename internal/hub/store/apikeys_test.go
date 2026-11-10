package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// keyFixture opens a store with one admin and returns it with the admin's id.
func keyFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s := openTest(t)
	p := seedParams("admin")
	if _, err := s.SeedAdmin(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return s, p.User.ID
}

func sampleKey(owner string, hashByte byte, created int64) APIKeyRow {
	return APIKeyRow{
		ID: NewID(), OwnerID: owner, Name: "ci bot",
		Hash: bytes.Repeat([]byte{hashByte}, 32), Hint: "abcdefgh",
		Actions:   []string{"operate", "read"},
		CreatedAt: created,
		Scopes: []APIKeyScopeRow{
			{Kind: "worker", WorkerID: NewID()},
			{Kind: "session", WorkerID: NewID(), SessionName: "minion-1"},
		},
	}
}

func TestCreateAPIKeyStoresHashAndHintAndReadsItBack(t *testing.T) {
	s, owner := keyFixture(t)
	ctx := context.Background()
	k := sampleKey(owner, 1, 100)
	k.BoundWorkerID, k.ExpiresAt = NewID(), 9_000
	audit := &AuditRow{ID: NewID(), TS: 100, Actor: owner, Action: "apikey.create", Target: k.ID, Outcome: "success"}
	if err := s.CreateAPIKey(ctx, k, audit); err != nil {
		t.Fatal(err)
	}
	for name, get := range map[string]func() (APIKeyRow, error){
		"by id":   func() (APIKeyRow, error) { return s.APIKeyByID(ctx, k.ID) },
		"by hash": func() (APIKeyRow, error) { return s.APIKeyByHash(ctx, k.Hash) },
	} {
		got, err := get()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Scope rows come back sorted by kind, user, worker, session.
		want := k
		want.Scopes = append([]APIKeyScopeRow(nil), k.Scopes...)
		if want.Scopes[0].Kind == "worker" && want.Scopes[1].Kind == "session" {
			want.Scopes[0], want.Scopes[1] = want.Scopes[1], want.Scopes[0]
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
	}
	rows, err := s.ListAudit(ctx, AuditQuery{Action: "apikey.create", Limit: 5})
	if err != nil || len(rows) != 1 || rows[0].Target != k.ID {
		t.Fatalf("audit rows = %+v, %v", rows, err)
	}
}

func TestAPIKeyTableHoldsOnlyHashAndHint(t *testing.T) {
	s, owner := keyFixture(t)
	ctx := context.Background()
	const secret = "mn_thisisthekeytextthatmustneverbestoredanywhere"
	k := sampleKey(owner, 2, 1)
	k.Hash, k.Hint = []byte(strings.Repeat("h", 32)), secret[3:11]
	if err := s.CreateAPIKey(ctx, k, nil); err != nil {
		t.Fatal(err)
	}
	// Dump every column of both tables: the key text appears nowhere; only
	// its 8-character hint does.
	for _, q := range []string{`SELECT * FROM api_keys`, `SELECT * FROM api_key_scopes`} {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				var text string
				switch x := v.(type) {
				case []byte:
					text = string(x)
				case string:
					text = x
				}
				if strings.Contains(text, secret) {
					t.Errorf("%s.%s holds the key text", q, cols[i])
				}
			}
		}
		rows.Close()
	}
}

func TestCreateAPIKeyIsAtomic(t *testing.T) {
	s, owner := keyFixture(t)
	ctx := context.Background()
	// A scope row that violates the table's CHECK rolls the key back too, and
	// so does the audit row.
	bad := sampleKey(owner, 3, 1)
	bad.Scopes = append(bad.Scopes, APIKeyScopeRow{Kind: "user", UserID: ""})
	audit := &AuditRow{ID: NewID(), TS: 1, Actor: owner, Action: "apikey.create", Target: bad.ID, Outcome: "success"}
	if err := s.CreateAPIKey(ctx, bad, audit); err == nil {
		t.Fatal("a key with an invalid scope row was stored")
	}
	if _, err := s.APIKeyByID(ctx, bad.ID); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("the key survived its failed grants: %v", err)
	}
	if rows, _ := s.ListAudit(ctx, AuditQuery{Action: "apikey.create", Limit: 5}); len(rows) != 0 {
		t.Errorf("the audit row of a failed create survived: %+v", rows)
	}
	// A key without any scope row is refused up front.
	none := sampleKey(owner, 4, 1)
	none.Scopes = nil
	if err := s.CreateAPIKey(ctx, none, nil); err == nil {
		t.Error("a key without scope was stored")
	}
	// A failing audit row (duplicate id) rolls the key back.
	good := sampleKey(owner, 5, 1)
	dup := &AuditRow{ID: NewID(), TS: 1, Actor: "x", Action: "a", Outcome: "success"}
	if err := s.InsertAudit(ctx, nil, *dup); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAPIKey(ctx, good, dup); err == nil {
		t.Fatal("create succeeded with a clashing audit id")
	}
	if _, err := s.APIKeyByID(ctx, good.ID); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("the key survived a failed audit write: %v", err)
	}
}

func TestAPIKeyHashIsUniqueAndMustBe32Bytes(t *testing.T) {
	s, owner := keyFixture(t)
	ctx := context.Background()
	a := sampleKey(owner, 6, 1)
	if err := s.CreateAPIKey(ctx, a, nil); err != nil {
		t.Fatal(err)
	}
	dup := sampleKey(owner, 6, 2)
	if err := s.CreateAPIKey(ctx, dup, nil); err == nil {
		t.Error("a duplicate hash was accepted")
	}
	short := sampleKey(owner, 7, 3)
	short.Hash = []byte("short")
	if err := s.CreateAPIKey(ctx, short, nil); err == nil {
		t.Error("a short hash was accepted")
	}
	badHint := sampleKey(owner, 8, 3)
	badHint.Hint = "abc"
	if err := s.CreateAPIKey(ctx, badHint, nil); err == nil {
		t.Error("a short hint was accepted")
	}
	if _, err := s.APIKeyByHash(ctx, bytes.Repeat([]byte{99}, 32)); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("unknown hash: %v", err)
	}
	if _, err := s.APIKeyByID(ctx, NewID()); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestListAPIKeysFiltersByOwnerNewestFirst(t *testing.T) {
	s, admin := keyFixture(t)
	ctx := context.Background()
	other := NewID()
	if _, err := s.db.Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, 'bob', 1, 1)`, other); err != nil {
		t.Fatal(err)
	}
	k1, k2, k3 := sampleKey(admin, 10, 100), sampleKey(admin, 11, 300), sampleKey(other, 12, 200)
	for _, k := range []APIKeyRow{k1, k2, k3} {
		if err := s.CreateAPIKey(ctx, k, nil); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(rows []APIKeyRow) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	mine, err := s.ListAPIKeys(ctx, admin)
	if err != nil || !reflect.DeepEqual(ids(mine), []string{k2.ID, k1.ID}) {
		t.Errorf("admin keys = %v, %v", ids(mine), err)
	}
	all, err := s.ListAPIKeys(ctx, "")
	if err != nil || !reflect.DeepEqual(ids(all), []string{k2.ID, k3.ID, k1.ID}) {
		t.Errorf("all keys = %v, %v", ids(all), err)
	}
	if len(mine[0].Scopes) != 2 {
		t.Errorf("listed key lost its scope rows: %+v", mine[0])
	}
	none, err := s.ListAPIKeys(ctx, NewID())
	if err != nil || len(none) != 0 {
		t.Errorf("unknown owner = %v, %v", none, err)
	}
}

func TestRevokeAPIKeyIsIdempotentAndAuditedOnce(t *testing.T) {
	s, owner := keyFixture(t)
	ctx := context.Background()
	k := sampleKey(owner, 20, 1)
	if err := s.CreateAPIKey(ctx, k, nil); err != nil {
		t.Fatal(err)
	}
	audit := func() *AuditRow {
		return &AuditRow{ID: NewID(), TS: 5, Actor: owner, Action: "apikey.revoke", Target: k.ID, Outcome: "success"}
	}
	changed, err := s.RevokeAPIKey(ctx, k.ID, 500, audit())
	if err != nil || !changed {
		t.Fatalf("first revoke = %v, %v", changed, err)
	}
	changed, err = s.RevokeAPIKey(ctx, k.ID, 900, audit())
	if err != nil || changed {
		t.Fatalf("second revoke = %v, %v", changed, err)
	}
	got, err := s.APIKeyByID(ctx, k.ID)
	if err != nil || got.RevokedAt != 500 {
		t.Fatalf("revoked_at = %d, %v; want the first revocation time", got.RevokedAt, err)
	}
	if rows, _ := s.ListAudit(ctx, AuditQuery{Action: "apikey.revoke", Limit: 5}); len(rows) != 1 {
		t.Errorf("revoke audit rows = %d, want 1", len(rows))
	}
	if _, err := s.RevokeAPIKey(ctx, NewID(), 1, nil); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("unknown key: %v", err)
	}
}

func TestRevokeKeysBoundToAWorkerAndOfAnOwner(t *testing.T) {
	s, owner := keyFixture(t)
	ctx := context.Background()
	bound := NewID()
	a, b, c := sampleKey(owner, 30, 1), sampleKey(owner, 31, 2), sampleKey(owner, 32, 3)
	a.BoundWorkerID, b.BoundWorkerID = bound, bound
	for _, k := range []APIKeyRow{a, b, c} {
		if err := s.CreateAPIKey(ctx, k, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Inside the caller's transaction, as the reassignment uses it.
	var n int64
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = s.RevokeAPIKeysBoundToWorker(ctx, tx, bound, 700)
		return err
	})
	if err != nil || n != 2 {
		t.Fatalf("revoked = %d, %v; want 2", n, err)
	}
	for _, k := range []APIKeyRow{a, b} {
		if got, _ := s.APIKeyByID(ctx, k.ID); got.RevokedAt != 700 {
			t.Errorf("bound key %s not revoked", k.ID)
		}
	}
	if got, _ := s.APIKeyByID(ctx, c.ID); got.RevokedAt != 0 {
		t.Error("an unbound key was revoked")
	}
	// A rolled back transaction revokes nothing.
	sentinel := errors.New("rollback")
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := s.RevokeAPIKeysOfOwner(ctx, tx, owner, 800); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if got, _ := s.APIKeyByID(ctx, c.ID); got.RevokedAt != 0 {
		t.Error("a rolled back revocation took effect")
	}
	n, err = s.RevokeAPIKeysOfOwner(ctx, nil, owner, 900)
	if err != nil || n != 1 {
		t.Fatalf("owner revoke = %d, %v; want only the one live key", n, err)
	}
	if got, _ := s.APIKeyByID(ctx, a.ID); got.RevokedAt != 700 {
		t.Errorf("an already revoked key changed its revocation time: %d", got.RevokedAt)
	}
}

func TestAPIKeysAreDeletedWithTheirOwner(t *testing.T) {
	s, _ := keyFixture(t)
	ctx := context.Background()
	owner := NewID()
	if _, err := s.db.Exec(`INSERT INTO users (id, username, created_at, updated_at) VALUES (?, 'carol', 1, 1)`, owner); err != nil {
		t.Fatal(err)
	}
	k := sampleKey(owner, 40, 1)
	if err := s.CreateAPIKey(ctx, k, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.APIKeyByID(ctx, k.ID); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("key outlived its owner: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM api_key_scopes`).Scan(&n); err != nil || n != 0 {
		t.Errorf("scope rows left behind: %d, %v", n, err)
	}
}

func TestAPIKeyForAnUnknownOwnerIsRefused(t *testing.T) {
	s, _ := keyFixture(t)
	if err := s.CreateAPIKey(context.Background(), sampleKey(NewID(), 50, 1), nil); err == nil {
		t.Error("a key for a user that does not exist was stored")
	}
}

func TestTouchAPIKeyRecordsTheLastUseAndThrottlesWrites(t *testing.T) {
	s, owner := keyFixture(t)
	ctx := context.Background()
	k := sampleKey(owner, 7, 100)
	if err := s.CreateAPIKey(ctx, k, nil); err != nil {
		t.Fatal(err)
	}
	lastUsed := func() int64 {
		t.Helper()
		got, err := s.APIKeyByID(ctx, k.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.LastUsedAt
	}
	if lastUsed() != 0 {
		t.Fatalf("a new key has last_used_at = %d", lastUsed())
	}
	const t0 = int64(1_000_000)
	step := func(name string, at int64, wantWrite bool, wantLast int64) {
		t.Helper()
		wrote, err := s.TouchAPIKey(ctx, k.ID, at)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if wrote != wantWrite || lastUsed() != wantLast {
			t.Errorf("%s: wrote = %v, last_used_at = %d; want %v, %d", name, wrote, lastUsed(), wantWrite, wantLast)
		}
	}
	step("first use is written at once", t0, true, t0)
	step("a use within the granularity is not written", t0+LastUseGranularityMS-1, false, t0)
	step("a use after the granularity is written", t0+LastUseGranularityMS, true, t0+LastUseGranularityMS)
	step("an older time never moves it back", t0, false, t0+LastUseGranularityMS)

	// The listing reads it too, and an unknown key writes nothing.
	list, err := s.ListAPIKeys(ctx, owner)
	if err != nil || len(list) != 1 || list[0].LastUsedAt != t0+LastUseGranularityMS {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if wrote, err := s.TouchAPIKey(ctx, NewID(), t0); err != nil || wrote {
		t.Errorf("unknown key: wrote = %v, err = %v", wrote, err)
	}
}
