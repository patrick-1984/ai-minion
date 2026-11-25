package store

import (
	"errors"
	"testing"
)

func TestBindSessionGrantRevisionUniqueAndNameOnlyRead(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	g := f.put(t, f.ownerGrant("boss"))
	if _, err := f.s.BindSessionGrant(ctx, g.ID, "inc-1", g.Revision+1); !errors.Is(err, ErrGrantRevision) {
		t.Fatalf("stale revision = %v", err)
	}
	bound, err := f.s.BindSessionGrant(ctx, g.ID, "inc-1", g.Revision)
	if err != nil || bound.SourceIncarnation != "inc-1" || bound.Revision != g.Revision+1 {
		t.Fatalf("bind = %+v, %v", bound, err)
	}
	// The name-only read never returns a verified grant.
	if rows, err := f.s.SessionGrantsFor(ctx, f.a.ID, "boss"); err != nil || len(rows) != 0 {
		t.Fatalf("name-only read = %+v, %v", rows, err)
	}
	src, err := f.s.VerifiedGrantSources(ctx, f.a.ID, "alice", f.a.Pub)
	if err != nil || len(src) != 1 || src[0] != (VerifiedGrantSource{SessionName: "boss", IncarnationID: "inc-1"}) {
		t.Fatalf("sources = %+v, %v", src, err)
	}
	if src, err := f.s.VerifiedGrantSources(ctx, f.a.ID, "bob", f.a.Pub); err != nil || len(src) != 0 {
		t.Fatalf("sources for another owner = %+v, %v", src, err)
	}
	// A claimed grant of the same pair may exist beside the verified one, and
	// then the verified one cannot be unbound onto it.
	f.put(t, f.ownerGrant("boss"))
	if _, err := f.s.BindSessionGrant(ctx, g.ID, "", bound.Revision); !errors.Is(err, ErrGrantExists) {
		t.Fatalf("unbind onto an existing claimed grant = %v", err)
	}
	// A Worker that is not owned as the row says pins the bind.
	f.exec(t, `UPDATE session_grant SET source_owner_id='bob', authority='admin' WHERE id=?`, g.ID)
	cur, _ := f.s.SessionGrant(ctx, g.ID)
	if _, err := f.s.BindSessionGrant(ctx, g.ID, "inc-2", cur.Revision); !errors.Is(err, ErrGrantOwners) {
		t.Fatalf("owner pin = %v", err)
	}
	if _, err := f.s.BindSessionGrant(ctx, "missing", "x", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing grant = %v", err)
	}
}
