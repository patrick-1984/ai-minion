package store

import (
	"errors"
	"testing"
)

// The apps_install flag (migration 0044): off on every new or existing row,
// only on a verified Control grant, cleared by unbinding and by a lower
// level; on an acceptance kept by a put without it, cleared by a revoke, and
// it alone makes the row a flag row of FileAcceptances.
func TestAppsInstallFlag(t *testing.T) {
	f := newGrantFixture(t)
	ctx := t.Context()
	claimed := f.ownerGrant("boss")
	claimed.Level, claimed.AppsInstall = 3, true
	if _, err := f.s.PutSessionGrant(ctx, claimed, 0, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("apps_install on a claimed grant: %v", err)
	}
	row := f.ownerGrant("boss")
	row.Level = 3
	g := f.put(t, row)
	if g.AppsInstall {
		t.Fatalf("a new grant has apps_install: %+v", g)
	}
	g, err := f.s.BindSessionGrant(ctx, g.ID, "inc-1", g.Revision, GrantWrite{ActorID: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if g.AppsInstall {
		t.Fatal("binding set apps_install")
	}
	g.AppsInstall, g.UpdatedAt = true, 0
	if g, err = f.s.PutSessionGrant(ctx, g, g.Revision, GrantWrite{ActorID: "alice"}); err != nil || !g.AppsInstall || g.FilesPush {
		t.Fatalf("apps_install on a verified Control grant = %+v, %v", g, err)
	}
	low := g
	low.Level, low.UpdatedAt = 2, 0
	if _, err := f.s.PutSessionGrant(ctx, low, g.Revision, GrantWrite{ActorID: "alice"}); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("apps_install at level 2: %v", err)
	}
	if g, err = f.s.BindSessionGrant(ctx, g.ID, "", g.Revision, GrantWrite{ActorID: "alice"}); err != nil || g.AppsInstall {
		t.Fatalf("unbound = %+v, %v", g, err)
	}

	base := AcceptRow{TargetWorkerID: f.b.ID, SourceWorkerID: f.a.ID, CreatedBy: "alice", CreatedAt: 1}
	if err := f.s.PutAcceptIfOwners(ctx, base, "alice", "alice"); err != nil {
		t.Fatal(err)
	}
	read := func() AcceptRow {
		t.Helper()
		rows, err := f.s.ListAccepts(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("accepts = %+v, %v", rows, err)
		}
		return rows[0]
	}
	if r := read(); *r.AppsInstall {
		t.Fatalf("new acceptance = %+v", r)
	}
	if fa, err := f.s.FileAcceptances(ctx, f.a.ID); err != nil || len(fa) != 0 {
		t.Fatalf("flag rows without a flag = %+v, %v", fa, err)
	}
	with := base
	with.AppsInstall = boolPtr(true)
	if err := f.s.PutAcceptIfOwners(ctx, with, "alice", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutAcceptIfOwners(ctx, base, "alice", "alice"); err != nil {
		t.Fatal(err)
	}
	if r := read(); !*r.AppsInstall || *r.FilesPush || *r.FilesPull {
		t.Fatalf("kept = %+v", r)
	}
	if fa, err := f.s.FileAcceptances(ctx, f.a.ID); err != nil || len(fa) != 1 || !*fa[0].AppsInstall {
		t.Fatalf("flag rows = %+v, %v", fa, err)
	}
}
