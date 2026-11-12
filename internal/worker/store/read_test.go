package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenReadOnlyDoesNotCreateState(t *testing.T) {
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		if st, err := OpenReadOnlyContext(context.Background(), dir); err == nil {
			_ = st.Close()
			t.Fatal("read-only open created a database")
		}
		if _, err := os.Stat(filepath.Join(dir, DBFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("database was created: %v", err)
		}
	}
}

func TestOpenReadOnlyReadsWhileWorkerWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	worker := openStore(t, dir)
	if err := worker.SaveShare(ShareSelection{Mode: ShareAll}); err != nil {
		t.Fatal(err)
	}
	tx, err := worker.DB().BeginTx(ctx, nil) // holds the WAL writer lock
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("UPDATE share_config SET mode = 'none'"); err != nil {
		t.Fatal(err)
	}
	status, err := OpenReadOnlyContext(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer status.Close()
	share, err := status.LoadShareContext(ctx)
	if err != nil || share.Mode != ShareAll {
		t.Fatalf("status must read committed state while a writer is active: %+v, %v", share, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Reserve two connections so the checks cover pool initialization too.
	for range 2 {
		conn, err := status.DB().Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var queryOnly int
		if err := conn.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
			t.Fatalf("query_only = %d, %v", queryOnly, err)
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA query_only=OFF"); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM share_config"); err == nil {
			t.Fatal("mode=ro allowed a write after query_only was disabled")
		}
	}
}
