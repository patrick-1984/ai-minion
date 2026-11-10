package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

func pubKey(b byte) ed25519.PublicKey { return bytes.Repeat([]byte{b}, ed25519.PublicKeySize) }

func TestEnrollmentIsAbsentOnANewDatabase(t *testing.T) {
	s := openStore(t, t.TempDir())
	if _, err := s.LoadEnrollment(context.Background()); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("LoadEnrollment = %v, want ErrNotEnrolled", err)
	}
}

func TestEnrollmentRoundTripSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clock := Clock(func() time.Time { return time.UnixMilli(1_700_000_000_123) })
	s, err := Open(dir, WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	in := Enrollment{HubPub: pubKey(1), PinSeq: 3, WorkerID: "w-1", Name: "pi", Owner: "alice", Kind: "private", PreviewOnly: true}
	if err := s.SaveEnrollment(ctx, in); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s = openStore(t, dir)
	got, err := s.LoadEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := in
	want.EnrolledAt = time.UnixMilli(1_700_000_000_123).UTC()
	if !bytes.Equal(got.HubPub, want.HubPub) || got.PinSeq != want.PinSeq || got.WorkerID != want.WorkerID ||
		got.Name != want.Name || got.Owner != want.Owner || got.Kind != want.Kind ||
		got.PreviewOnly != want.PreviewOnly || !got.EnrolledAt.Equal(want.EnrolledAt) {
		t.Fatalf("after reopen = %+v, want %+v", got, want)
	}
}

func TestSaveEnrollmentReplacesTheOldOne(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(1), WorkerID: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(2), WorkerID: "new"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadEnrollment(ctx)
	if err != nil || got.WorkerID != "new" || !bytes.Equal(got.HubPub, pubKey(2)) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestSaveEnrollmentRejectsAShortKey(t *testing.T) {
	s := openStore(t, t.TempDir())
	err := s.SaveEnrollment(context.Background(), Enrollment{HubPub: []byte{1, 2, 3}})
	if !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("err = %v, want ErrInvalidEnrollment", err)
	}
	if err := s.UpdatePin(context.Background(), []byte{1}, 1); !errors.Is(err, ErrInvalidEnrollment) {
		t.Fatalf("UpdatePin err = %v, want ErrInvalidEnrollment", err)
	}
}

func TestUpdatePinMovesForwardOnly(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	if err := s.UpdatePin(ctx, pubKey(1), 1); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("UpdatePin without enrollment = %v, want ErrNotEnrolled", err)
	}
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(1), PinSeq: 0, WorkerID: "w"}); err != nil {
		t.Fatal(err)
	}
	// Seq 0 means "unknown": the first chain link may be any seq >= 1.
	if err := s.UpdatePin(ctx, pubKey(2), 2); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePin(ctx, pubKey(3), 3); err != nil {
		t.Fatal(err)
	}
	// The same pin again is a no-op.
	if err := s.UpdatePin(ctx, pubKey(3), 3); err != nil {
		t.Fatalf("same pin: %v", err)
	}
	// Going back or sideways is refused and changes nothing.
	for _, c := range []struct {
		pub ed25519.PublicKey
		seq uint64
	}{{pubKey(2), 2}, {pubKey(9), 3}, {pubKey(1), 0}} {
		if err := s.UpdatePin(ctx, c.pub, c.seq); !errors.Is(err, ErrPinRegression) {
			t.Fatalf("UpdatePin(%d) = %v, want ErrPinRegression", c.seq, err)
		}
	}
	got, err := s.LoadEnrollment(ctx)
	if err != nil || !bytes.Equal(got.HubPub, pubKey(3)) || got.PinSeq != 3 || got.WorkerID != "w" {
		t.Fatalf("after refused updates = %+v, %v", got, err)
	}
}

func TestUpdateWelcomeKeepsThePin(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	if err := s.UpdateWelcome(ctx, WelcomeInfo{WorkerID: "x"}); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("UpdateWelcome without enrollment = %v", err)
	}
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(4), PinSeq: 2}); err != nil {
		t.Fatal(err)
	}
	w := WelcomeInfo{WorkerID: "w-9", Name: "box-2", Owner: "bob", Kind: "infrastructure", PreviewOnly: true}
	if err := s.UpdateWelcome(ctx, w); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkerID != "w-9" || got.Name != "box-2" || got.Owner != "bob" || got.Kind != "infrastructure" || !got.PreviewOnly {
		t.Fatalf("welcome data = %+v", got)
	}
	if !bytes.Equal(got.HubPub, pubKey(4)) || got.PinSeq != 2 {
		t.Fatalf("the pin changed: %+v", got)
	}
}

func TestWipeEnrollmentKeepsEverythingElse(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	if ok, err := s.WipeEnrollment(ctx); err != nil || ok {
		t.Fatalf("wipe on an empty database = %v, %v", ok, err)
	}
	if err := s.SaveShare(ShareSelection{Mode: ShareSelect, Sessions: []string{"work"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NextSessionNumber(); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEnrollment(ctx, Enrollment{HubPub: pubKey(1)}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.WipeEnrollment(ctx); err != nil || !ok {
		t.Fatalf("wipe = %v, %v", ok, err)
	}
	if _, err := s.LoadEnrollment(ctx); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("after wipe: %v", err)
	}
	sel, err := s.LoadShare()
	if err != nil || sel.Mode != ShareSelect || len(sel.Sessions) != 1 {
		t.Fatalf("share selection after wipe = %+v, %v", sel, err)
	}
	if n, err := s.NextSessionNumber(); err != nil || n != 2 {
		t.Fatalf("counter after wipe = %d, %v", n, err)
	}
}
