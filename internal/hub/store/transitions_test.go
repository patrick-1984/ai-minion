package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

func link(seq uint64, oldKey, newKey byte) TransitionRow {
	return TransitionRow{Seq: seq, OldPub: pub(oldKey), NewPub: pub(newKey), CreatedAt: int64(seq) * 1000, SigByOld: bytes.Repeat([]byte{byte(seq)}, 64)}
}

// rotate applies link l on top of the recorded identity.
func rotate(t *testing.T, s *Store, l TransitionRow, audit *AuditRow) error {
	t.Helper()
	return s.RotateHubIdentity(context.Background(), RotateParams{
		Expect: l.OldPub, Transition: l,
		Identity: HubIdentityRow{Pub: l.NewPub, Fingerprint: "fp", CreatedAt: l.CreatedAt},
		Audit:    audit,
	})
}

func seedIdentity(t *testing.T, s *Store, key byte) {
	t.Helper()
	if err := s.PutHubIdentity(context.Background(), HubIdentityRow{Pub: pub(key), Fingerprint: "fp0", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestRotationAppendsLinksAndReplacesTheIdentityInOrder(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedIdentity(t, s, 1)
	if chain, err := s.Transitions(ctx); err != nil || len(chain) != 0 {
		t.Fatalf("empty chain = %v, %v", chain, err)
	}
	if _, ok, err := s.LastTransition(ctx); err != nil || ok {
		t.Fatalf("LastTransition on an empty chain: ok=%v err=%v", ok, err)
	}
	for _, l := range []TransitionRow{link(1, 1, 2), link(2, 2, 3), link(3, 3, 4)} {
		if err := rotate(t, s, l, nil); err != nil {
			t.Fatal(err)
		}
	}
	chain, err := s.Transitions(ctx)
	if err != nil || len(chain) != 3 {
		t.Fatalf("chain = %v, %v", chain, err)
	}
	for i, l := range chain {
		want := link(uint64(i+1), byte(i+1), byte(i+2))
		if l.Seq != want.Seq || !bytes.Equal(l.OldPub, want.OldPub) || !bytes.Equal(l.NewPub, want.NewPub) ||
			l.CreatedAt != want.CreatedAt || !bytes.Equal(l.SigByOld, want.SigByOld) {
			t.Errorf("link %d = %+v, want %+v", i, l, want)
		}
	}
	if last, ok, err := s.LastTransition(ctx); err != nil || !ok || last.Seq != 3 {
		t.Errorf("last = %+v %v %v", last, ok, err)
	}
	if id, _, _ := s.HubIdentity(ctx); !bytes.Equal(id.Pub, pub(4)) {
		t.Error("the recorded identity is not the newest key")
	}
}

func TestTransitionsFromSlicesTheChainAtThePinnedKey(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedIdentity(t, s, 1)
	for _, l := range []TransitionRow{link(1, 1, 2), link(2, 2, 3), link(3, 3, 4)} {
		if err := rotate(t, s, l, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		pin  byte
		want []uint64
	}{{1, []uint64{1, 2, 3}}, {2, []uint64{2, 3}}, {3, []uint64{3}}, {4, nil}, {9, nil}} {
		got, err := s.TransitionsFrom(ctx, pub(tc.pin))
		if err != nil {
			t.Fatal(err)
		}
		var seqs []uint64
		for _, l := range got {
			seqs = append(seqs, l.Seq)
		}
		if len(seqs) != len(tc.want) {
			t.Errorf("pin %d: seqs = %v, want %v", tc.pin, seqs, tc.want)
			continue
		}
		for i := range seqs {
			if seqs[i] != tc.want[i] {
				t.Errorf("pin %d: seqs = %v, want %v", tc.pin, seqs, tc.want)
			}
		}
	}
}

func TestRotationIsAtomicAndRefusesAStaleBase(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedIdentity(t, s, 1)
	audit := &AuditRow{ID: NewID(), TS: 5, Actor: "u1", Action: "hub_identity.rotate", Target: "fp", Outcome: "success"}
	if err := rotate(t, s, link(1, 1, 2), audit); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAudit(ctx, AuditQuery{Action: "hub_identity.rotate", Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Actor != "u1" {
		t.Fatalf("audit rows = %+v, %v", rows, err)
	}

	// A rotation based on the old key loses: the chain and the identity stay.
	if err := rotate(t, s, link(2, 1, 3), nil); !errors.Is(err, ErrRotationConflict) {
		t.Errorf("stale base: err = %v", err)
	}
	// A wrong seq (gap) loses too.
	if err := rotate(t, s, link(3, 2, 3), nil); !errors.Is(err, ErrRotationConflict) {
		t.Errorf("gap: err = %v", err)
	}
	if chain, _ := s.Transitions(ctx); len(chain) != 1 {
		t.Errorf("chain length = %d, want 1", len(chain))
	}
	if id, _, _ := s.HubIdentity(ctx); !bytes.Equal(id.Pub, pub(2)) {
		t.Error("identity changed by a refused rotation")
	}

	// A failing audit row rolls the whole rotation back.
	dup := &AuditRow{ID: audit.ID, TS: 6, Actor: "u1", Action: "hub_identity.rotate", Outcome: "success"}
	if err := rotate(t, s, link(2, 2, 3), dup); err == nil {
		t.Fatal("a duplicate audit id was accepted")
	}
	if chain, _ := s.Transitions(ctx); len(chain) != 1 {
		t.Errorf("link appended although the audit row failed: %d links", len(chain))
	}
	if id, _, _ := s.HubIdentity(ctx); !bytes.Equal(id.Pub, pub(2)) {
		t.Error("identity changed although the audit row failed")
	}
}

func TestRotationWithoutARecordedIdentityOrWithMismatchedKeysIsRefused(t *testing.T) {
	s := openTest(t)
	if err := rotate(t, s, link(1, 1, 2), nil); !errors.Is(err, ErrRotationConflict) {
		t.Errorf("no recorded identity: err = %v", err)
	}
	seedIdentity(t, s, 1)
	bad := RotateParams{Expect: pub(1), Transition: link(1, 1, 2), Identity: HubIdentityRow{Pub: pub(7), Fingerprint: "x"}}
	if err := s.RotateHubIdentity(context.Background(), bad); err == nil || errors.Is(err, ErrRotationConflict) {
		t.Errorf("identity that is not the link's new key: err = %v", err)
	}
	bad = RotateParams{Transition: link(1, 1, 2), Identity: HubIdentityRow{Pub: pub(2)}}
	if err := s.RotateHubIdentity(context.Background(), bad); err == nil {
		t.Error("a rotation without an expected key was accepted")
	}
}

func TestChainRowsRejectLoopsAndShortKeys(t *testing.T) {
	s := openTest(t)
	seedIdentity(t, s, 1)
	if err := rotate(t, s, link(1, 1, 2), nil); err != nil {
		t.Fatal(err)
	}
	// Going back to an earlier key would make a loop: new_pub is UNIQUE.
	if err := rotate(t, s, link(2, 2, 1), nil); err == nil {
		t.Error("a link back to an earlier key was accepted")
	}
	short := link(2, 2, 3)
	short.SigByOld = []byte{1}
	if err := rotate(t, s, short, nil); err == nil {
		t.Error("a short signature was accepted")
	}
}

// The database runs with synchronous=NORMAL, so a plain commit lives only in
// the WAL until a checkpoint. A rotation must return after a full checkpoint:
// the main database file alone, without its WAL, already holds the new link.
func TestRotationIsCheckpointedToTheDatabaseFile(t *testing.T) {
	dir := t.TempDir()
	s, err := openCached(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seedIdentity(t, s, 1)
	if err := rotate(t, s, link(1, 1, 2), &AuditRow{ID: NewID(), TS: 1, Actor: "a", Action: "hub_identity.rotate", Outcome: "success"}); err != nil {
		t.Fatal(err)
	}
	// Copy only the main file, as a power loss would leave it.
	raw, err := os.ReadFile(filepath.Join(dir, DBFile))
	if err != nil {
		t.Fatal(err)
	}
	lone := t.TempDir()
	if err := os.WriteFile(filepath.Join(lone, DBFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(dir, DataKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	// WritePrivate: on Windows a key written with the inherited ACL of the temporary
	// directory is not a private file and Open refuses it.
	if err := identity.WritePrivate(filepath.Join(lone, DataKeyFile), key); err != nil {
		t.Fatal(err)
	}
	copied, err := openCached(t, lone)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copied.Close() }()
	chain, err := copied.Transitions(context.Background())
	if err != nil || len(chain) != 1 {
		t.Fatalf("the main database file alone holds %d links (%v); the commit was not checkpointed", len(chain), err)
	}
	if id, ok, _ := copied.HubIdentity(context.Background()); !ok || !bytes.Equal(id.Pub, pub(2)) {
		t.Error("the main database file alone does not hold the new identity")
	}
}

func TestMakeDurableConfirmsAndReportsWhatItCannotProve(t *testing.T) {
	s, err := openCached(t, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.MakeDurable(context.Background()); err != nil {
		t.Fatalf("MakeDurable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.MakeDurable(ctx); !errors.Is(err, ErrCommitNotDurable) {
		t.Fatalf("MakeDurable on a canceled context = %v; want ErrCommitNotDurable", err)
	}
}
