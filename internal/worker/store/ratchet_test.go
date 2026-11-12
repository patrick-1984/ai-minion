package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func hash32(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// embeddedUpTo returns the embedded migrations with a number below limit: the
// schema of an older release.
func embeddedUpTo(t *testing.T, limit string) map[string]string {
	t.Helper()
	entries, err := workerMigrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range entries {
		if e.Name() >= limit {
			continue
		}
		b, err := workerMigrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(b)
	}
	return files
}

func TestMigration0009KeepsA063EnrollmentWithAnEmptyRatchet(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old := embeddedUpTo(t, "0009")
	if _, ok := old["0008_audit_origin.sql"]; !ok || len(old) != 8 {
		t.Fatalf("fixture schema = %d files", len(old))
	}
	s := openStore(t, dir, testMigrations(old))
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO hub_enrollment (id, hub_pub, pin_seq, worker_id, worker_name, owner, kind, enrolled_at, updated_at)
		 VALUES (1, ?, 2, 'w-1', 'pi', 'alice', 'private', 5, 5)`, pubKey(7)); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s = openStore(t, dir)
	got, err := s.LoadEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxSerial != 0 || got.CertSHA256 != nil || got.PinSeq != 2 || got.WorkerID != "w-1" || !bytes.Equal(got.HubPub, pubKey(7)) {
		t.Fatalf("migrated enrollment = %+v", got)
	}
	if _, err := s.LoadPairResume(ctx); !errors.Is(err, ErrNoPairResume) {
		t.Fatalf("LoadPairResume = %v", err)
	}
}

func TestUpdatePinRatchetIsAtomicAndNeverLowers(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(1), WorkerID: "w"}); err != nil {
		t.Fatal(err)
	}
	load := func() Enrollment {
		t.Helper()
		e, err := s.LoadEnrollment(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	// Only the serial rose: a commit is needed and made.
	if err := s.UpdatePinRatchet(ctx, pubKey(1), 0, 5, hash32(5)); err != nil {
		t.Fatal(err)
	}
	if e := load(); e.MaxSerial != 5 || !bytes.Equal(e.CertSHA256, hash32(5)) {
		t.Fatalf("after raise = %+v", e)
	}
	// A lower serial changes nothing; an equal one keeps the stored hash.
	if err := s.UpdatePinRatchet(ctx, pubKey(1), 0, 3, hash32(3)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePinRatchet(ctx, pubKey(1), 0, 5, hash32(9)); err != nil {
		t.Fatal(err)
	}
	if e := load(); e.MaxSerial != 5 || !bytes.Equal(e.CertSHA256, hash32(5)) {
		t.Fatalf("after lower/equal = %+v", e)
	}
	// A pin regression is refused and writes nothing, ratchet included.
	if err := s.UpdatePinRatchet(ctx, pubKey(2), 0, 99, hash32(9)); !errors.Is(err, ErrPinRegression) {
		t.Fatalf("regression = %v", err)
	}
	if e := load(); e.MaxSerial != 5 {
		t.Fatalf("a refused update wrote %+v", e)
	}
	// A verified transition: new root, new scope (also lower than before).
	if err := s.UpdatePinRatchet(ctx, pubKey(2), 1, 2, hash32(2)); err != nil {
		t.Fatal(err)
	}
	if e := load(); !bytes.Equal(e.HubPub, pubKey(2)) || e.PinSeq != 1 || e.MaxSerial != 2 || !bytes.Equal(e.CertSHA256, hash32(2)) {
		t.Fatalf("after transition = %+v", e)
	}
	if err := s.UpdatePinRatchet(ctx, pubKey(2), 1, 2, hash32(2)); err != nil {
		t.Fatal(err)
	}
}

func TestUpdatePinRatchetFirstHashAtEqualSerial(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	_ = s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(1), MaxSerial: 4})
	if err := s.UpdatePinRatchet(ctx, pubKey(1), 0, 4, hash32(4)); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.LoadEnrollment(ctx); !bytes.Equal(e.CertSHA256, hash32(4)) {
		t.Fatalf("hash = %x", e.CertSHA256)
	}
	if err := s.UpdatePinRatchet(ctx, pubKey(1), 0, 4, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePinRatchet(ctx, nil, 0, 1, nil); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("short key = %v", err)
	}
	if err := s.UpdatePinRatchet(ctx, pubKey(1), 0, 1, []byte("x")); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("short hash = %v", err)
	}
}

func TestPairResumeRoundTripAndDeletedByEnrollmentAndReset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openStore(t, dir)
	rec := PairResume{Code: "MN-AAAA-BBBB", PairingID: "AAAA", ExpiresAt: time.UnixMilli(1_800_000_000_000),
		RootPub: pubKey(3), Serial: 7, CertSHA256: hash32(7)}
	if err := s.SavePairResume(ctx, rec); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s = openStore(t, dir)
	got, err := s.LoadPairResume(ctx)
	if err != nil || got.Code != rec.Code || got.Serial != 7 || !bytes.Equal(got.RootPub, pubKey(3)) ||
		!got.ExpiresAt.Equal(rec.ExpiresAt) || got.ChainPosition != 0 || !bytes.Equal(got.CertSHA256, hash32(7)) {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, bad := range []PairResume{{}, {Code: "c", PairingID: "p", RootPub: pubKey(1), Serial: 0, CertSHA256: hash32(1)}} {
		if err := s.SavePairResume(ctx, bad); err == nil {
			t.Fatal("an incomplete record was stored")
		}
	}
	// The enrollment commit deletes it in the same transaction.
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(3), MaxSerial: 7, CertSHA256: hash32(7)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadPairResume(ctx); !errors.Is(err, ErrNoPairResume) {
		t.Fatalf("after enrollment: %v", err)
	}
	// reset deletes it
	_ = s.SavePairResume(ctx, rec)
	if _, err := s.ResetPairing(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadPairResume(ctx); !errors.Is(err, ErrNoPairResume) {
		t.Fatalf("after reset: %v", err)
	}
	if ok, err := s.DeletePairResume(ctx); err != nil || ok {
		t.Fatalf("delete of nothing = %v %v", ok, err)
	}
}
