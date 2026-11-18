package store

import (
	"context"
	"errors"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

func TestHubCheckDefaultsToNone(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(1)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadEnrollment(ctx)
	if err != nil || got.HubCheck != HubCheckNone || !got.HubCheckAt.IsZero() || got.HubCheckAnchor != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestHubCheckSavedAndResetByANewEnrollment(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(1), HubCheck: HubCheckFingerprint, HubCheckAnchor: "a"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadEnrollment(ctx)
	if err != nil || got.HubCheck != HubCheckFingerprint || got.HubCheckAt.IsZero() || got.HubCheckAnchor != "a" {
		t.Fatalf("got %+v, %v", got, err)
	}
	// A pin update keeps the check.
	if err := s.UpdatePinRatchet(ctx, pubKey(2), 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.LoadEnrollment(ctx); got.HubCheck != HubCheckFingerprint || got.HubCheckAnchor != "a" {
		t.Fatalf("after a pin update: %+v", got)
	}
	// A new enrollment resets it.
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(3)}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.LoadEnrollment(ctx); got.HubCheck != HubCheckNone || got.HubCheckAnchor != "" || !got.HubCheckAt.IsZero() {
		t.Fatalf("after a new enrollment: %+v", got)
	}
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(3), HubCheck: "bogus"}); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("unknown check: %v", err)
	}
}

func TestSetHubCheckOnlyForThePinnedKey(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	if err := s.SetHubCheck(ctx, pubKey(1), HubCheckFingerprint, ""); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("not enrolled: %v", err)
	}
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(1)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetHubCheck(ctx, pubKey(2), HubCheckMismatch, ""); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("another root: %v", err)
	}
	if got, _ := s.LoadEnrollment(ctx); got.HubCheck != HubCheckNone {
		t.Fatalf("another root changed the check: %+v", got)
	}
	if err := s.SetHubCheck(ctx, pubKey(1), HubCheckMismatch, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoadEnrollment(ctx); got.HubCheck != HubCheckMismatch || got.HubCheckAt.IsZero() {
		t.Fatalf("got %+v", got)
	}
	if err := s.SetHubCheck(ctx, pubKey(1), "nope", ""); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("unknown check: %v", err)
	}
}

// A database at schema 9 (v0.9.3 and v0.10.0) with an enrollment migrates to
// hub_check none.
func TestHubCheckMigrationFromSchema9(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	all := map[string]string{}
	entries, err := workerMigrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() >= "0010" {
			continue
		}
		body, err := workerMigrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		all[e.Name()] = string(body)
	}
	old := openStore(t, dir, testMigrations(all))
	if err := old.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(1)}); err == nil {
		t.Fatal("the schema-9 store must not know hub_check")
	}
	if _, err := old.db.ExecContext(ctx,
		`INSERT INTO hub_enrollment (id, hub_pub, pin_seq, worker_id, worker_name, owner, kind, preview_only, enrolled_at, updated_at)
		 VALUES (1, ?, 0, 'w', 'n', 'o', 'private', 0, 1, 1)`, []byte(pubKey(1))); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	s := openStore(t, dir)
	got, err := s.LoadEnrollment(ctx)
	if err != nil || got.HubCheck != HubCheckNone || got.HubCheckAnchor != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// The lineage records the keys a checked pin moved through (the old pin and
// the keys passed in one chain); a new anchor empties it, an unchecked
// enrollment has none.
func TestHubCheckLineageFollowsVerifiedPinMoves(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	fp := func(n byte) string {
		f, err := identity.FingerprintOf(pubKey(n))
		if err != nil {
			t.Fatal(err)
		}
		return string(f)
	}
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(1), HubCheck: HubCheckFingerprint, HubCheckAnchor: fp(1)}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePinRatchet(ctx, pubKey(2), 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePinRatchet(ctx, pubKey(4), 3, 1, nil, pubKey(3)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LoadEnrollment(ctx)
	if want := fp(1) + " " + fp(2) + " " + fp(3); got.HubCheckLineage != want {
		t.Fatalf("lineage = %q, want %q", got.HubCheckLineage, want)
	}
	// The same anchor (a mismatch keeps it) keeps the lineage; a new one
	// empties it.
	if err := s.SetHubCheck(ctx, pubKey(4), HubCheckMismatch, fp(1)); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.LoadEnrollment(ctx); got.HubCheckLineage == "" {
		t.Fatal("the same anchor must keep the lineage")
	}
	if err := s.SetHubCheck(ctx, pubKey(4), HubCheckFingerprint, fp(4)); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.LoadEnrollment(ctx); got.HubCheckLineage != "" {
		t.Fatalf("a new anchor must empty the lineage: %q", got.HubCheckLineage)
	}
	// No anchor, no lineage.
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(5)}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePin(ctx, pubKey(6), 1); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.LoadEnrollment(ctx); got.HubCheckLineage != "" {
		t.Fatalf("unchecked: %q", got.HubCheckLineage)
	}
}
