package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestKeepUnkeepSaveAndRename(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	if err := s.KeepSession(ctx, "work", "t1-a"); err != nil {
		t.Fatal(err)
	}
	if saved, err := s.SaveKeptLayout(ctx, "work", "", `{"windows":[]}`); err != nil || !saved {
		t.Fatalf("save: %v %v", saved, err)
	}
	// Keeping again keeps the layout; an empty instance id keeps the old one.
	if err := s.KeepSession(ctx, "work", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.KeptSessions(ctx)
	if err != nil || len(got) != 1 || got[0].Name != "work" || got[0].InstanceID != "t1-a" || got[0].Layout != `{"windows":[]}` || got[0].Saved.IsZero() {
		t.Fatalf("kept = %+v, %v", got, err)
	}
	// A layout of a session that is not kept is not added.
	if saved, err := s.SaveKeptLayout(ctx, "other", "", `{}`); err != nil || saved {
		t.Fatalf("save of an unkept session: %v %v", saved, err)
	}
	// Rename follows the session; it never overwrites a kept target.
	if err := s.KeepSession(ctx, "taken", ""); err != nil {
		t.Fatal(err)
	}
	if moved, err := s.RenameKeptSession(ctx, "work", "taken"); err != nil || moved {
		t.Fatalf("rename onto a kept name: %v %v", moved, err)
	}
	if moved, err := s.RenameKeptSession(ctx, "work", "renamed"); err != nil || !moved {
		t.Fatalf("rename: %v %v", moved, err)
	}
	names, err := s.KeptNames(ctx)
	if err != nil || !names["renamed"] || !names["taken"] || names["work"] || len(names) != 2 {
		t.Fatalf("names = %v, %v", names, err)
	}
	if removed, err := s.UnkeepSession(ctx, "renamed"); err != nil || !removed {
		t.Fatalf("unkeep: %v %v", removed, err)
	}
	if removed, err := s.UnkeepSession(ctx, "renamed"); err != nil || removed {
		t.Fatalf("unkeep again: %v %v", removed, err)
	}
	if _, err := s.SaveKeptLayout(ctx, "taken", "", string(make([]byte, MaxKeptLayout+1))); err == nil {
		t.Fatal("an oversized layout was saved")
	}
}

func TestKeepListIsBounded(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	for i := range MaxKeptSessions {
		if err := s.KeepSession(ctx, fmt.Sprintf("s%d", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.KeepSession(ctx, "one-more", ""); !errors.Is(err, ErrKeepListFull) {
		t.Fatalf("err = %v, want ErrKeepListFull", err)
	}
	if err := s.KeepSession(ctx, "s0", "t1-b"); err != nil {
		t.Fatalf("a kept session can be kept again on a full list: %v", err)
	}
}

// kept_sessions is created outside the numbered migrations (like
// op_result_callers): a database of the release before gets it without a
// schema change and without a pre-migration backup, so a self-update rollback
// restores nothing and keeps the keep list, and the older binary (which does
// not know the table) opens the database as before.
func TestKeptSessionsNeedNoMigrationSoARollbackKeepsThem(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	prev := openStore(t, dir)
	var before int
	if err := prev.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&before); err != nil {
		t.Skipf("no schema_migrations table to compare: %v", err)
	}
	// The database as the release before left it: no keep table.
	if _, err := prev.db.ExecContext(ctx, `DROP TABLE kept_sessions`); err != nil {
		t.Fatal(err)
	}
	if err := prev.Close(); err != nil {
		t.Fatal(err)
	}
	s := openStore(t, dir)
	if entries, err := os.ReadDir(filepath.Join(dir, backupDirName)); err == nil && len(entries) > 0 {
		t.Fatalf("adding the keep table wrote a pre-migration backup %v", entries)
	}
	if err := s.KeepSession(ctx, "work", ""); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&after); err != nil || after != before {
		t.Fatalf("schema migrations %d -> %d (%v): the keep table must not be a numbered migration", before, after, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again := openStore(t, dir)
	if names, err := again.KeptNames(ctx); err != nil || !names["work"] {
		t.Fatalf("after reopen: %v, %v", names, err)
	}
}
