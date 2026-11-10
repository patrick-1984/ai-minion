package workers

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func textLogger(w *syncBuf) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestFirstStartCreatesTheIdentityKeyAndLogsTheFingerprint(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	var logs syncBuf
	id, err := LoadOrCreateIdentity(context.Background(), dir, st, textLogger(&logs))
	if err != nil {
		t.Fatal(err)
	}
	if !id.Created() {
		t.Error("Created() = false on the first start")
	}
	fp := id.Fingerprint().String()
	if len(fp) != 52 {
		t.Fatalf("fingerprint %q has %d characters", fp, len(fp))
	}
	if !strings.Contains(logs.String(), fp) {
		t.Errorf("the fingerprint was not logged:\n%s", logs.String())
	}
	path := filepath.Join(dir, IdentityFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("identity.key mode = %v, want 0600", fi.Mode().Perm())
	}
	row, ok, err := st.HubIdentity(context.Background())
	if err != nil || !ok || row.Fingerprint != fp || !bytes.Equal(row.Pub, id.Public()) {
		t.Errorf("recorded identity = %+v ok=%v err=%v", row, ok, err)
	}
}

func TestIdentityNeverChangesOnRestart(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	first, err := LoadOrCreateIdentity(context.Background(), dir, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		// A restart reopens the database and loads the file again.
		st2 := openStore(t, dir)
		again, err := LoadOrCreateIdentity(context.Background(), dir, st2, nil)
		if err != nil {
			t.Fatal(err)
		}
		if again.Created() {
			t.Error("a restart created a new identity")
		}
		if !bytes.Equal(again.Public(), first.Public()) || again.Fingerprint() != first.Fingerprint() {
			t.Fatalf("restart %d changed the identity: %s -> %s", i, first.Fingerprint(), again.Fingerprint())
		}
		_ = st2.Close()
	}
}

func TestMissingKeyFileWithRecordedIdentityIsRefused(t *testing.T) {
	// A restore of the database without identity.key (or the reverse) must not
	// produce a new identity that strands every enrolled Worker.
	dir := t.TempDir()
	st := openStore(t, dir)
	first, err := LoadOrCreateIdentity(context.Background(), dir, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, IdentityFile)); err != nil {
		t.Fatal(err)
	}
	_, err = LoadOrCreateIdentity(context.Background(), dir, st, nil)
	if err == nil || !strings.Contains(err.Error(), first.Fingerprint().String()) || !strings.Contains(err.Error(), "restore") {
		t.Fatalf("err = %v, want a refusal that names the recorded fingerprint and says to restore", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, IdentityFile)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Error("a key file was generated despite the refusal")
	}
}

func TestDifferentKeyFileThanRecordedIsRefused(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	first, err := LoadOrCreateIdentity(context.Background(), dir, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	otherStore := openStore(t, other)
	second, err := LoadOrCreateIdentity(context.Background(), other, otherStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Put the other Hub's key file into this data dir.
	raw, err := os.ReadFile(filepath.Join(other, IdentityFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, IdentityFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadOrCreateIdentity(context.Background(), dir, st, nil)
	if err == nil || !strings.Contains(err.Error(), first.Fingerprint().String()) || !strings.Contains(err.Error(), second.Fingerprint().String()) {
		t.Fatalf("err = %v", err)
	}
}

func TestKeyFileWithoutRecordIsAdopted(t *testing.T) {
	dir := t.TempDir()
	st := openStore(t, dir)
	first, err := LoadOrCreateIdentity(context.Background(), dir, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A database without the row (restored from before the first start of
	// this identity, or an interrupted first start).
	if _, err := st.DB().Exec(`DELETE FROM hub_identity`); err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreateIdentity(context.Background(), dir, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Created() || again.Fingerprint() != first.Fingerprint() {
		t.Error("the existing key was not kept")
	}
	if row, ok, _ := st.HubIdentity(context.Background()); !ok || row.Fingerprint != first.Fingerprint().String() {
		t.Error("the identity was not recorded again")
	}
}

func TestLooseKeyFilePermissionsAreRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := t.TempDir()
	st := openStore(t, dir)
	if _, err := LoadOrCreateIdentity(context.Background(), dir, st, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, IdentityFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(context.Background(), dir, st, nil); err == nil {
		t.Fatal("a world-readable identity key was accepted")
	}
}

func TestReadIdentityDoesNotCreate(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadIdentity(dir); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "minion hub serve") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, IdentityFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("ReadIdentity created a key file")
	}
	st := openStore(t, dir)
	created, err := LoadOrCreateIdentity(context.Background(), dir, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadIdentity(dir)
	if err != nil || read.Fingerprint() != created.Fingerprint() {
		t.Fatalf("read = %v err=%v", read, err)
	}
}

// TestConcurrentFirstStartsKeepOneIdentity: several initializers of the same
// fresh data directory (two Hub processes started together) end with the same
// identity, and the key file always holds the key that the database records.
func TestConcurrentFirstStartsKeepOneIdentity(t *testing.T) {
	for round := 0; round < 20; round++ {
		dir := t.TempDir()
		st := openStore(t, dir)
		const n = 6
		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			fps   [n]string
			errs  [n]error
			made  [n]bool
		)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				id, err := LoadOrCreateIdentity(context.Background(), dir, st, textLogger(&syncBuf{}))
				if err != nil {
					errs[i] = err
					return
				}
				fps[i], made[i] = id.Fingerprint().String(), id.Created()
			}()
		}
		close(start)
		wg.Wait()
		creators := 0
		for i := 0; i < n; i++ {
			if errs[i] != nil {
				t.Fatalf("round %d initializer %d: %v", round, i, errs[i])
			}
			if fps[i] != fps[0] {
				t.Fatalf("round %d: initializers disagree: %v", round, fps)
			}
			if made[i] {
				creators++
			}
		}
		if creators != 1 {
			t.Fatalf("round %d: %d initializers report that they created the key", round, creators)
		}
		row, ok, err := st.HubIdentity(context.Background())
		if err != nil || !ok || row.Fingerprint != fps[0] {
			t.Fatalf("round %d: recorded identity = %+v (%v), want %s", round, row, err, fps[0])
		}
		onDisk, err := ReadIdentity(dir)
		if err != nil || onDisk.Fingerprint().String() != fps[0] {
			t.Fatalf("round %d: the key file does not hold the recorded identity (%v)", round, err)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, ".identity.key.init-*")); len(left) != 0 {
			t.Errorf("temporary key files left behind: %v", left)
		}
	}
}
