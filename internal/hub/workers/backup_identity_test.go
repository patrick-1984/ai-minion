package workers

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/identity"
)

// restoreAndStart restores archive into a new data directory and starts the
// identity there, as a restored Hub does. It returns the identity it loaded.
func restoreAndStart(t *testing.T, archive string) *Identity {
	t.Helper()
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "restored")
	if _, err := store.Restore(ctx, archive, target, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st, err := store.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, err := LoadOrCreateIdentity(ctx, target, st, nil)
	if err != nil {
		t.Fatalf("the restored Hub does not start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, PendingKeyFile)); err == nil {
		t.Error("a restore produced a pending key file")
	}
	return id
}

// A rotation that finishes between the database snapshot and the capture of
// the key must not give an old database with the new private key.
func TestBackupDuringARotationRestoresAHubThatStarts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := openStore(t, dir)
	svc, err := NewService(ctx, ServiceOptions{DataDir: dir, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	old := bytes.Clone(svc.Identity().Public())

	snapshots := 0
	hook := func() {
		snapshots++
		if snapshots == 1 {
			// The snapshot holds the old identity; now the rotation finishes.
			if _, err := svc.Rotator().Rotate(ctx, RotateActor{ID: "a"}); err != nil {
				t.Error(err)
			}
		}
	}
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := store.Backup(ctx, dir, out, "dev", store.WithBackupSnapshotHook(hook)); err != nil {
		t.Fatal(err)
	}
	if snapshots < 2 {
		t.Fatalf("the snapshot was taken %d times; a mismatching key must trigger another snapshot", snapshots)
	}
	got := restoreAndStart(t, out)
	if bytes.Equal(got.Public(), old) || !bytes.Equal(got.Public(), svc.Identity().Public()) {
		t.Errorf("the restored Hub has fingerprint %s, want the rotated %s", got.Fingerprint(), svc.Identity().Fingerprint())
	}
}

// A committed rotation whose key is still identity.key.next: the snapshot
// already records the new key, so the backup must carry that key.
func TestBackupDuringAPendingRotationCarriesTheNewKey(t *testing.T) {
	u := newUnflushed(t)
	u.fs.failFlush.Store(true)
	if _, err := u.svc.Rotator().Rotate(context.Background(), RotateActor{ID: "a"}); !errors.Is(err, ErrRotationNotDurable) {
		t.Fatalf("err = %v", err)
	}
	pending, err := identity.LoadKey(filepath.Join(u.dir, PendingKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if _, err := store.Backup(context.Background(), u.dir, out, "dev"); err != nil {
		t.Fatal(err)
	}
	got := restoreAndStart(t, out)
	if !bytes.Equal(got.Public(), pending.Public()) {
		t.Errorf("the restored Hub has fingerprint %s, want the pending %s", got.Fingerprint(), pending.Fingerprint())
	}
	// The live data directory is untouched by the backup.
	if !bytes.Equal(u.identityKeyPub(t), u.oldPub) {
		t.Error("the backup changed identity.key")
	}
}

// With a database that records an identity and no key that matches it, no
// archive is published.
func TestBackupRefusesAKeyThatDoesNotMatchTheDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := openStore(t, dir)
	svc, err := NewService(ctx, ServiceOptions{DataDir: dir, Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if err := identity.SaveKey(filepath.Join(dir, IdentityFile), newKey(t)); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	_, err = store.Backup(ctx, dir, out, "dev")
	if !errors.Is(err, store.ErrBackup) {
		t.Fatalf("err = %v; want ErrBackup", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("an archive was published")
	}
	if entries, _ := os.ReadDir(filepath.Dir(out)); len(entries) != 0 {
		t.Errorf("files left behind: %v", entries)
	}
}
