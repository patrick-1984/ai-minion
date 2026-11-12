package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// openBefore opens a database at the end of the schema before migration
// number (a "NNNN" prefix), the shape of a released 0.6.3 Hub database.
func openBefore(t *testing.T, dir, number string) *Store {
	t.Helper()
	files := map[string]string{}
	entries, err := hubMigrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") || e.Name() >= number {
			continue
		}
		b, err := hubMigrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(b)
	}
	s, err := Open(dir, testMigrations(files))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMigration0021OnAV063DatabaseWithIdentity(t *testing.T) {
	dir := t.TempDir()
	old := openBefore(t, dir, "0021")
	ctx := context.Background()
	if err := old.PutHubIdentity(ctx, HubIdentityRow{Pub: pub(7), Fingerprint: "fp7", CreatedAt: 42}); err != nil {
		t.Fatal(err)
	}
	if err := old.RotateHubIdentity(ctx, RotateParams{Expect: pub(7), Transition: link(1, 7, 8), Identity: HubIdentityRow{Pub: pub(8), Fingerprint: "fp8", CreatedAt: 1000}}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	row, ok, err := s.HubIdentityV2(ctx)
	if err != nil || !ok {
		t.Fatalf("row: %v %v", err, ok)
	}
	if !bytes.Equal(row.Pub, pub(8)) || row.Fingerprint != "fp8" || row.CreatedAt != 1000 {
		t.Fatalf("the 0.6 identity changed: %+v", row)
	}
	if row.RootSealed || row.RootFileSHA256 != nil || row.CurrentCert != nil || row.CurrentSerial != 0 || row.LastIssued != 0 || !row.MigratedFrom06 {
		t.Fatalf("defaults: %+v", row)
	}
	if row.Web != (ScopeState{}) || row.CLI != (ScopeState{}) || row.LastOp != "" || row.ExpiryWarnDay != 0 {
		t.Fatalf("scope defaults: %+v", row)
	}
	if chain, err := s.Transitions(ctx); err != nil || len(chain) != 1 {
		t.Fatalf("the chain survived: %v %v", chain, err)
	}
}

func TestMigration0021OnAV063DatabaseWithoutIdentity(t *testing.T) {
	dir := t.TempDir()
	old := openBefore(t, dir, "0021")
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, ok, err := s.HubIdentityV2(context.Background()); err != nil || ok {
		t.Fatalf("a never-started Hub has a row: %v %v", ok, err)
	}
	// A row created now is a sealed new Hub, not a migrated one.
	if err := s.CreateSealedIdentity(context.Background(), HubIdentityRow{Pub: pub(1), Fingerprint: "f", CreatedAt: 1}, bytes.Repeat([]byte{9}, 32), 5, nil); err != nil {
		t.Fatal(err)
	}
	row, _, _ := s.HubIdentityV2(context.Background())
	if !row.RootSealed || row.MigratedFrom06 || row.LastOp != "passphrase_set" {
		t.Fatalf("row %+v", row)
	}
	if err := s.CreateSealedIdentity(context.Background(), HubIdentityRow{Pub: pub(2), Fingerprint: "f", CreatedAt: 1}, bytes.Repeat([]byte{9}, 32), 5, nil); !errors.Is(err, ErrIdentityState) {
		t.Fatalf("second create: %v", err)
	}
}

func TestUnsealFailureTransitionsAndGate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedIdentity(t, s, 1)
	now := int64(1_000_000)
	// Failures 1..4 no lock; 5 locks 15 min; 6..9 keep it; 10 locks 30 min; then 60, 120, 240, 240.
	wantLock := map[int64]int64{5: 15, 10: 30, 15: 60, 20: 120, 25: 240, 30: 240, 35: 240}
	for f := int64(1); f <= 35; f++ {
		st, err := s.RecordUnsealFailure(ctx, ScopeCLI, now, "a", "cli")
		if err != nil {
			t.Fatal(err)
		}
		if m, ok := wantLock[f]; ok {
			if st.LockedUntil != now+m*60_000 {
				t.Fatalf("failure %d: locked until +%d ms, want %d min", f, st.LockedUntil-now, m)
			}
			now = st.LockedUntil
		}
		if st.Failures != f {
			t.Fatalf("failures = %d", st.Failures)
		}
	}
	// The web scope is untouched.
	row, _, _ := s.HubIdentityV2(ctx)
	if row.Web != (ScopeState{}) {
		t.Fatalf("web: %+v", row.Web)
	}
	// Gate: locked refuses without counting; unlocked counts and refuses past the allowance.
	var le *LimitError
	if err := s.BeginUnsealAttempt(ctx, ScopeCLI, now-1, 10); !errors.As(err, &le) || !le.Locked || le.Until != now || !errors.Is(err, ErrUnsealLocked) {
		t.Fatalf("locked gate: %v", err)
	}
	row, _, _ = s.HubIdentityV2(ctx)
	if row.CLI.WindowCount != 0 {
		t.Fatal("a locked request consumed allowance")
	}
	for i := 0; i < 3; i++ {
		if err := s.BeginUnsealAttempt(ctx, ScopeWeb, now, 3); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BeginUnsealAttempt(ctx, ScopeWeb, now+AllowanceWindow-1, 3); !errors.Is(err, ErrUnsealAllowance) {
		t.Fatalf("allowance: %v", err)
	}
	if err := s.BeginUnsealAttempt(ctx, ScopeWeb, now+AllowanceWindow, 3); err != nil {
		t.Fatalf("new window: %v", err)
	}
	if err := s.BeginUnsealAttempt(ctx, "tty", now, 3); err == nil {
		t.Fatal("unknown scope")
	}
}

func TestCommitIssuanceIsAtomicWithItsAuditRows(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.CreateSealedIdentity(ctx, HubIdentityRow{Pub: pub(1), Fingerprint: "f", CreatedAt: 1}, bytes.Repeat([]byte{1}, 32), 1, nil); err != nil {
		t.Fatal(err)
	}
	cert := bytes.Repeat([]byte{3}, 153)
	good := AuditRow{ID: NewID(), TS: 1, Actor: "a", Action: "hub_identity.reissue", Outcome: "success"}
	dup := []AuditRow{good, good} // the second insert violates the primary key
	err := s.CommitIssuance(ctx, IssuanceParams{ExpectPub: pub(1), Serial: 10, Cert: cert, NowMS: 2, Scope: ScopeWeb, Op: "reissue", Audit: dup})
	if err == nil {
		t.Fatal("no error")
	}
	row, _, _ := s.HubIdentityV2(ctx)
	if row.CurrentCert != nil || row.LastIssued != 0 {
		t.Fatal("the certificate committed without its audit row")
	}
	if rows, _ := s.ListAudit(ctx, AuditQuery{Limit: 10}); len(rows) != 0 {
		t.Fatalf("audit rows leaked: %d", len(rows))
	}
	good.ID = NewID()
	if err := s.CommitIssuance(ctx, IssuanceParams{ExpectPub: pub(1), Serial: 10, Cert: cert, NowMS: 2, Op: "reissue", Audit: []AuditRow{good}}); err != nil {
		t.Fatal(err)
	}
	// last_issued never goes down: the same or a lower serial is refused.
	for _, serial := range []int64{10, 9} {
		if err := s.CommitIssuance(ctx, IssuanceParams{ExpectPub: pub(1), Serial: serial, Cert: cert, NowMS: 2, Op: "reissue"}); !errors.Is(err, ErrIdentityState) {
			t.Fatalf("serial %d: %v", serial, err)
		}
	}
	// A foreign root is refused.
	if err := s.CommitIssuance(ctx, IssuanceParams{ExpectPub: pub(2), Serial: 11, Cert: cert, NowMS: 2, Op: "reissue"}); !errors.Is(err, ErrIdentityState) {
		t.Fatalf("foreign root: %v", err)
	}
	// The daily event is written once per day and the expiry once per serial.
	for i, want := range []bool{true, false, true} {
		day := []int64{20000, 20000, 20001}[i]
		got, err := s.MarkExpiryWarning(ctx, day, AuditRow{ID: NewID(), TS: 1, Actor: "system:identity-expiry", Action: "hub_identity.expiry_warning", Outcome: "success"})
		if err != nil || got != want {
			t.Fatalf("warning %d: %v %v", i, got, err)
		}
	}
	if rows, _ := s.ListAudit(ctx, AuditQuery{Action: "hub_identity.expiry_warning", Limit: 10}); len(rows) != 2 {
		t.Fatalf("warning rows %d", len(rows))
	}
}
