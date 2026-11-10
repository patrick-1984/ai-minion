package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func newEnrollments(t *testing.T) (*Enrollments, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewEnrollments(st, dir), st, dir
}

func TestWorkerKeyIsCreatedOnceAndPersisted(t *testing.T) {
	e, _, dir := newEnrollments(t)
	if _, err := e.LoadKey(); !errors.Is(err, ErrNoWorkerKey) {
		t.Fatalf("LoadKey before creation = %v, want ErrNoWorkerKey", err)
	}
	k1, err := e.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if e.KeyPath() != filepath.Join(dir, WorkerKeyFile) {
		t.Fatalf("KeyPath = %q", e.KeyPath())
	}
	k2, err := e.LoadOrCreateKey()
	if err != nil || !bytes.Equal(k1.Public(), k2.Public()) {
		t.Fatalf("the second call must return the same key: %v", err)
	}
	// A new process (a new Enrollments over the same directory) sees it too.
	k3, err := NewEnrollments(nil, dir).LoadKey()
	if err != nil || !bytes.Equal(k1.Public(), k3.Public()) {
		t.Fatalf("reload: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(e.KeyPath())
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode = %v, %v; want 0600", fi.Mode().Perm(), err)
		}
	}
}

func TestWorkerKeyFileThatOthersCanReadIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	e, _, _ := newEnrollments(t)
	if _, err := e.LoadOrCreateKey(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.KeyPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.LoadKey(); err == nil || errors.Is(err, ErrNoWorkerKey) {
		t.Fatalf("LoadKey of a world-readable key = %v, want a refusal", err)
	}
	if _, err := e.LoadOrCreateKey(); err == nil {
		t.Fatal("LoadOrCreateKey must not replace a key it refuses to read")
	}
}

func TestEnrollmentCommitLoadRewriteWipe(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newEnrollments(t)
	if _, ok, err := e.Load(ctx); err != nil || ok {
		t.Fatalf("a new Worker is not enrolled: ok %v, err %v", ok, err)
	}
	hub, _ := identity.GenerateKey()
	in := Enrollment{
		Pin: identity.Pin{Pub: hub.Public(), Seq: 0}, WorkerID: "w-1", Name: "pi", Owner: "alice", Kind: "private",
		Policy: HubPolicy{PreviewOnly: true},
	}
	if err := e.Commit(ctx, in); err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.Changed():
	default:
		t.Fatal("Commit must wake an idle connector")
	}
	got, ok, err := e.Load(ctx)
	if err != nil || !ok {
		t.Fatalf("Load = %v, %v", ok, err)
	}
	if !bytes.Equal(got.Pin.Pub, hub.Public()) || got.WorkerID != "w-1" || got.Owner != "alice" || !got.Policy.PreviewOnly || got.EnrolledAt.IsZero() {
		t.Fatalf("loaded %+v", got)
	}

	next, _ := identity.GenerateKey()
	if err := e.RewritePin(ctx, identity.Pin{Pub: next.Public(), Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.RewritePin(ctx, identity.Pin{Pub: hub.Public(), Seq: 0}); !errors.Is(err, store.ErrPinRegression) {
		t.Fatalf("a pin that goes back = %v, want ErrPinRegression", err)
	}
	if err := e.ApplyWelcome(ctx, HubWelcome{WorkerID: "w-1", Name: "pi-2", Owner: "bob", Kind: "infrastructure"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = e.Load(ctx)
	if got.Name != "pi-2" || got.Owner != "bob" || got.Policy.PreviewOnly || got.Pin.Seq != 1 || !bytes.Equal(got.Pin.Pub, next.Public()) {
		t.Fatalf("after rewrite and welcome: %+v", got)
	}

	if wiped, err := e.Wipe(ctx); err != nil || !wiped {
		t.Fatalf("Wipe = %v, %v", wiped, err)
	}
	if _, ok, _ := e.Load(ctx); ok {
		t.Fatal("still enrolled after Wipe")
	}
	if wiped, err := e.Wipe(ctx); err != nil || wiped {
		t.Fatalf("a second Wipe = %v, %v; want a quiet no-op", wiped, err)
	}
}

func TestWipeKeepsTheKeyPair(t *testing.T) {
	ctx := context.Background()
	e, _, _ := newEnrollments(t)
	k, err := e.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	hub, _ := identity.GenerateKey()
	if err := e.Commit(ctx, Enrollment{Pin: identity.Pin{Pub: hub.Public()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wipe(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := e.LoadKey()
	if err != nil || !bytes.Equal(again.Public(), k.Public()) {
		t.Fatalf("Wipe must keep the key pair: %v", err)
	}
}

func TestEnrollmentSurvivesAReopenOfTheState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hub, _ := identity.GenerateKey()
	if err := NewEnrollments(st, dir).Commit(ctx, Enrollment{Pin: identity.Pin{Pub: hub.Public(), Seq: 4}, WorkerID: "w"}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, ok, err := NewEnrollments(st, dir).Load(ctx)
	if err != nil || !ok || got.Pin.Seq != 4 || got.WorkerID != "w" {
		t.Fatalf("after reopen: %+v, %v, %v", got, ok, err)
	}
}
